package onebox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	ctypes "github.com/compose-spec/compose-go/v2/types"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/compose"
	"github.com/labstack/onebox/internal/engine"
)

// There is no user-authored Compose file to read and classify: the project
// declares its workloads, and the runtime is generated from that declaration.
// Which service is the app, which is a database, what order they start in — the
// author states all of it and the loader checks it, rather than any of it being
// inferred.
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
	return s.loadObservedProject(ctx, lenient, s.images, false)
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
	return s.loadObservedProject(ctx, lenient, merged, true)
}

// loadObservedProject resolves authoring input, observes durable service state
// on the target, and only then renders. This ordering is a safety boundary: a
// protected service must never briefly become a mutable tag merely because
// rendering happened before lifecycle state was loaded.
func (s *Service) loadObservedProject(ctx context.Context, lenient bool, images app.Images, onlyBuilt bool) (*loadedProject, error) {
	lp, renderImages, err := resolveProjectRestricted(s.configPath, s.environment, images, onlyBuilt)
	if err != nil {
		return nil, err
	}
	states, err := s.observeServiceRuntimeStates(ctx, lp.resolved)
	if err != nil {
		return nil, err
	}
	if len(states) > 0 {
		lp.resolved, err = lp.resolved.WithServiceRuntimeStates(states)
		if err != nil {
			return nil, err
		}
	}
	if err := renderLoadedProject(ctx, lp, s.environment, lenient, renderImages); err != nil {
		return nil, err
	}
	return lp, nil
}

func (s *Service) observeServiceRuntimeStates(ctx context.Context, resolved *app.Resolved) (map[string]app.ServiceRuntimeState, error) {
	if resolved == nil || len(resolved.Services) == 0 {
		return nil, nil
	}
	environment, err := resolved.Environment(resolved.Env)
	if err != nil {
		return nil, err
	}
	target, err := s.connect(ctx, environment.Target())
	if err != nil {
		return nil, fmt.Errorf("observe service lifecycle state: %w", err)
	}
	defer target.Close()

	names := resolved.NamesFor(resolved.Env)
	states := map[string]app.ServiceRuntimeState{}
	for _, service := range sortedNames(resolved.Services) {
		statePath := names.ProtectionLifecycleStateFile(service)
		result, err := target.Run(ctx, "if [ -f "+quote(statePath)+" ]; then printf 'present\\n'; cat "+quote(statePath)+"; elif [ -e "+quote(statePath)+" ]; then printf 'invalid\\n'; else printf 'missing\\n'; fi")
		if err != nil {
			return nil, fmt.Errorf("observe service %s lifecycle state: %w", service, err)
		}
		if result.ExitCode != 0 {
			return nil, fmt.Errorf("observe service %s lifecycle state failed", service)
		}
		marker, encoded, _ := strings.Cut(result.Stdout, "\n")
		switch marker {
		case "", "missing":
			continue
		case "invalid":
			return nil, fmt.Errorf("service %s lifecycle state path is not a regular file", service)
		case "present":
		default:
			return nil, fmt.Errorf("service %s lifecycle state observation is invalid", service)
		}
		state, err := DecodeProtectionLifecycleState([]byte(encoded))
		if err != nil {
			return nil, fmt.Errorf("service %s lifecycle state: %w", service, err)
		}
		if state.Application != names.App || state.Environment != resolved.Env || state.Service != service {
			return nil, fmt.Errorf("service %s lifecycle state belongs to a different protected identity", service)
		}
		runtime := state.RuntimeState()
		if runtime.ServiceImage != "" {
			runtime.DigestAvailable, err = engine.ServiceImageDigestAvailable(ctx, target, runtime.ServiceImage)
			if err != nil {
				return nil, fmt.Errorf("observe service %s registry image: %w", service, err)
			}
			runtime.CacheVerified, err = engine.ExactServiceImageCached(ctx, target, runtime.ServiceImage)
			if err != nil {
				return nil, fmt.Errorf("observe service %s cached image: %w", service, err)
			}
		}
		states[service] = runtime
	}
	return states, nil
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
	lp, renderImages, err := resolveProjectRestricted(configPath, environment, images, onlyBuilt)
	if err != nil {
		return nil, err
	}
	if err := renderLoadedProject(ctx, lp, environment, lenient, renderImages); err != nil {
		return nil, err
	}
	return lp, nil
}

func resolveProjectRestricted(configPath, environment string, images app.Images, onlyBuilt bool) (*loadedProject, app.Images, error) {
	absConfig, err := filepath.Abs(configPath)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve config path: %w", err)
	}
	cfgBytes, err := os.ReadFile(absConfig)
	if err != nil {
		return nil, nil, err
	}
	spec, err := app.LoadBytes(cfgBytes, absConfig)
	if err != nil {
		return nil, nil, err
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
		return nil, nil, err
	}
	return &loadedProject{spec: spec, resolved: resolved, configPath: absConfig, configBytes: cfgBytes}, images, nil
}

func renderLoadedProject(ctx context.Context, lp *loadedProject, environment string, lenient bool, images app.Images) error {
	resolved, spec := lp.resolved, lp.spec
	var err error
	var rendered *app.Rendered
	if lenient {
		rendered, err = resolved.RenderForInspection(environment, images)
	} else {
		rendered, err = resolved.Render(environment, "", images)
	}
	if err != nil {
		return err
	}

	interpolation, err := resolved.Spec.InterpolationEnv()
	if err != nil {
		return err
	}
	p, err := compose.LoadBytes(ctx, rendered.Bytes, resolved.NamesFor(environment).ComposeProject(), spec.Dir, interpolation)
	if err != nil {
		var interpolation *compose.InterpolationError
		if errors.As(err, &interpolation) {
			if hidden := resolved.Spec.EncryptedDocumentEntries(); len(hidden) > 0 {
				return fmt.Errorf("%w\n  the encrypted %s may supply it, and this command decrypts nothing — plan the deploy, which decrypts as it stages",
					err, strings.Join(hidden, ", "))
			}
			return err
		}
		return fmt.Errorf("the generated runtime did not parse as Compose — this is an Onebox bug: %w", err)
	}
	lp.compose, lp.unresolved, lp.composeBytes = p, rendered.Unresolved, rendered.Bytes
	return nil
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
