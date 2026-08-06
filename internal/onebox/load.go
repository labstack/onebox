package onebox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	ctypes "github.com/compose-spec/compose-go/v2/types"

	"errors"
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

// loadProjectWith loads against a specific image map.
//
// Deploying from a saved plan has to use the plan's pinned references, and it
// has to use them here — rendering happens during the load, so a build-sourced
// workload fails with image_unresolved long before anything downstream gets a
// chance to apply them. The plan is the authority on what a build produced;
// anything passed on the command line is a fallback for what the plan did not
// name.
func (s *Service) loadProjectWith(ctx context.Context, lenient bool, images app.Images) (*loadedProject, error) {
	merged := app.Images{}
	for name, ref := range s.images {
		merged[name] = ref
	}
	for name, ref := range images {
		merged[name] = ref
	}
	if len(merged) == 0 {
		merged = nil
	}
	return loadProjectRestricted(ctx, s.configPath, s.environment, lenient, merged, true)
}

func loadProjectAt(ctx context.Context, configPath, environment string, lenient bool, images app.Images) (*loadedProject, error) {
	return loadProjectRestricted(ctx, configPath, environment, lenient, images, false)
}

// loadProjectRestricted optionally narrows the image map to the workloads that
// cannot render without it.
//
// A plan's pinned images are digests resolved *after* the plan rendered, so
// feeding all of them back into the render would produce a different document
// than the one the plan bound — and the binding check would refuse its own
// plan. Only a build-sourced workload needs an image to render at all, and for
// that workload the plan's entry is the same reference the render already used.
func loadProjectRestricted(ctx context.Context, configPath, environment string, lenient bool, images app.Images, onlyBuilt bool) (*loadedProject, error) {
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

	if onlyBuilt && len(images) > 0 {
		restricted := app.Images{}
		for name, ref := range images {
			if w, ok := spec.Workloads[name]; ok && w.Build != nil {
				restricted[name] = ref
			}
		}
		images = nil
		if len(restricted) > 0 {
			images = restricted
		}
	}

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

	interpolation, err := resolved.Spec.InterpolationEnv()
	if err != nil {
		return nil, err
	}
	p, err := compose.LoadBytes(ctx, rendered.Bytes, resolved.NamesFor(environment).ComposeProject(), spec.Dir, interpolation)
	if err != nil {
		var interpolation *compose.InterpolationError
		if errors.As(err, &interpolation) {
			return nil, err
		}
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

// encryptedEntries are the document-scope entries a release must decrypt.
//
// This replaces a function that returned one file, chosen by sorting the
// declared secrets by name. A project declaring two got whichever sorted first,
// which is how one environment's credentials reached another. There is nothing
// to choose now: the list is the answer, in the order it was written.
func encryptedEntries(r *app.Resolved) []app.EnvFile {
	var out []app.EnvFile
	seen := map[string]bool{}
	// Sorted, because ranging a map is not an order. The comment this replaces
	// claimed "the order it was written" while iterating `Workloads`, which
	// swapped one nondeterminism for another — and the whole point of the list
	// is that nothing is chosen by accident.
	for _, name := range sortedNames(r.Spec.Workloads) {
		w := r.Spec.Workloads[name]
		for _, entry := range r.Spec.EnvFilesFor(w) {
			if entry.Encrypted() && !seen[entry.File] {
				seen[entry.File] = true
				out = append(out, entry)
			}
		}
	}
	return out
}

func sortedNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
