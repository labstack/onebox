package onebox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	ctypes "github.com/compose-spec/compose-go/v2/types"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/compose"
)

// Loading changed shape with the declarative contract. There is no longer a
// user-authored Compose file to read and classify: the project declares its
// workloads, and the runtime is generated from that declaration. What used to
// be inference — guess which service is the app, which is a database, what
// order they start in — is now something the author states and the loader
// checks.
//
// The generated runtime is still parsed back into a Compose project, because
// the execution engine works in terms of services and images and that is the
// form it needs. Parsing our own output is cheap and keeps one source of truth
// for what a service is.

type loadedProject struct {
	spec     *app.Spec
	resolved *app.Resolved
	// compose is the parsed generated runtime. On an inspection load it may
	// carry placeholder images for workloads nobody has built yet; unresolved
	// names them so a caller reports "not released" rather than a fake digest.
	compose      *ctypes.Project
	unresolved   []string
	configPath   string
	configBytes  []byte
	composeBytes []byte
}

// loadProject reads the project, resolves it for the environment, and renders
// the runtime it implies.
//
// lenient asks for a runtime that can be described but not deployed: a project
// whose images are not built yet still has a shape worth reporting. Every
// execution path passes false and fails closed on an unresolved image.
func (s *Service) loadProject(ctx context.Context, lenient bool) (*loadedProject, error) {
	return loadProjectAt(ctx, s.configPath, s.environment, lenient, s.images)
}

func loadProjectAt(ctx context.Context, configPath, environment string, lenient bool, images app.Images) (*loadedProject, error) {
	absConfig, err := filepath.Abs(configPath)
	if err != nil {
		return nil, fmt.Errorf("resolve config path: %w", err)
	}
	cfgBytes, err := os.ReadFile(absConfig)
	if err != nil {
		return nil, err
	}
	spec, err := app.LoadBytes(cfgBytes, absConfig)
	if err != nil {
		return nil, err
	}
	spec.Dir = filepath.Dir(absConfig)

	resolved, err := spec.Resolve(environment)
	if err != nil {
		return nil, err
	}

	var rendered *app.Rendered
	if lenient {
		rendered, err = resolved.RenderForInspection(environment, images)
	} else {
		rendered, err = resolved.Render(environment, "", images)
	}
	if err != nil {
		return nil, err
	}

	p, err := compose.LoadBytes(ctx, rendered.Bytes, resolved.NamesFor(environment).ComposeProject(), spec.Dir)
	if err != nil {
		return nil, fmt.Errorf("the generated runtime did not parse as Compose — this is an Onebox bug: %w", err)
	}

	return &loadedProject{
		spec: spec, resolved: resolved, compose: p, unresolved: rendered.Unresolved,
		configPath: absConfig, configBytes: cfgBytes, composeBytes: rendered.Bytes,
	}, nil
}

// durableVolumeNames is the set of managed volumes a workload keeps across
// releases. Bind mounts are excluded: the host owns those, and naming one as a
// backup resource would promise Onebox can restore something it never created.
func durableVolumeNames(w app.Workload) []string {
	var out []string
	for _, v := range w.Volumes {
		if !v.IsBind() {
			out = append(out, v.Name)
		}
	}
	sort.Strings(out)
	return out
}

// sopsSource is the declared SOPS-encrypted secrets file, if there is one. The
// contract allows several providers; only SOPS has an implementation, and a
// project declaring another gets nothing rather than a silent fallback to a
// file it did not name.
func sopsSource(r *app.Resolved) string {
	for _, name := range sortedNames(r.Secrets) {
		if s := r.Secrets[name]; s.Provider == "sops" {
			return s.File
		}
	}
	return ""
}

func sortedNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
