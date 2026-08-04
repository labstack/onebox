package app

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Deploy is the first execution path on this contract. It renders, preflights,
// stages a release, brings it up, waits for health, and records the release as
// current.
//
// What it deliberately does NOT do yet: take a lock, fence a stale runner,
// write a journal, drain connections, roll back on failure, or verify anything
// beyond container health. Those live in the execution engine, and this path
// exists to prove the contract reaches a real host — not to replace them. It
// refuses to pretend otherwise, which is why the result reports what it skipped.

// Target is the transport this path needs.
type Target interface {
	Runner
	Upload(ctx context.Context, localDir, remoteDir string) error
	Target() string
}

// DeployResult reports what happened.
type DeployResult struct {
	Release        string   `json:"release"`
	ComposeProject string   `json:"compose_project"`
	ReleaseDir     string   `json:"release_dir"`
	Digest         string   `json:"digest"`
	Healthy        []string `json:"healthy,omitempty"`
	Unhealthy      []string `json:"unhealthy,omitempty"`
	Skipped        []string `json:"safety_not_yet_implemented"`
}

// OK reports whether every workload that declares health reached it.
func (d *DeployResult) OK() bool { return len(d.Unhealthy) == 0 }

// DeployOptions bounds the parts a caller may reasonably vary.
type DeployOptions struct {
	Images     Images
	Wait       time.Duration // how long to wait for health; 0 uses a default
	SkipHealth bool
}

// Deploy brings a release up on the target.
func (r *Resolved) Deploy(ctx context.Context, t Target, releaseID string, opts DeployOptions) (*DeployResult, error) {
	p := r.Spec
	n := p.NamesFor(r.Env)

	rendered, err := r.Render(r.Env, releaseID, opts.Images)
	if err != nil {
		return nil, err
	}

	report, err := r.Preflight(ctx, t)
	if err != nil {
		return nil, err
	}
	if !report.OK() {
		var names []string
		for _, c := range report.Failures() {
			names = append(names, c.Name)
		}
		return nil, errf("preflight_failed", "", "ob preflight",
			"the target is not ready: %s", strings.Join(names, ", "))
	}

	releaseDir := n.ReleaseDir(releaseID)
	staging, err := r.stage(rendered)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(staging)

	// Creating the release directory is the first mutation. Everything before
	// this point was a read, so a failure up to here leaves the host untouched.
	if err := run(ctx, t, fmt.Sprintf("mkdir -p %q", releaseDir)); err != nil {
		return nil, errf("release_dir_failed", releaseDir, "",
			"cannot create the release directory: %v", err)
	}

	// Services come up before the release that needs them, and outside it. An
	// application released against a database that has not started yet fails
	// for a reason that has nothing to do with the application.
	if err := r.applyServices(ctx, t, rendered.Services); err != nil {
		return nil, err
	}
	if err := t.Upload(ctx, staging, releaseDir); err != nil {
		return nil, errf("stage_failed", releaseDir, "",
			"cannot stage the release: %v", err)
	}

	compose := fmt.Sprintf("docker compose -p %q -f %q", n.ComposeProject(),
		filepath.Join(releaseDir, "compose.yaml"))
	if err := run(ctx, t, compose+" up -d --remove-orphans"); err != nil {
		return nil, errf("compose_up_failed", "", "ob preflight",
			"bringing the release up failed: %v", err)
	}

	result := &DeployResult{
		Release:        releaseID,
		ComposeProject: n.ComposeProject(),
		ReleaseDir:     releaseDir,
		Digest:         rendered.Digest,
		Skipped: []string{
			"deploy lock", "runner fencing", "journal", "connection drain",
			"automatic rollback", "release verification",
		},
	}

	if !opts.SkipHealth {
		healthy, unhealthy, err := r.awaitHealth(ctx, t, compose, opts.Wait)
		if err != nil {
			return nil, err
		}
		result.Healthy, result.Unhealthy = healthy, unhealthy
	}

	// The current pointer moves only after health, so a failed release does not
	// become the one a reader believes is live.
	if result.OK() {
		_ = run(ctx, t, fmt.Sprintf("ln -sfn %q %q", releaseDir, n.CurrentLink()))
	}
	return result, nil
}

// stage builds the local directory that becomes the release: the generated
// runtime, plus every repository file the project declares it needs.
func (r *Resolved) stage(rendered *Rendered) (string, error) {
	dir, err := os.MkdirTemp("", "ob-stage-*")
	if err != nil {
		return "", errf("stage_failed", "", "", "cannot create a staging directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), rendered.Bytes, 0o600); err != nil {
		os.RemoveAll(dir)
		return "", errf("stage_failed", "", "", "cannot write the runtime: %v", err)
	}

	var wanted []string
	wanted = append(wanted, r.Files...)
	if r.Runtime != nil {
		wanted = append(wanted, r.Runtime.EnvFiles...)
	}
	for _, name := range sortedKeys(r.Workloads) {
		wanted = append(wanted, r.Workloads[name].EnvFiles...)
	}

	seen := map[string]bool{}
	for _, rel := range wanted {
		if rel == "" || seen[rel] {
			continue
		}
		seen[rel] = true
		src, err := resolveRepoPath(r.Dir, rel)
		if err != nil {
			os.RemoveAll(dir)
			return "", err
		}
		body, err := os.ReadFile(src)
		if err != nil {
			os.RemoveAll(dir)
			return "", errf("stage_file_missing", rel, "",
				"declared file %q cannot be read: %v", rel, err)
		}
		dst := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			os.RemoveAll(dir)
			return "", errf("stage_failed", rel, "", "%v", err)
		}
		// Mode 0600: a staged env file carries secrets and must not be readable
		// by anything else that lands on the host.
		if err := os.WriteFile(dst, body, 0o600); err != nil {
			os.RemoveAll(dir)
			return "", errf("stage_failed", rel, "", "%v", err)
		}
	}
	return dir, nil
}

