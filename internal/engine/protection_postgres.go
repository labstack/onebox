package engine

import (
	"context"
	"fmt"
	"path"

	"github.com/labstack/onebox/internal/app"
)

// Executable protection for the postgres driver.
//
// Everything above this file describes protection: the project declares intent,
// the catalogue declares what each driver could support, the lifecycle state
// records whether it was ever established, and the artifact set records what a
// protected service should look like. None of it runs anything. This file and
// its _ops sibling are the part that does, and they are deliberately narrow —
// one driver, one recovery kind, physical base plus WAL.
//
// Whether a service *is* protected is not decided here. It is durable state on
// the target, observed at project load and bound into the rendered project
// before anything renders, so a policy that was declared but never enabled
// produces an ordinary server rather than one archiving to a stanza nobody
// created.

// writeProtectionConfigs places the generated pgBackRest configuration for every
// protected service before anything starts that mounts it.
//
// Mode 0644 rather than the 0600 every other generated file gets, and that is a
// deliberate exception: the file is bind-mounted into the container and read by
// the unprivileged server user, so a root-owned 0600 file would be a container
// that cannot start. It is safe precisely because of the design above it — the
// configuration names the repository and holds no credential, and every secret
// arrives separately through the environment.
func (e *Engine) writeProtectionConfigs(ctx context.Context) error {
	configs, err := e.Spec.RenderServiceProtectionConfigs(e.Opts.Environment)
	if err != nil {
		return err
	}
	for _, path := range sortedPaths(configs) {
		// The directory is what the container mounts, so it must exist and be
		// traversable before the file lands in it.
		res, err := e.T.Run(ctx, "mkdir -p "+q(dirOf(path))+" && chmod 755 "+q(dirOf(path)))
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("cannot create the protection configuration directory for %s", path)
		}
		if err := e.writeServiceFile(ctx, path, configs[path]); err != nil {
			return fmt.Errorf("cannot place protection configuration %s: %w", path, err)
		}
		res, err = e.T.Run(ctx, "chmod 644 "+q(path))
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("cannot make protection configuration %s readable by the service", path)
		}
	}
	return nil
}

// WriteProtectionLifecycleState places the already-sealed lifecycle record that
// makes a service protected. The record's schema, transitions, and digest all
// belong to the layer above; the engine only puts the bytes on the target,
// under the same fence as every other generated file.
func (e *Engine) WriteProtectionLifecycleState(ctx context.Context, service string, body []byte) error {
	n := e.names()
	res, err := e.T.Run(ctx, "mkdir -p "+q(n.AppDir()+"/protection/state"))
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("cannot create the protection state directory: %s", res.Stderr)
	}
	return e.writeServiceFile(ctx, n.ProtectionLifecycleStateFile(service), body)
}

// RebindServiceRuntimeStates re-derives the rendered project from lifecycle
// state the caller has just written. Enablement is the one flow that needs it:
// the project was loaded before the service was protected, so without this the
// same run would render the unprotected server it started from.
func (e *Engine) RebindServiceRuntimeStates(states map[string]app.ServiceRuntimeState) error {
	bound, err := e.Spec.WithServiceRuntimeStates(states)
	if err != nil {
		return err
	}
	e.Spec = bound
	return nil
}

// ResolveProtectedImage pins the derived image by the digest the host actually
// has, after pulling it. A tag is not a pin: the whole reason protected image
// selection is durable state is that the bytes running over a live data
// directory must not change because a tag moved.
func (e *Engine) ResolveProtectedImage(ctx context.Context, service string) (string, error) {
	declared, ok := e.Spec.Services[service]
	if !ok {
		return "", fmt.Errorf("service %s is not declared in this project", service)
	}
	repository, ok := app.ProtectedImageRepository("postgres")
	if !ok {
		return "", fmt.Errorf("service %s has no protected image in the lifecycle catalogue", service)
	}
	reference := repository + ":" + app.VersionString(declared.Version)
	st := e.ui.Step("protected image "+reference, false)
	res, err := e.T.Run(ctx, "docker pull "+q(reference))
	if err != nil {
		st(err)
		return "", err
	}
	if res.ExitCode != 0 {
		err := fmt.Errorf("cannot pull the protected image %s: %s", reference, lastLines(res.Stderr, 3))
		st(err)
		return "", err
	}
	res, err = e.T.Run(ctx, "docker image inspect --format '{{index .RepoDigests 0}}' "+q(reference))
	if err != nil {
		st(err)
		return "", err
	}
	pinned := trimLine(res.Stdout)
	if res.ExitCode != 0 || !containsDigest(pinned) {
		err := fmt.Errorf(
			"the protected image %s has no registry digest on this host; it must come from a registry rather than a local build",
			reference)
		st(err)
		return "", err
	}
	st(nil)
	return pinned, nil
}

func sortedPaths(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// ReadProtectionLifecycleState returns the raw lifecycle record for a service,
// or nil when none exists. Decoding belongs to the layer that owns the schema;
// the engine only fetches the bytes.
func (e *Engine) ReadProtectionLifecycleState(ctx context.Context, service string) ([]byte, error) {
	path := e.names().ProtectionLifecycleStateFile(service)
	res, err := e.T.Run(ctx, "cat "+q(path)+" 2>/dev/null || true")
	if err != nil {
		return nil, err
	}
	if len(res.Stdout) == 0 {
		return nil, nil
	}
	return []byte(res.Stdout), nil
}

func dirOf(full string) string { return path.Dir(full) }
