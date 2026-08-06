package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/onebox/internal/journal"
	"github.com/labstack/onebox/internal/release"
)

// SecretsPush ships freshly rendered secrets into the CURRENT release and
// bounces the roles — a journaled maintenance event that diffs the
// rendered output, bounce only when it changed; content never logged, only
// hashes travel).
func (e *Engine) SecretsPush(ctx context.Context, name string, envBytes []byte) error {
	_, err := e.SecretsPushWithJournalID(ctx, name, envBytes)
	return err
}

// SecretsPushWithJournalID pushes secrets to the current release and returns
// the journal identity used for any maintenance evidence. The current release
// is returned even when no mutation is needed or a later step fails.
// name is the file the generated runtime references for this entry. It is a
// parameter rather than a constant because the runtime now names one file per
// encrypted entry: a single shared name made a later entry win outright instead
// of key by key, and — worse — pushing to a constant while generation
// referenced a derived name wrote a file nothing read, so `secrets push`
// reported success and changed nothing the container could see.
func (e *Engine) SecretsPushWithJournalID(ctx context.Context, name string, envBytes []byte) (string, error) {
	cur, err := release.Current(ctx, e.T, e.names())
	if err != nil {
		return "", err
	}
	if cur == "" {
		return "", fmt.Errorf("nothing deployed yet — secrets ship with `ob deploy`")
	}
	return cur, e.secretsPush(ctx, cur, name, envBytes)
}

func (e *Engine) secretsPush(ctx context.Context, cur, name string, envBytes []byte) error {
	curDir := release.PathsFor(e.names()).Releases + "/" + cur
	sum := sha256.Sum256(envBytes)
	localHash := hex.EncodeToString(sum[:])
	res, err := e.T.Run(ctx, "sha256sum "+q(curDir+"/"+name)+" 2>/dev/null | awk '{print $1}'")
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
	jw := &journal.Writer{T: e.T, Names: e.names(), DeployID: cur, Epoch: epoch, Operator: journal.DefaultOperator(), Runner: &e.Opts.Runner}
	_ = jw.Append(ctx, journal.Record{Phase: "secrets-push", Event: "start", Detail: "hash=sha256:" + localHash})

	staging, err := os.MkdirTemp("", "ob-secrets")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if err := os.WriteFile(filepath.Join(staging, name), envBytes, 0o600); err != nil {
		return err
	}
	remoteStaging := e.base() + "/.secrets-" + strconv.Itoa(epoch)
	staleCleanup := "find " + q(e.base()) + " -mindepth 1 -maxdepth 1 -type d -name '.secrets-*' -exec rm -rf -- {} +" +
		" && find " + q(curDir) + " -mindepth 1 -maxdepth 1 -type f -name '.ob-decrypted-*.tmp-*' -delete -o -name '*.env.tmp-*' -delete"
	if res, err := e.mutate(ctx, staleCleanup); err != nil || res.ExitCode != 0 {
		return fmt.Errorf("clean stale secret staging: %v %s", err, strings.TrimSpace(res.Stderr))
	}
	remoteTemporary := curDir + "/" + name + ".tmp-" + strconv.Itoa(epoch)
	defer func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = e.mutate(cleanupContext, "rm -rf "+q(remoteStaging)+" && rm -f "+q(remoteTemporary))
	}()
	if err := e.T.Upload(ctx, staging, remoteStaging); err != nil {
		_ = jw.Append(ctx, journal.Record{Phase: "secrets-push", Event: "finish", Status: "fail", Detail: err.Error()})
		return err
	}
	install := "cp " + q(remoteStaging+"/"+name) + " " + q(remoteTemporary) +
		" && chmod 600 " + q(remoteTemporary) + " && mv -f " + q(remoteTemporary) + " " + q(curDir+"/"+name) +
		" && rm -rf " + q(remoteStaging)
	if res, err := e.mutate(ctx, install); err != nil {
		_ = jw.Append(ctx, journal.Record{Phase: "secrets-push", Event: "finish", Status: "fail", Detail: err.Error()})
		return err
	} else if res.ExitCode != 0 {
		return fmt.Errorf("install secrets: %s", strings.TrimSpace(res.Stderr))
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
