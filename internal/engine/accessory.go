package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pmezard/go-difflib/difflib"

	"github.com/labstack/yeet/internal/journal"
	"github.com/labstack/yeet/internal/release"
)

// AccessoryApply converges stateful services explicitly (design §03): a
// planned maintenance event — never mid-deploy. Shows the diff against the
// live release, refuses destructive mount changes without force, then
// `up -d --no-deps <accessories>` under the full lock/fence/journal regime.
func (e *Engine) AccessoryApply(ctx context.Context, releaseID, localStagingDir string, force bool) error {
	if len(e.Cfg.Accessories) == 0 {
		return fmt.Errorf("no accessories declared")
	}
	newB, err := os.ReadFile(filepath.Join(localStagingDir, "compose.yaml"))
	if err != nil {
		return err
	}

	// the diff: rendered vs the live release's compose
	cur, err := release.Current(ctx, e.T, e.Cfg.App)
	if err != nil {
		return err
	}
	live := ""
	if cur != "" {
		res, err := e.T.Run(ctx, "cat "+q(release.PathsFor(e.Cfg.App).Releases+"/"+cur+"/compose.yaml")+" 2>/dev/null || true")
		if err != nil {
			return err
		}
		live = res.Stdout
	}
	diff, _ := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A: difflib.SplitLines(live), B: difflib.SplitLines(string(newB)),
		FromFile: "live (" + cur + ")", ToFile: "planned (" + releaseID + ")", Context: 2,
	})
	if diff == "" {
		e.logf("accessory apply: no rendered change vs live release")
	} else {
		e.ui.Diff(diff)
	}

	// destructive-mount check: a named volume or absolute bind the running
	// container uses that the new config drops means data would detach
	var destructive []string
	for _, acc := range e.Cfg.Accessories {
		id, err := e.containerID(ctx, acc)
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
		newSet := map[string]bool{}
		if svc, ok := e.Project.Services[acc]; ok {
			for _, v := range svc.Volumes {
				newSet[string(v.Type)+"="+v.Source] = true
			}
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
	jw := &journal.Writer{T: e.T, App: e.Cfg.App, DeployID: releaseID, Epoch: epoch, Operator: journal.DefaultOperator(), GitSHA: e.Opts.GitSHA, ConfigHash: e.Opts.ConfigHash}
	_ = jw.Append(ctx, journal.Record{Phase: "accessory-apply", Event: "start", Detail: strings.Join(e.Cfg.Accessories, ",")})

	pushed, err := release.Push(ctx, e.T, localStagingDir, e.Cfg.App, releaseID)
	if err != nil {
		return err
	}
	cc := e.composeCmd(pushed + "/compose.yaml")
	args := strings.Join(e.Cfg.Accessories, " ")
	if res, err := e.mutate(ctx, cc+" up -d --no-deps "+args); err != nil {
		_ = jw.Append(ctx, journal.Record{Phase: "accessory-apply", Event: "finish", Status: "fail"})
		return err
	} else if res.ExitCode != 0 {
		_ = jw.Append(ctx, journal.Record{Phase: "accessory-apply", Event: "finish", Status: "fail", Detail: res.Stderr})
		return fmt.Errorf("accessory apply: %s", strings.TrimSpace(res.Stderr))
	}
	for _, acc := range e.Cfg.Accessories {
		id, _ := e.containerID(ctx, acc)
		if id != "" {
			h, _ := e.healthOf(ctx, id)
			e.logf("accessory %s: %s", acc, h)
		}
	}
	_ = jw.Append(ctx, journal.Record{Phase: "accessory-apply", Event: "finish", Status: "ok"})
	return nil
}
