// Package compose parses Compose through compose-go — the same loader docker
// compose v2 uses — so the supported dialect is exactly what Compose accepts;
// Onebox does not reimplement the Compose specification.
//
// What it parses is now Onebox's own generated runtime rather than a file the
// user wrote. The engine works in services and images, and parsing our own
// output is the cheapest way to keep one definition of what a service is. The
// inference that used to live here — guess which service is the application,
// which is a database, what order they start in — is gone: the project states
// all of it, and a guess that disagrees with the author is worse than a
// question.
package compose

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"

	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/template"
	"github.com/compose-spec/compose-go/v2/types"
)

var ident = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

func Load(ctx context.Context, composePath, projectName string, envFiles ...string) (*types.Project, error) {
	return load(ctx, composePath, projectName, false, envFiles...)
}

// LoadLenient loads for READ-ONLY verbs (status, logs, exec, audit): they
// never consume interpolated values, so a missing required variable
// (`${VAR:?...}`) must not block a query — it resolves to the visible
// placeholder `${VAR}` instead of an error. Deploy-path verbs keep the strict
// contract via Load.
func LoadLenient(ctx context.Context, composePath, projectName string, envFiles ...string) (*types.Project, error) {
	return load(ctx, composePath, projectName, true, envFiles...)
}

func load(ctx context.Context, composePath, projectName string, lenient bool, envFiles ...string) (*types.Project, error) {
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
	if lenient {
		fns = append(fns, cli.WithLoadOptions(func(o *loader.Options) {
			// this mutator also runs for LoadConfigFiles' zero Options (no
			// interpolation there) — only wrap the real interpolation pass
			if o.Interpolate == nil {
				return
			}
			// Per-VARIABLE leniency via an overlay loop: substitute strictly;
			// when a ${VAR:?}/${VAR?} requirement fails (unset OR set-but-empty
			// — both error), overlay exactly that variable with the visible
			// placeholder ${VAR} and retry. Every other variable keeps exact
			// compose semantics (:- defaults, :+ presence, nested defaults) —
			// a whole-string fallback mapping would corrupt them, and the
			// template regex is too greedy for per-match interception (a
			// default value swallows following ${...} into one match).
			o.Interpolate.Substitute = func(s string, m template.Mapping) (string, error) {
				overlay := map[string]string{}
				wrapped := func(key string) (string, bool) {
					if v, ok := overlay[key]; ok {
						return v, true
					}
					return m(key)
				}
				for {
					out, err := template.Substitute(s, wrapped)
					if err == nil {
						return out, nil
					}
					var missing *template.MissingRequiredError
					if errors.As(err, &missing) {
						if _, seen := overlay[missing.Variable]; !seen {
							overlay[missing.Variable] = "${" + missing.Variable + "}"
							continue // one failing variable per pass; loop is bounded by distinct vars
						}
					}
					return out, err // non-required error, or no progress
				}
			}
		}))
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
func LoadBytes(ctx context.Context, content []byte, projectName, dir string) (*types.Project, error) {
	p, err := loader.LoadWithContext(ctx, types.ConfigDetails{
		WorkingDir:  dir,
		ConfigFiles: []types.ConfigFile{{Filename: "compose.yaml", Content: content}},
	}, func(o *loader.Options) {
		o.SetProjectName(projectName, true)
		o.SkipResolveEnvironment = true
	})
	if err != nil {
		return nil, fmt.Errorf("compose parse: %w", err)
	}
	return p, nil
}
