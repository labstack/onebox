package engine

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

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
	ids        []string
	health     string            // proxy container health, parsed from docker ps .Status
	discovery  bool              // isolated Docker discovery controller is running
	applied    string            // config and Compose identity the host applied
	owner      string            // sole application identity from the host owner record
	ownerEnv   string            // environment identity from the owner record
	acme       []acmeStore       // raw ACME stores; parsed at render, and keys never leave
	certStores map[string]string // resolver → ACME store file, from the applied static config; nil when unknown
	localHash  string            // locally staged config and Compose identity (computed offline)
	// Why a read could not be trusted, when it could not. Recorded rather
	// than raised: gather returns on the first error and Status renders
	// nothing after it, so raising costs the operator every other fact about
	// the host at exactly the moment it is in a strange state.
	ownerIssue   string
	appliedIssue string
	acmeIssue    string
	// certConfigIssue: the applied static configuration could not be read or
	// parsed, so certificates are judged by expiry alone.
	certConfigIssue string
}

func (px proxyRaw) ownershipIssue(application, environment string) string {
	if px.owner != "" && px.owner != application {
		return fmt.Sprintf("host is owned by application %s", px.owner)
	}
	if px.ownerEnv != "" && px.ownerEnv != environment {
		return fmt.Sprintf(
			"host is claimed by the %s environment of application %s; status targets %s",
			px.ownerEnv, px.owner, environment,
		)
	}
	return ""
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
			discoveryIDs, err := e.discoveryContainerIDs(ctx)
			if err != nil {
				return err
			}
			px.discovery = len(discoveryIDs) > 0
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
				record := strings.TrimSpace(res.Stdout)
				// Use the same parser as preflight and every mutation. Owner
				// records written before environments contain one field; current
				// records contain application and environment as two fields.
				// Applying the application-name regex to the whole record made
				// status reject the current format while mutations accepted it.
				switch record {
				case "":
					px.ownerIssue = "host owner record is present but empty; an empty record is not a valid claim"
				default:
					owner, ok := app.ParseHostOwnerRecord(record)
					if !ok {
						px.ownerIssue = "the host owner record is not a valid application/environment claim; " +
							"every mutation will refuse this host"
						return nil
					}
					px.owner = owner.Application
					px.ownerEnv = owner.Environment
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
			// The static configuration the host applied says which resolver
			// writes which store, and so which stores to read. It is read
			// first, in the same step as the stores it names.
			if err := e.readCertificateConfig(ctx, hp, px); err != nil {
				return err
			}
			for _, name := range certificateStoreFiles(px.certStores) {
				path := hp.Acme + "/" + name
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
				px.acme = append(px.acme, acmeStore{file: name, path: path, body: res.Stdout})
			}
			return nil
		},
		func() error {
			staging, err := os.MkdirTemp("", "onebox-proxy-status")
			if err != nil {
				return err
			}
			defer os.RemoveAll(staging)
			id, err := e.stageProxy(staging)
			px.localHash = id.String()
			return err
		},
	}
}

// readCertificateConfig reads the static configuration the host applied and
// records which store each of its resolvers writes. The host's file is the
// one that matters: the running proxy renews by it, and a local change that
// was never applied renews nothing yet. A failed read or a configuration that
// does not parse is recorded, not raised, and leaves certStores nil so the
// certificates are still judged by expiry. Only a transport error is raised. No file at all is a host that
// never applied a proxy, which has no certificates to judge.
func (e *Engine) readCertificateConfig(ctx context.Context, hp proxy.Paths, px *proxyRaw) error {
	for _, name := range proxy.StaticConfigNames {
		path := hp.ConfigDir + "/" + name
		res, err := e.T.Run(ctx, readableFileProbe(path))
		if err != nil {
			return err
		}
		if issue, refused := statusFileIssue("the applied static configuration", path, res); refused {
			px.certConfigIssue = issue
			return nil
		}
		// Any other failure, such as cat losing a race with ob proxy apply
		// swapping the directory, is recorded too: the stores this step reads
		// next matter more than the configuration that refines them.
		if res.ExitCode != 0 {
			px.certConfigIssue = fmt.Sprintf("the applied static configuration at %s could not be read (exit %d); certificates are judged by expiry alone", path, res.ExitCode)
			return nil
		}
		if strings.TrimSpace(res.Stdout) == "" {
			continue
		}
		// The parse error is not quoted: it can repeat values from the file,
		// and the message reaches the terminal and the JSON snapshot.
		stores, err := proxy.CertificateStores([]byte(res.Stdout))
		if err != nil {
			px.certConfigIssue = fmt.Sprintf("the applied static configuration at %s is not valid YAML; certificates are judged by expiry alone", path)
			return nil
		}
		px.certStores = stores
		return nil
	}
	return nil
}

