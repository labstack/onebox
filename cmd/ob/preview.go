package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/labstack/onebox/internal/app"
)

// `ob preview` renders what a release would put on the host: the application's
// runtime and every supporting service's own document.
//
// The services are printed too, because they are the part most worth reading —
// Onebox wrote all of it, and a preview that showed only what the author typed
// would hide exactly the decisions they did not make.
//
// It touches nothing: no target connection, no local state, no mutation.
func addPreviewCommand(root *cobra.Command, g *globalFlags) {
	var (
		release    string
		imageFlags []string
		digestOnly bool
		showRaw    bool
	)

	cmd := &cobra.Command{
		Use:   "preview",
		Short: "render the runtime the declarative contract generates (no target, no changes)",
		Long: "Load an onebox.run/v1 project, resolve the environment's overrides, and print\n" +
			"the Compose runtime Onebox would generate, with its content digest.\n\n" +
			"Nothing is contacted and nothing is written. Environment values are redacted:\n" +
			"a preview must never put a secret on a terminal.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := app.Load(g.ConfigPath)
			if err != nil {
				return explain(err)
			}

			images, err := parseImages(imageFlags)
			if err != nil {
				return err
			}

			if release == "" {
				release = "preview"
			}
			rendered, err := p.Render(g.Env, release, images)
			if err != nil {
				return explain(err)
			}

			out := cmd.OutOrStdout()

			if isStructuredOutput(g) {
				// The structured stream is the one that gets piped into a
				// file, a log or a CI artifact. It is always redacted, and
				// --raw is refused beside it rather than silently ignored:
				// a flag that appears to work and does not is worse than one
				// that says no.
				if showRaw {
					return fmt.Errorf("--raw cannot be combined with --output %s: "+
						"the structured stream is always redacted", g.Output)
				}
				body, err := redactEnvValues(rendered.Bytes)
				if err != nil {
					return err
				}
				services := map[string]string{}
				for _, name := range sortedServiceNames(rendered.Services) {
					doc, err := redactEnvValuesExcept(rendered.Services[name], p.ServicePublicEnv(name))
					if err != nil {
						return err
					}
					services[name] = string(doc)
				}
				return writeCLIJSON(out, cliPreviewEnvelope{
					SchemaVersion: cliPreviewSchemaVersion,
					Environment:   g.Env,
					Release:       release,
					Digest:        rendered.Digest,
					Redacted:      true,
					Runtime:       string(body),
					Services:      services,
				}, g.Output == "json")
			}

			if digestOnly {
				fmt.Fprintln(out, rendered.Digest)
				return nil
			}

			body := rendered.Bytes
			if !showRaw {
				if body, err = redactEnvValues(body); err != nil {
					return err
				}
			}
			fmt.Fprintf(out, "# digest %s\n", rendered.Digest)
			fmt.Fprintf(out, "# environment %s, release %s\n", g.Env, release)
			if !showRaw {
				fmt.Fprintln(out, "# environment values redacted; --raw shows them")
			}
			if _, err := out.Write(body); err != nil {
				return err
			}

			for _, name := range sortedServiceNames(rendered.Services) {
				doc := rendered.Services[name]
				if !showRaw {
					if doc, err = redactEnvValuesExcept(doc, p.ServicePublicEnv(name)); err != nil {
						return err
					}
				}
				fmt.Fprintf(out, "\n--- service %s: applied outside any release, in its own Compose\n", name)
				fmt.Fprintf(out, "--- project, so no deploy and no rollback can reach it.\n")
				if _, err := out.Write(doc); err != nil {
					return err
				}
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&release, "release", "", "release identity to stamp (default \"preview\")")
	cmd.Flags().StringArrayVar(&imageFlags, "image", nil,
		"resolved image for a build-sourced workload, as workload=reference (repeatable)")
	cmd.Flags().BoolVar(&digestOnly, "digest", false, "print only the content digest")
	cmd.Flags().BoolVar(&showRaw, "raw", false, "do not redact environment values")
	root.AddCommand(cmd)
}

// explain turns a typed loader failure into terminal output that says what is
// wrong, where, and what to run next — the same three things the structured
// mode carries, so an agent and a person read the same information.
func explain(err error) error {
	e, ok := err.(*app.Error)
	if !ok {
		return err
	}
	var b strings.Builder
	b.WriteString(e.Message)
	if e.Path != "" {
		fmt.Fprintf(&b, "\n  at: %s", e.Path)
	}
	if e.Next != "" {
		fmt.Fprintf(&b, "\n  try: %s", e.Next)
	}
	fmt.Fprintf(&b, "\n  code: %s", e.Code)
	return fmt.Errorf("%s", b.String())
}

// redactEnvValues replaces every environment value with a placeholder. A
// declared value is as likely to be a token as a log level, and a preview that
// leaks one is worse than a preview nobody runs.
func redactEnvValues(in []byte) ([]byte, error) {
	return redactEnvValuesExcept(in, nil)
}

// redactEnvValuesExcept keeps the named variables legible. It is used for the
// service documents, whose environment Onebox wrote itself; keep is never
// derived from anything the author typed.
func redactEnvValuesExcept(in []byte, keep []string) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(in, &doc); err != nil {
		return nil, fmt.Errorf("cannot redact preview: %w", err)
	}
	visible := map[string]bool{}
	for _, k := range keep {
		visible[k] = true
	}
	redactNode(&doc, false, visible)
	var sb strings.Builder
	enc := yaml.NewEncoder(&sb)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return []byte(sb.String()), nil
}

func redactNode(n *yaml.Node, inEnv bool, visible map[string]bool) {
	if n == nil {
		return
	}
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, val := n.Content[i], n.Content[i+1]
			if inEnv && val.Kind == yaml.ScalarNode {
				if visible[key.Value] {
					continue
				}
				val.Value = "«redacted»"
				val.Tag = "!!str"
				val.Style = 0
				continue
			}
			// "environment" is the generated runtime's key; "env" is the
			// project document's. The same rule covers both, so the canonical
			// form cannot be the one place a declared value escapes.
			redactNode(val, key.Value == "environment" || key.Value == "env", visible)
		}
		return
	}
	for _, c := range n.Content {
		redactNode(c, inEnv, visible)
	}
}

// sortedServiceNames keeps the printed order stable across runs.
func sortedServiceNames(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
