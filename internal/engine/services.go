package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

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
// EnsureServiceConnections makes the target carry what the application needs to
// reach its services: the shared network, the credential, and the connection
// files each workload reads — without touching a running service.
//
// A release runs this because a release can introduce a workload that needs a
// database. The connection file is derived per workload, so a workload added
// today has no file until something writes one, and the deploy failed with the
// path of a file nobody had told the operator to create. Converging the
// services instead would mean a deploy could restart a database, which is the
// one thing their separation exists to prevent.
func (e *Engine) EnsureServiceConnections(ctx context.Context) error {
	names := e.Spec.ServiceNames()
	if len(names) == 0 {
		return nil
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
		if err := e.ensureServiceSecret(ctx, n, name); err != nil {
			return err
		}
	}
	return nil
}

// ApplyServices converges every declared service and the connections to it.
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
	if err := e.EnsureServiceConnections(ctx); err != nil {
		return err
	}
	n := e.Spec.NamesFor(e.Opts.Environment)

	for _, name := range names {
		doc, ok := rendered[name]
		if !ok {
			return fmt.Errorf("service %s was declared but not rendered — this is an Onebox bug", name)
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

		// Recorded only after the service reports healthy, because the fact
		// worth keeping is which version successfully opened the data
		// directory — not which image was last started.
		if healthy, err := e.serviceIsHealthy(ctx, name); err != nil {
			return err
		} else if healthy {
			if err := e.writeServiceFile(ctx, n.ServiceVersionFile(name),
				[]byte(e.Spec.DeclaredVersion(name)+"\n")); err != nil {
				return fmt.Errorf("service %s: cannot record its version: %w", name, err)
			}
		}
	}
	return nil
}

// serviceIsHealthy waits briefly for a just-started service to report health.
// A driver with no health check reports "none", which is as strong a statement
// as it can make and is treated as running.
func (e *Engine) serviceIsHealthy(ctx context.Context, name string) (bool, error) {
	deadline := e.Opts.Now().Add(90 * time.Second)
	for {
		id, err := e.serviceContainerID(ctx, name)
		if err != nil {
			return false, err
		}
		if id != "" {
			h, err := e.healthOf(ctx, id)
			if err != nil {
				return false, err
			}
			if h == "healthy" || h == "none" {
				return true, nil
			}
		}
		if e.Opts.Now().After(deadline) {
			return false, nil
		}
		e.Opts.Sleep(2 * time.Second)
	}
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
	// One alias file per workload that asked for its own names. They are
	// derived from the same password in the same script, so they cannot
	// disagree with the canonical file or with each other.
	// Every workload, not just the ones in the release order — that excludes
	// jobs, and a migration job needs a database more surely than anything
	// else in the project.
	var aliases []app.AliasFile
	for _, workload := range sortedNames(e.Spec.Workloads) {
		for _, need := range e.Spec.Workloads[workload].Needs {
			if need.Name == name && len(need.Env) > 0 {
				aliases = append(aliases, app.AliasFile{
					Path: n.ServiceAliasFile(name, workload), Vars: need.Env,
				})
			}
		}
	}
	script := client.ClientEnvScript(n.ServiceSecretFile(name), n.ServiceClientFile(name), aliases)
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
