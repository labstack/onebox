package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/proxy"
	"github.com/labstack/onebox/internal/transport"
)

// renewalFloor: lego renews 30 days before expiry; a cert inside 21 days
// means renewal has been failing for over a week — an incident, not a warning.
const renewalFloorDays = 21

// proxyRaw is the managed proxy's status reads, gathered concurrently with the
// app-side reads.
type proxyRaw struct {
	ids       []string
	health    string // proxy container health, parsed from docker ps .Status
	applied   string // config hash the host applied
	owner     string // sole application identity from the host owner record
	acme      string // raw acme.json; parsed at render, and keys never leave
	localHash string // hash of the locally staged config (computed offline)
	// Why a read could not be trusted, when it could not. Recorded rather
	// than raised: gather returns on the first error and Status renders
	// nothing after it, so raising costs the operator every other fact about
	// the host at exactly the moment it is in a strange state.
	ownerIssue   string
	appliedIssue string
	acmeIssue    string
}

// proxyReads returns the managed proxy's independent status reads as thunks for
// gather — the host round trips (container+health, config hash, owner, acme) run
// concurrently with the app-side reads, and the local config hash is computed
// in the same wave. Each thunk writes a distinct proxyRaw field, so they share
// no state. Health comes from docker ps .Status, same as the app side.
func (e *Engine) proxyReads(ctx context.Context, px *proxyRaw) []func() error {
	hp := proxy.HostPaths(e.names())
	return []func() error{
		func() error {
			id, health, err := e.proxyContainer(ctx)
			if err != nil {
				return err
			}
			if id != "" {
				px.ids = []string{id}
			}
			px.health = health
			return nil
		},
		func() error {
			res, err := e.T.Run(ctx, readableFileProbe(hp.Hash))
			if err != nil {
				return err
			}
			if issue, refused := statusFileIssue("the applied configuration", hp.Hash, res); refused {
				px.appliedIssue = issue
				return nil
			}
			if res.ExitCode != 0 {
				return statusReadResult("proxy applied configuration", res, nil)
			}
			px.applied = strings.TrimSpace(res.Stdout)
			return nil
		},
		func() error {
			// The same probe every other owner read uses. A -r/-e read
			// follows symlinks, so status would report a symlinked
			// record's target as the owner while every mutation refuses
			// the host — two answers about one file.
			res, err := e.T.Run(ctx, app.HostOwnerProbe(hp.Owner))
			if err != nil {
				return err
			}
			// A bad owner record is reported, not raised: gather returns on
			// the first error and Status renders nothing after it, so
			// raising here would cost the operator container health, config
			// drift and certificate runway at exactly the moment the host is
			// in a strange state. makeStatusProxy marks the read incomplete
			// so the JSON path keeps claiming only what it read.
			switch res.ExitCode {
			case 0:
				// An interrupted claim leaves a zero-byte record: the
				// noclobber write creates the file before writing it.
				// Rendering that as "(unclaimed)" would have status
				// invite a bootstrap that readHostOwner then rejects as
				// invalid — two answers about one file.
				px.owner = strings.TrimSpace(res.Stdout)
				// The same validity rule readHostOwner applies. Publishing a
				// record that every mutation then refuses is the "two answers
				// about one file" this read was rewritten to eliminate: an
				// agent branching on the JSON would read the host as claimed.
				switch {
				case px.owner == "":
					px.ownerIssue = "host owner record is present but empty; an empty record is not a valid claim"
				case !appNameRe.MatchString(px.owner):
					px.ownerIssue = "the host owner record is not a valid application name; every mutation will refuse this host"
					px.owner = ""
				}
				return nil
			case app.ProbeAbsent:
				return nil
			case app.ProbeUnreadable:
				px.ownerIssue = "the host owner record exists but could not be read; verify the record's permissions"
				return nil
			case app.ProbeNotRegular:
				px.ownerIssue = "host owner record is not a regular file; only a regular file is a valid owner record"
				return nil
			case app.ProbeStatePathNotDirectory:
				px.ownerIssue = "the path that should hold the host owner record is not a directory"
				return nil
			case app.ProbeUndetermined:
				px.ownerIssue = "the host state directory cannot be searched, so the owner record could not be read"
				return nil
			}
			// Anything else is still the owner read failing — cat can exit 1
			// when the record is unlinked or replaced between the -r test and
			// the read, or on an I/O error. Raising it here would stop gather
			// and render nothing, which is the outcome this branch records
			// issues to avoid.
			px.ownerIssue = fmt.Sprintf("the host owner record could not be read (exit %d)", res.ExitCode)
			return nil
		},
		func() error {
			path := hp.Acme + "/acme.json"
			res, err := e.T.Run(ctx, readableFileProbe(path))
			if err != nil {
				return err
			}
			if issue, refused := statusFileIssue("the certificate store", path, res); refused {
				px.acmeIssue = issue
				return nil
			}
			if res.ExitCode != 0 {
				return statusReadResult("proxy certificate store", res, nil)
			}
			px.acme = res.Stdout
			return nil
		},
		func() error {
			// Empty stays empty: joining it with the project directory would
			// point at the repository root and ask it to be Traefik's config.
			localCfg := e.Spec.Proxy.Config
			if localCfg != "" && !filepath.IsAbs(localCfg) {
				localCfg = filepath.Join(e.Opts.LocalDir, localCfg)
			}
			staging, err := os.MkdirTemp("", "ob-proxy-status")
			if err != nil {
				return err
			}
			defer os.RemoveAll(staging)
			px.localHash, err = proxy.Stage(localCfg, staging, e.Spec.Proxy.Image, e.Spec.Proxy.Network)
			return err
		},
	}
}

