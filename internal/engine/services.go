package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/labstack/onebox/internal/app"
)

// Supporting services are applied outside the release, because they outlive
// it. Each has its own Compose project and its own document under the
// application's state directory, so `ob deploy` and `ob rollback` cannot reach
// them: a database is not something an application release gets to restart,
// and a rollback that took the database with it would be a rollback nobody
// could afford to run.
//
// Credentials are generated here, on the target, once. They are not in the
// project, not in the rendered runtime, and not in the digest — reading a
// release tells you nothing you should not know, and there is no password for
// anyone to copy into a repository. The generator is `openssl rand` where it
// exists and /dev/urandom where it does not, because a target that cannot
// produce a random string must fail loudly rather than pick something
// guessable.

// ApplyServices converges every declared service. It is idempotent: a service
// already running with the same definition is left alone, and a credential
// that already exists is never regenerated — rotating a password silently
// would lock the application out of its own database.
func (e *Engine) ApplyServices(ctx context.Context) error {
	names := e.Spec.ServiceNames()
	if len(names) == 0 {
		return nil
	}
	// Rendered here rather than handed in. A caller that forgot would produce
	// a host missing a database, and the engine already holds everything the
	// documents are derived from.
	rendered, err := e.Spec.RenderServices(e.Opts.Environment)
	if err != nil {
		return err
	}
	n := e.Spec.NamesFor(e.Opts.Environment)

	// The network is shared by the application and every service, and it
	// outlives both. Creating it here rather than in a release is what lets a
	// release be removed without cutting the application off from its data.
	if res, err := e.mutate(ctx, fmt.Sprintf(
		"docker network inspect %s >/dev/null 2>&1 || docker network create %s",
		q(n.ServiceNetwork()), q(n.ServiceNetwork()))); err != nil {
		return err
	} else if res.ExitCode != 0 {
		return fmt.Errorf("service network: %s", strings.TrimSpace(res.Stderr))
	}

	if res, err := e.mutate(ctx, "install -d -m 700 "+q(n.ServiceDir())); err != nil {
		return err
	} else if res.ExitCode != 0 {
		return fmt.Errorf("service directory: %s", strings.TrimSpace(res.Stderr))
	}

	for _, name := range names {
		doc, ok := rendered[name]
		if !ok {
			return fmt.Errorf("service %s was declared but not rendered — this is an Onebox bug", name)
		}
		if err := e.ensureServiceSecret(ctx, n, name); err != nil {
			return err
		}
		if err := e.writeServiceFile(ctx, n.ServiceFile(name), doc); err != nil {
			return fmt.Errorf("service %s: cannot place its runtime: %w", name, err)
		}
		cmd := fmt.Sprintf("docker compose -p %s -f %s up -d --remove-orphans",
			q(n.ServiceProject(name)), q(n.ServiceFile(name)))
		st := e.ui.Step("service "+name, false)
		res, err := e.mutate(ctx, cmd)
		if err != nil {
			st(err)
			return err
		}
		if res.ExitCode != 0 {
			st(fmt.Errorf("exit %d", res.ExitCode))
			return fmt.Errorf("service %s: %s", name, strings.TrimSpace(res.Stderr))
		}
		st(nil)
	}
	return nil
}

// ensureServiceSecret generates the credential and the connection file the
// first time, and leaves both alone afterwards.
//
// The two files are written together and the client file is derived from the
// secret, so they cannot disagree. Two runners must not reach this at once —
// one would establish a password the other's application never sees — which is
// why every caller holds the deploy lock first.
func (e *Engine) ensureServiceSecret(ctx context.Context, n app.Names, name string) error {
	client, ok := e.Spec.ClientEnvFor(name)
	if !ok {
		return fmt.Errorf("service %s has no known driver — this is an Onebox bug", name)
	}
	script := client.ClientEnvScript(n.ServiceSecretFile(name), n.ServiceClientFile(name))
	res, err := e.mutate(ctx, script)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("service %s: cannot establish its credential: %s", name, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// writeServiceFile places a generated document on the target through stdin
// rather than interpolating it into a command, so no content can be read as
// shell. It writes beside the target and renames, so an interrupted run cannot
// leave a half-written Compose file that the next apply would try to use.
func (e *Engine) writeServiceFile(ctx context.Context, path string, body []byte) error {
	tmp := path + ".ob-tmp"
	cmd := "umask 077 && cat > " + q(tmp) + " && mv -f " + q(tmp) + " " + q(path)
	if e.fenceVal != "" {
		cmd = `if [ "$(cat ` + q(e.fencePath()) + ` 2>/dev/null)" = ` + q(e.fenceVal) + ` ]; then ` +
			cmd + `; else echo ob-fenced >&2; exit 97; fi`
	}
	res, err := e.T.RunInput(ctx, cmd, string(body))
	if err != nil {
		return err
	}
	if res.ExitCode == 97 && strings.Contains(res.Stderr, "ob-fenced") {
		return ErrFenced
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%s", strings.TrimSpace(res.Stderr))
	}
	return nil
}

// serviceContainerID finds a service's container. A service lives in its own
// Compose project, so looking it up by the application's project — which is
// how every workload is found — would always come back empty.
func (e *Engine) serviceContainerID(ctx context.Context, name string) (string, error) {
	n := e.Spec.NamesFor(e.Opts.Environment)
	res, err := e.T.Run(ctx,
		"docker ps -q --filter label=com.docker.compose.project="+q(n.ServiceProject(name))+
			" --filter label=com.docker.compose.service="+q(name))
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("list service %s containers: %s", name, strings.TrimSpace(res.Stderr))
	}
	ids, err := splitIDs(res.Stdout)
	if err != nil || len(ids) == 0 {
		return "", err
	}
	return ids[0], nil
}