// certificateStoreFiles lists the store files to read: the two Onebox has
// always written, so that keys left in them stay visible, plus any other file
// the applied configuration points a resolver at.
func certificateStoreFiles(stores map[string]string) []string {
	files := []string{"acme.json", "acme-wildcard.json"}
	seen := map[string]bool{"acme.json": true, "acme-wildcard.json": true}
	var extra []string
	for _, file := range stores {
		if file != "" && !seen[file] {
			seen[file] = true
			extra = append(extra, file)
		}
	}
	sort.Strings(extra)
	return append(files, extra...)
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
	// Every sentence leads with the component, so a reader — or a consumer
	// matching on the text — can tell which one a message is about without
	// parsing the rest of it. (An unreachable classifier in internal/onebox
	// used to depend on this and was the stated reason for the rule; the rule
	// is worth keeping on its own, which is why the prose has not changed.)
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
		" --filter label=com.docker.compose.service=proxy"+
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
	if !px.discovery {
		e.ui.Println("  discovery controller NOT RUNNING ⚠ — `ob proxy apply`")
		diverged = true
	}
	health := px.health
	if health != "healthy" {
		diverged = true
	}

	local, _ := proxy.ParseIdentity(px.localHash)
	state := fmt.Sprintf("config %s compose %s %s", shortDigest(local.Config), shortDigest(local.Compose), e.ui.OK("(in sync)"))
	if px.appliedIssue != "" {
		// Never DRIFTED on an unread file: that names a remedy (`ob proxy
		// apply`) for a problem it would not fix.
		state = e.ui.Warn("config UNKNOWN ⚠ — " + px.appliedIssue)
		diverged = true
	} else if parts := proxyDriftParts(px.applied, local); len(parts) > 0 {
		state = e.ui.Warn(fmt.Sprintf("DRIFTED ⚠ (%s) — `ob proxy apply`", strings.Join(parts, ", ")))
		diverged = true
	}
	owner := px.owner
	if px.ownerEnv != "" {
		owner += "/" + px.ownerEnv
	}
	if owner == "" {
		owner = "(unclaimed)"
	}
	if px.ownerIssue != "" {
		owner = e.ui.Warn(px.ownerIssue + " ⚠")
		diverged = true
	} else if issue := px.ownershipIssue(e.Spec.Name, e.Opts.Environment); issue != "" {
		owner = e.ui.Warn(issue + " ⚠")
		diverged = true
	}
	fmt.Fprintf(e.Opts.Out, "proxy %-12s %-10s %s   owner: %s\n", proxy.ContainerName, health, state, owner)

	// cert runway — the renewal loop is the proxy's one silent failure mode
	if px.acmeIssue != "" {
		fmt.Fprintf(e.Opts.Out, "  cert store unreadable ⚠ (%s)\n", px.acmeIssue)
		return true, nil
	}
	if px.certConfigIssue != "" {
		fmt.Fprintf(e.Opts.Out, "  cert resolvers unknown ⚠ (%s)\n", px.certConfigIssue)
		diverged = true
	}
	certs, err := judgeCertificates(px.acme, px.certStores, e.Opts.Now())
	if err != nil {
		fmt.Fprintf(e.Opts.Out, "  cert store unreadable ⚠ (%v)\n", err)
		return true, nil
	}
	for _, c := range certs {
		mark := ""
		switch {
		case c.Orphaned:
			mark = e.ui.Dim(fmt.Sprintf("  not renewed: the applied configuration has no resolver %q writing this store (remove the key from %s while the proxy is stopped)", c.Resolver, c.Store))
		case c.Overdue:
			mark = e.ui.Warn("  RENEWAL OVERDUE ⚠ (lego renews at 30d — check CF_DNS_API_TOKEN / proxy logs)")
			diverged = true
		}
		e.ui.Println(fmt.Sprintf("  cert %-20s expires %s %s%s", c.Domain, c.NotAfter.UTC().Format("2006-01-02"), e.ui.Dim(fmt.Sprintf("(%dd)", c.Days)), mark))
	}
	return diverged, nil
}

