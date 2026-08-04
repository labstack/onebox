package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/pmezard/go-difflib/difflib"

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
func (e *Engine) ServiceApply(ctx context.Context, releaseID string, force bool) error {
	if len(e.Spec.ServiceNames()) == 0 {
		return fmt.Errorf("no services declared")
	}
	n := e.Spec.NamesFor(e.Opts.Environment)
	rendered, err := e.Spec.RenderServices(e.Opts.Environment)
	if err != nil {
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
		return fmt.Errorf("destructive mount change(s) — data would detach:\n  %s\nre-run with --force if intended",
			strings.Join(destructive, "\n  "))
	}
	if len(destructive) > 0 {
		e.warnf("proceeding past %d destructive mount change(s) (--force)", len(destructive))
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
	jw := &journal.Writer{T: e.T, App: e.Spec.Name, DeployID: releaseID, Epoch: epoch, Operator: journal.DefaultOperator(), GitSHA: e.Opts.GitSHA, ConfigHash: e.Opts.ConfigHash, Runner: &e.Opts.Runner}
	_ = jw.Append(ctx, journal.Record{Phase: "accessory-apply", Event: "start", Detail: strings.Join(e.Spec.ServiceNames(), ",")})

	if err := e.ApplyServices(ctx); err != nil {
		_ = jw.Append(ctx, journal.Record{Phase: "accessory-apply", Event: "finish", Status: "fail", Detail: err.Error()})
		return err
	}
	for _, acc := range e.Spec.ServiceNames() {
		id, _ := e.serviceContainerID(ctx, acc)
		if id != "" {
			h, _ := e.healthOf(ctx, id)
			e.logf("service %s: %s", acc, h)
		}
	}
	_ = jw.Append(ctx, journal.Record{Phase: "accessory-apply", Event: "finish", Status: "ok"})
	return nil
}
