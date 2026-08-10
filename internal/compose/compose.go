// Package compose parses Compose through compose-go — the same loader docker
// compose v2 uses — so the supported dialect is exactly what Compose accepts;
// Onebox does not reimplement the Compose specification.
//
// What it parses is Onebox's own generated runtime, not a file the user wrote.
// The engine works in services and images, and parsing our own output is the
// cheapest way to keep one definition of what a service is.
//
// It infers nothing — not which service is the application, which is a
// database, nor what order they start in. The project states all of it, because
// a guess that disagrees with the author is worse than a question.
package compose

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"

	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	"strings"
)

var ident = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

func Load(ctx context.Context, composePath, projectName string, envFiles ...string) (*types.Project, error) {
	return load(ctx, composePath, projectName, envFiles...)
}

func load(ctx context.Context, composePath, projectName string, envFiles ...string) (*types.Project, error) {
	fns := []cli.ProjectOptionsFn{
		cli.WithName(projectName),
		cli.WithWorkingDirectory(filepath.Dir(composePath)),
		cli.WithOsEnv,
		// Onebox models the complete production contract. Profile-gated jobs and
		// maintenance services still need classification and may be explicitly
		// targeted by the engine, so load every declared profile.
		cli.WithProfiles([]string{"*"}),
		// WithEnvFiles(envFiles...) feeds ${VAR} INTERPOLATION (image tags, etc.)
		// from the config's env_files; with none it falls back to <project-dir>/.env.
		// WithDotEnv reads them. Order matters: os env is merged first and wins
		// (compose semantics), and later env files override earlier ones.
		cli.WithEnvFiles(envFiles...),
		cli.WithDotEnv,
		// Do NOT fold `env_file:` into each service's `environment:` map. That
		// folding would inline the entire secret env file into the rendered
		// compose (and thus the plan diff/artifact), violating "secrets content
		// never logged". Interpolation of ${VAR} in the Compose
		// file itself is unaffected; env_file references survive and are shipped
		// as a mode-600 payload file that `docker compose` reads at runtime.
		cli.WithoutEnvironmentResolution,
	}
	opts, err := cli.NewProjectOptions([]string{composePath}, fns...)
	if err != nil {
		return nil, err
	}
	p, err := opts.LoadProject(ctx)
	if err != nil {
		return nil, fmt.Errorf("compose load %s: %w", composePath, err)
	}
	return p, nil
}

// LoadBytes parses a Compose document already in memory. Onebox generates its
// runtime rather than reading one, so there is no file to point the loader at
// — and writing a temporary one into the user's repository to read it straight
// back would be a worse answer than passing the bytes.
//
// dir anchors relative paths (build contexts, env files) exactly as the
// project file's directory does elsewhere.
// env supplies `${VAR}` expressions carried in verbatim from a Compose source
// the project referenced. It is deliberately explicit rather than inherited:
// the runner's own environment is a developer's laptop, and a document that
// parsed differently there than on the target would be the worst kind of
// difference — invisible until it deployed.
func LoadBytes(ctx context.Context, content []byte, projectName, dir string, env map[string]string) (*types.Project, error) {
	if env == nil {
		env = map[string]string{}
	}
	p, err := loader.LoadWithContext(ctx, types.ConfigDetails{
		WorkingDir:  dir,
		ConfigFiles: []types.ConfigFile{{Filename: "compose.yaml", Content: content}},
		Environment: env,
	}, func(o *loader.Options) {
		o.SetProjectName(projectName, true)
		o.SkipResolveEnvironment = true
		// Jobs live behind the generated `job` profile so `compose up` never
		// starts them accidentally. Planning and targeted execution still need
		// those services in the parsed runtime; otherwise their images cannot be
		// pinned and `compose run <job>` has no service to invoke.
		o.Profiles = []string{"*"}
	})
	if err != nil {
		// An unsatisfied variable is the author's, not ours: it comes from a
		// Compose file they referenced, and the caller would otherwise report
		// it as an Onebox bug in generated output.
		if strings.Contains(err.Error(), "interpolating") || strings.Contains(err.Error(), "required variable") {
			return nil, &InterpolationError{err: err}
		}
		return nil, fmt.Errorf("compose parse: %w", err)
	}
	return p, nil
}

// InterpolationError is a `${VAR}` a referenced Compose source needs and the
// project's environment files do not supply.
type InterpolationError struct{ err error }

func (e *InterpolationError) Error() string {
	return "a referenced Compose file needs a variable the project does not supply: " + e.err.Error() +
		"\n  declare it in one of runtime.env_files, which is what feeds interpolation"
}
func (e *InterpolationError) Unwrap() error { return e.err }