// readableFileProbe reads a status file that may legitimately be absent:
// 0 with the contents on stdout, 2 present but unreadable, 5 absence could not
// be established, 0 and empty when there is nothing there.
//
// -e follows symlinks, so the -L arm is what stops a dangling link scoring as
// absent — status would otherwise call a host with a broken link un-applied
// and report drift against a file it never managed to read. An unsearchable
// ancestor produces the same false drift, which is what the exit-5 arm is for.
func readableFileProbe(path string) string {
	p := q(path)
	return "if [ -r " + p + " ]; then cat " + p +
		"; elif [ -e " + p + " ] || [ -L " + p + " ]; then exit " + strconv.Itoa(app.ProbeUnreadable) + "; else " +
		app.UndeterminedArm(path) + "true; fi"
}

// statusFileIssue names the probe's deliberate refusals. They carry no stderr,
// so reporting the exit code alone would give a bare number for a condition
// that names itself. An empty string means the read is usable.
//
// These are issues rather than errors for the same reason the owner read
// records one: status is what an operator runs to find out what is wrong, and
// one unreadable file should not take the rest of the report with it.
// Only the probe's own refusals become issues. An exit it never raises is the
// command itself failing — a transport or shell problem, not a fact about the
// file — and that stays a failed read so the snapshot reports the component as
// incomplete rather than quietly asserting it was checked.
func statusFileIssue(component, path string, result transport.Result) (string, bool) {
	// Every sentence leads with the component. statusIssueCodes derives a
	// branchable code by matching this prose, and a message that led with the
	// path instead collapsed to the generic <component>_diverged — the same
	// indistinguishability the owner refusal was given a code to avoid.
	switch result.ExitCode {
	case app.ProbeUnreadable:
		return fmt.Sprintf("%s exists but could not be read; verify the file and its permissions", component), true
	case app.ProbeStatePathNotDirectory:
		return fmt.Sprintf("%s could not be read: the path that should hold %s is not a directory", component, path), true
	case app.ProbeUndetermined:
		return fmt.Sprintf("%s could not be read: the directory holding %s cannot be searched, so a missing file cannot be told from an unreadable one", component, path), true
	}
	return "", false
}

