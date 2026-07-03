package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/labstack/yeet/internal/journal"
	"github.com/labstack/yeet/internal/release"
	"github.com/labstack/yeet/internal/secrets"
)

// SecretsPush ships freshly rendered secrets into the CURRENT release and
// bounces the roles — a journaled maintenance event (design §07: diff the
// rendered output, bounce only when it changed; content never logged, only
// hashes travel).
func (e *Engine) SecretsPush(ctx context.Context, envBytes []byte) error {
	cur, err := release.Current(ctx, e.T, e.Cfg.App)
	if err != nil {
		return err
	}
	if cur == "" {
		return fmt.Errorf("nothing deployed yet — secrets ship with `yeet deploy`")
	}
	curDir := release.PathsFor(e.Cfg.App).Releases + "/" + cur
	sum := sha256.Sum256(envBytes)
	localHash := hex.EncodeToString(sum[:])
	res, err := e.T.Run(ctx, "sha256sum "+q(curDir+"/"+secrets.EnvFileName)+" 2>/dev/null | awk '{print $1}'")
	if err != nil {
		return err
	}
	if strings.TrimSpace(res.Stdout) == localHash {
		e.logf("secrets unchanged (hash match) — nothing to do")
		return nil
	}

	epoch, err := e.AcquireLock(ctx, cur, e.Opts.ForceLock)
	if err != nil {
		return err
	}
	defer e.ReleaseLock(ctx)
	if err := e.WriteFence(ctx, cur, epoch); err != nil {
		return err
	}
	jw := &journal.Writer{T: e.T, App: e.Cfg.App, DeployID: cur, Epoch: epoch, Operator: journal.DefaultOperator()}
	_ = jw.Append(ctx, journal.Record{Phase: "secrets-push", Event: "start", Detail: "hash=sha256:" + localHash})

	staging, err := os.MkdirTemp("", "yeet-secrets")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if err := os.WriteFile(filepath.Join(staging, secrets.EnvFileName), envBytes, 0o600); err != nil {
		return err
	}
	if err := e.T.Upload(ctx, staging, curDir); err != nil {
		_ = jw.Append(ctx, journal.Record{Phase: "secrets-push", Event: "finish", Status: "fail", Detail: err.Error()})
		return err
	}

	e.logf("secrets changed — bouncing roles")
	if err := e.releaseRoles(ctx, curDir+"/compose.yaml"); err != nil {
		_ = jw.Append(ctx, journal.Record{Phase: "secrets-push", Event: "finish", Status: "fail", Detail: err.Error()})
		return fmt.Errorf("secrets push: %w", err)
	}
	if err := e.Verify(ctx); err != nil {
		_ = jw.Append(ctx, journal.Record{Phase: "secrets-push", Event: "finish", Status: "fail", Detail: err.Error()})
		return fmt.Errorf("secrets push verify: %w", err)
	}
	_ = jw.Append(ctx, journal.Record{Phase: "secrets-push", Event: "finish", Status: "ok"})
	return nil
}
