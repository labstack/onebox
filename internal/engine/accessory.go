package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/pmezard/go-difflib/difflib"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/journal"
)

// ServiceApply converges the supporting services explicitly, as a planned
// maintenance event — never mid-deploy. It shows the diff against what is
// running, refuses destructive mount changes without force, then converges
// each service's own Compose project under the full lock/fence/journal regime.
//
// The diff is against each service's live document rather than against a
// release, because a service is not part of one. Comparing to the release
// would report every service as changed on the first apply after a deploy,
// and report nothing when a Postgres version changed under an untouched app.
func (e *Engine) ServiceApply(ctx context.Context, releaseID string, force bool) (err error) {
	if len(e.Spec.ServiceNames()) == 0 {
		return fmt.Errorf("no services declared")
	}
	n := e.Spec.NamesFor(e.Opts.Environment)
	rendered, err := e.Spec.RenderServices(e.Opts.Environment)
	if err != nil {
		return err
	}

	// A major version change to a service that cannot read the previous
	// version's data directory is refused before anything is replaced. The
	// diff shows one line — `postgres:16` becoming `postgres:17` — which reads
	// as routine and is not: the new container starts, finds a data directory
	// it cannot open, and crash-loops with the database intact and unreachable.
	if err := e.refuseUnsafeMajorUpgrade(ctx, n, force); err != nil {
		return err
	}

	changed := false
	for _, name := range e.Spec.ServiceNames() {
		res, err := e.T.Run(ctx, "cat "+q(n.ServiceFile(name))+" 2>/dev/null || true")
		if err != nil {
			return err
		}
		diff, _ := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
			A: difflib.SplitLines(res.Stdout), B: difflib.SplitLines(string(rendered[name])),
			FromFile: "live (" + name + ")", ToFile: "planned (" + name + ")", Context: 2,
		})
		if diff != "" {
			changed = true
			e.ui.Diff(diff)
		}
	}
	if !changed {
		e.logf("service apply: no change vs what is running")
	}

	// destructive-mount check: a named volume or absolute bind the running
	// container uses that the new config drops means data would detach
	var destructive []string
	for _, acc := range e.Spec.ServiceNames() {
		id, err := e.serviceContainerID(ctx, acc)
		if err != nil {
			return err
		}
		if id == "" {
			continue // not running: apply will create it, nothing to lose
		}
		res, err := e.T.Run(ctx,
			`docker inspect -f '{{range .Mounts}}{{.Type}}={{if eq .Type "volume"}}{{.Name}}{{else}}{{.Source}}{{end}} {{end}}' `+id)
		if err != nil {
			return err
		}
		// What the planned document mounts. A service's durable volume is
		// derived, so this is the contract name and a mount that no longer
		// appears in it is data about to detach.
		newSet := map[string]bool{}
		for _, v := range e.Spec.Services[acc].Volumes {
			newSet["volume="+n.ServiceVolume(acc, v)] = true
		}
		for _, m := range strings.Fields(res.Stdout) {
			src := strings.SplitN(m, "=", 2)
			if len(src) != 2 {
				continue
			}
			// per-release payload binds live under the releases tree and
			// change every release by construction — not data
			if strings.Contains(src[1], "/releases/") {
				continue
			}
			if !newSet[m] {
				destructive = append(destructive, acc+": "+m)
			}
		}
	}
	if len(destructive) > 0 && !force {
		return fmt.Errorf("destructive mount change(s) — data would detach:\n  %s\nre-run with --allow-destructive-mounts if intended",
			strings.Join(destructive, "\n  "))
	}
	if len(destructive) > 0 {
		e.warnf("proceeding past %d destructive mount change(s) (--allow-destructive-mounts)", len(destructive))
	}

	// the regime: lock, fence, journal, converge
	epoch, err := e.AcquireLock(ctx, releaseID, e.Opts.ForceLock)
	if err != nil {
		return err
	}
	defer e.ReleaseLock(ctx)
	if err := e.WriteFence(ctx, releaseID, epoch); err != nil {
		return err
	}
	jw := &journal.Writer{T: e.T, Names: e.names(), DeployID: releaseID, Epoch: epoch, Operator: journal.DefaultOperator(), GitSHA: e.Opts.GitSHA, ConfigHash: e.Opts.ConfigHash, Runner: &e.Opts.Runner}
	if err := jw.Append(ctx, journal.Record{Phase: "accessory-apply", Event: "start", Detail: strings.Join(e.Spec.ServiceNames(), ",")}); err != nil {
		return fmt.Errorf("journal service apply start: %w", err)
	}
	defer func() {
		finish := journal.Record{Phase: "accessory-apply", Event: "finish", Status: "ok"}
		if err != nil {
			finish.Status = "fail"
			finish.Detail = err.Error()
		}
		if journalErr := jw.Append(ctx, finish); journalErr != nil {
			err = errors.Join(err, fmt.Errorf("journal service apply finish: %w", journalErr))
		}
	}()

	if err := e.ApplyServices(ctx); err != nil {
		return err
	}
	for _, acc := range e.Spec.ServiceNames() {
		id, _ := e.serviceContainerID(ctx, acc)
		if id != "" {
			h, _ := e.healthOf(ctx, id)
			e.logf("service %s: %s", acc, h)
		}
	}
	return nil
}

// refuseUnsafeMajorUpgrade compares the version a service is running against
// the one the project now declares.
//
// Onebox owns these services, which means it owns the consequences of changing
// one. It cannot perform a dump-and-restore upgrade yet, so the honest answer
// is to refuse the change and say what it would take, rather than to converge
// and leave the operator with a database that will not start.
func (e *Engine) refuseUnsafeMajorUpgrade(ctx context.Context, n app.Names, force bool) error {
	for _, name := range e.Spec.ServiceNames() {
		if e.Spec.UpgradeInPlace(name) {
			continue
		}
		// The version that last ran successfully, which is the one that wrote
		// the data directory. Absent means Onebox has never recorded a healthy
		// run, and it cannot then claim to know what the data is — refusing on
		// a guess would trap an operator recovering from exactly that state.
		res, err := e.T.Run(ctx, "cat "+q(n.ServiceVersionFile(name))+" 2>/dev/null || true")
		if err != nil {
			return err
		}
		applied := strings.TrimSpace(res.Stdout)
		if applied == "" {
			continue
		}
		declared := e.Spec.DeclaredVersion(name)
		if app.MajorOf(applied) == app.MajorOf(declared) {
			continue
		}
		runningVersion := applied
		if force {
			e.warnf("service %s: %s → %s across a major version (--allow-destructive-mounts); its data directory may not open",
				name, runningVersion, declared)
			continue
		}
		return fmt.Errorf(
			"service %s runs %s and the project declares %s. A %s data directory written by %s "+
				"cannot be opened by %s, so replacing the container would leave it crash-looping with the "+
				"data intact and unreachable. Onebox does not perform this upgrade yet: dump the data, "+
				"remove the service and its volume, then apply the new version and restore",
			name, runningVersion, declared, name, app.MajorOf(runningVersion), app.MajorOf(declared))
	}
	return nil
}
