package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/labstack/onebox/internal/app"
)

// `ob preview` renders the declarative contract. It is deliberately separate
// from `ob render`, which still serves the classifier contract the engine
// executes today; at the cutover this becomes `render` and the other goes.
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

			images := app.Images{}
			for _, pair := range imageFlags {
				name, ref, ok := strings.Cut(pair, "=")
				if !ok {
					return fmt.Errorf("--image expects workload=reference, got %q", pair)
				}
				images[name] = ref
			}

			if release == "" {
				release = "preview"
			}
			rendered, err := p.Render(g.Env, release, images)
			if err != nil {
				return explain(err)
			}

			out := cmd.OutOrStdout()
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
			_, err = out.Write(body)
			return err
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
	var doc yaml.Node
	if err := yaml.Unmarshal(in, &doc); err != nil {
		return nil, fmt.Errorf("cannot redact preview: %w", err)
	}
	redactNode(&doc, false)
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

func redactNode(n *yaml.Node, inEnv bool) {
	if n == nil {
		return
	}
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, val := n.Content[i], n.Content[i+1]
			if inEnv && val.Kind == yaml.ScalarNode {
				val.Value = "«redacted»"
				val.Tag = "!!str"
				val.Style = 0
				continue
			}
			redactNode(val, key.Value == "environment")
		}
		return
	}
	for _, c := range n.Content {
		redactNode(c, inEnv)
	}
}