func statusReadResult(component string, result transport.Result, err error) error {
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("read %s failed (exit %d): %s", component, result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return nil
}

// proxyContainer returns the managed proxy's id and health from ONE docker ps —
// the proxy is a single container, so status doesn't need the id-then-inspect
// two-step (that was the last serial hop in the read wave). Health is parsed
// from .Status like the app side. Empty id when the proxy isn't running.
func (e *Engine) proxyContainer(ctx context.Context) (id, health string, err error) {
	res, err := e.T.Run(ctx, "docker ps --filter label=com.docker.compose.project="+q(proxy.Project)+
		" --format '{{.ID}}|{{.Status}}'")
	if err != nil {
		return "", "", err
	}
	if res.ExitCode != 0 {
		return "", "", fmt.Errorf("proxy docker ps failed (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	line := strings.SplitN(strings.TrimSpace(res.Stdout), "\n", 2)[0] // single container
	if line == "" {
		return "", "", nil
	}
	id, status, _ := strings.Cut(line, "|")
	if !validID.MatchString(id) {
		return "", "", fmt.Errorf("suspicious proxy container id %q from docker ps", id)
	}
	return id, healthFromStatus(status), nil
}

// renderProxy prints the managed proxy under the same recorded-vs-actual
// contract as roles: recorded = the locally staged config hash, actual = what
// the host applied + the container's health + each cert's runway. Divergence
// (absent, unhealthy, drifted config, overdue renewal) returns true. Pure — it
// issues no round trips; everything was gathered concurrently upfront.
func (e *Engine) renderProxy(px proxyRaw) (bool, error) {
	if len(px.ids) == 0 {
		e.ui.Println(fmt.Sprintf("proxy %-12s %s", proxy.ContainerName, e.ui.Warn("NOT RUNNING ⚠ — `ob proxy apply`")))
		return true, nil
	}
	diverged := false
	health := px.health
	if health != "healthy" {
		diverged = true
	}

	state := fmt.Sprintf("config %.8s %s", px.applied, e.ui.OK("(in sync)"))
	if px.appliedIssue != "" {
		// Never DRIFTED on an unread file: that names a remedy (`ob proxy
		// apply`) for a problem it would not fix.
		state = e.ui.Warn("config UNKNOWN ⚠ — " + px.appliedIssue)
		diverged = true
	} else if px.applied != px.localHash {
		state = e.ui.Warn(fmt.Sprintf("config DRIFTED ⚠ (local %.8s ≠ applied %.8s) — `ob proxy apply`", px.localHash, px.applied))
		diverged = true
	}
	owner := px.owner
	if owner == "" {
		owner = "(unclaimed)"
	}
	if px.ownerIssue != "" {
		owner = e.ui.Warn(px.ownerIssue + " ⚠")
		diverged = true
	}
	fmt.Fprintf(e.Opts.Out, "proxy %-12s %-10s %s   owner: %s\n", proxy.ContainerName, health, state, owner)

	// cert runway — the renewal loop is the proxy's one silent failure mode
	if px.acmeIssue != "" {
		fmt.Fprintf(e.Opts.Out, "  cert store unreadable ⚠ (%s)\n", px.acmeIssue)
		return true, nil
	}
	certs, err := proxy.CertExpiries([]byte(px.acme))
	if err != nil {
		fmt.Fprintf(e.Opts.Out, "  cert store unreadable ⚠ (%v)\n", err)
		return true, nil
	}
	for _, c := range certs {
		days := int(c.NotAfter.Sub(e.Opts.Now()).Hours() / 24)
		mark := ""
		if days < renewalFloorDays {
			mark = e.ui.Warn("  RENEWAL OVERDUE ⚠ (lego renews at 30d — check CF_DNS_API_TOKEN / proxy logs)")
			diverged = true
		}
		e.ui.Println(fmt.Sprintf("  cert %-20s expires %s %s%s", c.Domain, c.NotAfter.UTC().Format("2006-01-02"), e.ui.Dim(fmt.Sprintf("(%dd)", days)), mark))
	}
	return diverged, nil
}