// awaitHealth waits for the workloads that declare a health check. A workload
// without one is not reported as healthy, because nothing was checked.
func (r *Resolved) awaitHealth(ctx context.Context, t Target, compose string, wait time.Duration) ([]string, []string, error) {
	var expect []string
	for _, name := range sortedKeys(r.Workloads) {
		if r.Workloads[name].Health != nil && r.Workloads[name].Role != RoleJob {
			expect = append(expect, name)
		}
	}
	if len(expect) == 0 {
		return nil, nil, nil
	}
	if wait <= 0 {
		wait = 3 * time.Minute
	}

	deadline := time.Now().Add(wait)
	var healthy, unhealthy []string
	for {
		healthy, unhealthy = nil, nil
		res, err := t.Run(ctx, compose+` ps --format '{{.Service}}\t{{.Health}}'`)
		if err != nil {
			return nil, nil, errf("target_unreachable", "", "", "cannot read health: %v", err)
		}
		states := map[string]string{}
		for _, line := range strings.Split(res.Stdout, "\n") {
			svc, state, _ := strings.Cut(strings.TrimSpace(line), "\t")
			if svc != "" {
				states[svc] = strings.TrimSpace(state)
			}
		}
		pending := false
		for _, name := range expect {
			switch states[name] {
			case "healthy":
				healthy = append(healthy, name)
			case "starting", "":
				pending = true
			default:
				unhealthy = append(unhealthy, name)
			}
		}
		if !pending || time.Now().After(deadline) {
			for _, name := range expect {
				if states[name] == "starting" || states[name] == "" {
					unhealthy = append(unhealthy, name+" (still starting)")
				}
			}
			return healthy, unhealthy, nil
		}
		select {
		case <-ctx.Done():
			return healthy, unhealthy, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

func run(ctx context.Context, t Runner, cmd string) error {
	res, err := t.Run(ctx, cmd)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%s", strings.TrimSpace(firstLine(res.Stderr)))
	}
	return nil
}

// applyServices converges the supporting services on the target.
//
// They live outside the release: their own Compose project, their own document
// under the application's state directory, and a credential generated here and
// never anywhere else. A release can be removed without touching them, which
// is the whole reason a database is not a workload.
func (r *Resolved) applyServices(ctx context.Context, t Target, docs map[string][]byte) error {
	if len(docs) == 0 {
		return nil
	}
	n := r.NamesFor(r.Env)

	// External, so Compose does not create it with a release and remove it
	// with one — taking the application's route to its data along with it.
	if err := run(ctx, t, fmt.Sprintf(
		"docker network inspect %q >/dev/null 2>&1 || docker network create %q",
		n.ServiceNetwork(), n.ServiceNetwork())); err != nil {
		return errf("service_network_failed", n.ServiceNetwork(), "",
			"cannot create the service network: %v", err)
	}
	if err := run(ctx, t, fmt.Sprintf("install -d -m 700 %q", n.ServiceDir())); err != nil {
		return errf("service_dir_failed", n.ServiceDir(), "",
			"cannot create the service directory: %v", err)
	}

	for _, name := range sortedKeys(docs) {
		if err := r.ensureServiceCredential(ctx, t, n, name); err != nil {
			return err
		}
		if err := writeRemoteFile(ctx, t, n.ServiceFile(name), docs[name]); err != nil {
			return errf("service_write_failed", name, "",
				"cannot place the runtime for service %q: %v", name, err)
		}
		if err := run(ctx, t, fmt.Sprintf("docker compose -p %q -f %q up -d --remove-orphans",
			n.ServiceProject(name), n.ServiceFile(name))); err != nil {
			return errf("service_up_failed", name, "",
				"service %q did not come up: %v", name, err)
		}
	}
	return nil
}

// ensureServiceCredential generates the password and the connection file once,
// on the target, and leaves both alone afterwards. Regenerating either would
// leave the application holding a credential its database no longer accepts.
func (r *Resolved) ensureServiceCredential(ctx context.Context, t Target, n Names, name string) error {
	client, ok := r.Spec.ClientEnvFor(name)
	if !ok {
		return errf("unknown_service_driver", "services."+name, "",
			"service %q has no known driver", name)
	}
	script := client.ClientEnvScript(n.ServiceSecretFile(name), n.ServiceClientFile(name))
	if err := run(ctx, t, script); err != nil {
		return errf("service_credential_failed", name, "",
			"cannot establish the credential for service %q: %v", name, err)
	}
	return nil
}

// writeRemoteFile places generated content on the target without letting any of
// it be read as shell. The document is base64 on the wire for exactly that
// reason: a Compose file contains quotes, dollars and backticks, and building
// a command around them is how a generator becomes an injection.
//
// It writes beside the target and renames, so an interrupted run cannot leave a
// half-written Compose file for the next apply to use.
func writeRemoteFile(ctx context.Context, t Target, path string, body []byte) error {
	encoded := base64.StdEncoding.EncodeToString(body)
	return run(ctx, t, fmt.Sprintf(
		"umask 077 && printf %%s %q | base64 -d > %q && mv -f %q %q",
		encoded, path+".ob-tmp", path+".ob-tmp", path))
}