// acmeStore is one ACME store file as read from the host. The body holds
// private keys, so it is parsed in place and never rendered.
type acmeStore struct {
	file string // file name, as a resolver's acme.storage names it
	path string // where the file lives on the host
	body string
}

// judgedCertificate is one certificate from an ACME store with the verdict
// that both status views report.
type judgedCertificate struct {
	proxy.CertExpiry
	Store    string // host path of the store the certificate was read from
	Days     int    // whole days until expiry
	Overdue  bool   // a configured resolver failed to renew it in time
	Orphaned bool   // no configured resolver writes it to this store
}

// judgeCertificates parses every store and judges each certificate by the
// resolver that issued it. A certificate is renewed only when the static
// configuration defines its resolver with this very store as its storage, so
// only such a certificate can be overdue. A key that fails that test was left
// behind by an older proxy configuration (a renamed resolver, wildcard routes
// that were removed, a storage path that moved). Nothing renews it, and
// judging its expiry would report a healthy host as diverged forever, so it is
// reported as orphaned instead and the operator can remove the stale key.
//
// stores maps each resolver of the applied configuration to its store file.
// A nil map means that configuration is unknown. Then no certificate is
// called orphaned, since that would invite deleting one the proxy may still
// renew, and every certificate is judged by its expiry, since hiding a real
// overdue renewal is the worse mistake.
//
// Certificates are sorted, because the store's resolver keys parse from a map
// and both views must print the same order on every run.
func judgeCertificates(raw []acmeStore, stores map[string]string, now time.Time) ([]judgedCertificate, error) {
	var out []judgedCertificate
	for _, store := range raw {
		certs, err := proxy.CertExpiries([]byte(store.body))
		if err != nil {
			return nil, err
		}
		for _, cert := range certs {
			days := int(cert.NotAfter.Sub(now).Hours() / 24)
			// Without the configuration every certificate might be renewed,
			// so each is judged by its expiry, as before resolvers were known.
			// A resolver whose store CertificateStores could not place ("")
			// might write here, so it is given the benefit of the doubt.
			renewed := true
			if stores != nil {
				file, configured := stores[cert.Resolver]
				renewed = configured && (file == "" || file == store.file)
			}
			out = append(out, judgedCertificate{
				CertExpiry: cert,
				Store:      store.path,
				Days:       days,
				Overdue:    renewed && days < renewalFloorDays,
				Orphaned:   !renewed,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Domain != b.Domain {
			return a.Domain < b.Domain
		}
		if a.Resolver != b.Resolver {
			return a.Resolver < b.Resolver
		}
		if a.Store != b.Store {
			return a.Store < b.Store
		}
		return a.NotAfter.Before(b.NotAfter)
	})
	return out, nil
}

func proxyDriftParts(applied string, local proxy.Identity) []string {
	remote, ok := proxy.ParseIdentity(applied)
	if _, valid := proxy.ParseIdentity(local.String()); !ok || !valid {
		return []string{"config", "compose"}
	}
	var parts []string
	if remote.Config != local.Config {
		parts = append(parts, "config")
	}
	if remote.Compose != local.Compose {
		parts = append(parts, "compose")
	}
	return parts
}

func shortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 8 {
		return d[:8]
	}
	return d
}
