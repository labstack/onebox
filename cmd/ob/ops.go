package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/onebox"
	"github.com/labstack/onebox/internal/shellquote"
)

func addOpsCommands(root *cobra.Command, g *globalFlags) {
	// supporting/data service apply
	serviceCmd := &cobra.Command{Use: "service", Short: "manage supporting and data services",
		Long: "Manage the supporting services this project declares.\n\n" +
			"They run in their own Compose projects, so no deploy and no rollback can\n" +
			"stop them or remove their volumes. `apply` converges them to what the\n" +
			"project declares; a major version change a driver cannot perform in place\n" +
			"is refused rather than attempted.",
		Args: cobra.NoArgs, RunE: showCommandHelp}
	var serviceBreakLock, allowDestructiveMounts bool
	serviceApplyCmd := &cobra.Command{
		Use:   "apply",
		Short: "planned service convergence — diff shown, destructive mounts refused without --allow-destructive-mounts",
		Long:  "Converge the supporting services to what the project declares.\n\nEach runs in its own Compose project, so this never touches a release and a\nrelease never touches it. A change a driver can apply in place is applied; a\nmajor version change it cannot — a data directory the new version could not\nopen — is refused with what to do instead, rather than replacing the\ncontainer and leaving the data intact and unreachable.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runMutation(cmd, g, onebox.ExecuteRequest{
				Kind: onebox.KindServiceApply, BreakLock: serviceBreakLock, AllowDestructiveMounts: allowDestructiveMounts,
			}, "service apply")
		},
	}
	serviceApplyCmd.Flags().BoolVar(&serviceBreakLock, "break-lock", false, "break a stale operation lock after inspecting its holder")
	serviceApplyCmd.Flags().BoolVar(&allowDestructiveMounts, "allow-destructive-mounts", false, "permit only the mount detachments named in the service plan")
	serviceCmd.AddCommand(serviceApplyCmd)
	root.AddCommand(serviceCmd)

	// proxy apply — converge the host-scoped proxy for the host's sole app owner.
	proxyCmd := &cobra.Command{Use: "proxy", Short: "manage the host-scoped proxy (proxy.managed: true)",
		Long: "Manage the host-scoped proxy owned by this host's sole Onebox application.\n\n" +
			"The proxy outlives application releases and holds the public ports and\n" +
			"certificates. A different application identity is refused before mutation.",
		Args: cobra.NoArgs, RunE: showCommandHelp}
	var proxyBreakLock bool
	proxyApplyCmd := &cobra.Command{
		Use:   "apply",
		Short: "converge the host proxy — diff shown; unchanged config never touches the container (ACME-safe)",
		Long:  "Reconfigure the host proxy from the sole owning application's routes.\n\nTaken under the host lock because the proxy belongs to the box rather than to\na release. Cross-application route merging is not supported.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runMutation(cmd, g, onebox.ExecuteRequest{
				Kind: onebox.KindProxyApply, BreakLock: proxyBreakLock,
			}, "proxy apply")
		},
	}
	proxyApplyCmd.Flags().BoolVar(&proxyBreakLock, "break-lock", false, "break a stale host lock after inspecting its holder")
	proxyCmd.AddCommand(proxyApplyCmd)
	root.AddCommand(proxyCmd)

	// secrets list | edit | push
	secretsCmd := &cobra.Command{Use: "secrets", Short: "SOPS-encrypted secrets",
		Long: "SOPS-encrypted secrets for this project.\n\n" +
			"`list` names every value-free declaration and its stable entry ID. `edit`\n" +
			"decrypts the selected source to a temporary file, opens an editor, and re-encrypts.\n" +
			"`push` renders the decrypted values into the current release on the host\n" +
			"and restarts what reads them. Plaintext never enters the project file, the\n" +
			"generated runtime, or any plan.",
		Args: cobra.NoArgs, RunE: showCommandHelp}
	secretsCmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "list value-free secret declarations and stable entry IDs",
		Long:  "List the encrypted secret declarations active in the selected environment.\n\nThe result contains stable entry IDs, source paths, scopes, output paths, and\naffected workloads, but never decrypted values. Pass an ID to `ob secrets edit`\nwhen more than one editable source exists.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			resolved, err := loadResolvedConfigOnly(g)
			if err != nil {
				return writeStructuredReadFailure(cmd, g, err)
			}
			entries := resolved.SecretDeclarationGraph()
			if isStructuredOutput(g) {
				return writeFiniteSuccess(cmd, g, map[string]any{"entries": entries})
			}
			if len(entries) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no encrypted secret declarations")
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tSOURCE\tSCOPE\tWORKLOADS")
			for _, entry := range entries {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", entry.ID, entry.SourceFile, entry.Scope, strings.Join(entry.AffectedWorkloads, ","))
			}
			return w.Flush()
		},
	})
	secretsCmd.AddCommand(&cobra.Command{
		Use:   "edit [entry-id]",
		Short: "open one encrypted source in $EDITOR via sops",
		Long:  "Decrypt the secrets file, open it in $EDITOR, and re-encrypt on save.\n\nPlaintext exists only in a temporary file for the life of the editor. It\nnever enters the project file, the generated runtime, or any plan.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resolved, err := loadResolvedConfigOnly(g)
			if err != nil {
				return writeStructuredReadFailure(cmd, g, err)
			}
			entryID := ""
			if len(args) == 1 {
				entryID = args[0]
			}
			declaration, err := editableSecretDeclaration(resolved, entryID)
			if err != nil {
				return writeStructuredCommandFailure(cmd, g, "secret_entry_not_selected", "select one editable secret declaration", err)
			}
			if declaration.SourceFile == "" {
				err := fmt.Errorf("no encrypted env_files entry declared in %s — add one as {file: <path>, provider: sops}", g.ConfigPath)
				return writeStructuredCommandFailure(cmd, g, "sops_source_missing", "no SOPS-encrypted environment file is declared", err)
			}
			path := filepath.Join(filepath.Dir(g.ConfigPath), declaration.SourceFile)
			c := exec.CommandContext(cmd.Context(), "sops", path)
			c.Stdin = cmd.InOrStdin()
			if isStructuredOutput(g) {
				// The editor owns its terminal, but the command's stdout remains one
				// parseable JSON document.
				c.Stdout, c.Stderr = cmd.ErrOrStderr(), cmd.ErrOrStderr()
			} else {
				c.Stdout, c.Stderr = cmd.OutOrStdout(), cmd.ErrOrStderr()
			}
			err = c.Run()
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) && exitErr.ExitCode() == 200 {
				if isStructuredOutput(g) {
					return writeFiniteNoOp(cmd, g, map[string]any{"path": path, "changed": false})
				}
				return nil
			}
			if err != nil {
				return writeStructuredCommandFailure(cmd, g, "sops_failed", "SOPS did not complete successfully", err)
			}
			if isStructuredOutput(g) {
				return writeFiniteSuccess(cmd, g, map[string]any{"path": path, "changed": true})
			}
			return nil
		},
	})
	secretsCmd.AddCommand(&cobra.Command{
		Use:   "push",
		Short: "re-render secrets into the live release and restart workloads if changed",
		Long: "Render the complete decrypted secret graph into the current release on the host and\n" +
			"replace every workload that reads the declared secret graph when any value changes.\n\n" +
			"The payload is uploaded and compared on the host even for a no-op; unchanged values do\n" +
			"not replace or restart workloads. Refused when secret declarations differ from the\n" +
			"deployed release, or when that release predates opaque secret generations. In either\n" +
			"case, deploy first.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runMutation(cmd, g, onebox.ExecuteRequest{Kind: onebox.KindSecretsPush}, "secrets push")
		},
	})
	root.AddCommand(secretsCmd)

	// destroy
	var destroyVolumes, destroyProxy bool
	destroyCmd := &cobra.Command{
		Use:   "destroy",
		Short: "tear the app down (typed confirmation; volumes kept unless --volumes)",
		Long:  "Tear the application down: every container it owns, its scheduled timers, and\nits state directory.\n\nRequires the application name typed back. Volumes are kept unless --volumes,\nand when they are kept the service credentials are kept with them — a volume\nwhose credential is gone cannot be opened by a new one. The host proxy survives\nunless --proxy is supplied.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc := operationsService(cmd, g)
			binding, err := svc.ResolveExecutionBinding(cmd.Context(), onebox.KindDestroy)
			if err != nil {
				return writeEarlyOperationFailure(cmd, g, err)
			}
			cfg, err := notificationConfig(g)
			if err != nil {
				return writeEarlyOperationFailure(cmd, g, err)
			}
			if cfg.Name != binding.Application {
				return writeEarlyOperationFailure(cmd, g, fmt.Errorf("configuration changed while preparing destroy — retry"))
			}
			promptOut := cmd.OutOrStdout()
			if isStructuredOutput(g) {
				promptOut = cmd.ErrOrStderr()
			}
			fmt.Fprintf(promptOut, "This tears down every %s container \u2014 workloads, supporting services and scheduled timers \u2014 and ob's state dir on %s.\nVolumes are %s.\nType the app name (%s) to confirm: ",
				binding.Application, binding.Environment, map[bool]string{true: "REMOVED — data loss", false: "kept"}[destroyVolumes], binding.Application)
			line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
			if strings.TrimSpace(line) != binding.Application {
				return writeCancelled(cmd, g, "confirmation mismatch; nothing was changed")
			}
			return runMutation(cmd, g, onebox.ExecuteRequest{
				Kind: onebox.KindDestroy, RemoveVolumes: destroyVolumes, RemoveProxy: destroyProxy,
				ExpectedBinding: &binding,
			}, "destroy")
		},
	}
	destroyCmd.Flags().BoolVar(&destroyVolumes, "volumes", false, "also remove named volumes (DATA LOSS)")
	destroyCmd.Flags().BoolVar(&destroyProxy, "proxy", false, "also remove this host's managed proxy")
	root.AddCommand(destroyCmd)

	// logs
	var follow bool
	var tail int
	logsCmd := &cobra.Command{
		Use:   "logs <workload|service>",
		Short: "compose logs from the current release",
		Long:  "Stream logs from one workload or Onebox-run supporting service. Reads only.\n\nLog bytes are operator-controlled and may contain secrets; Onebox does not claim\nto redact passthrough output.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if follow && g.Output == "json" {
				err := errors.New("--follow cannot be represented as one JSON document; use --output ndjson")
				publicErr := &cliPublicError{
					Code: "output_mode_incompatible", SafeMessage: safeMessageForCode("output_mode_incompatible", "follow mode requires streaming output"),
					// Must satisfy SafeGuidanceCommand: one placeholder hole,
					// no alternation. An agent is told this is safe to run.
					NextCommand: "ob logs <workload> --follow --output ndjson",
				}
				if writeErr := writeFiniteOutcome(cmd, g, cliOutcomeError, nil, publicErr); writeErr != nil {
					return writeErr
				}
				return withExitCode(err, 1)
			}
			cfg, p, err := loadAllLenient(cmd.Context(), g)
			if err != nil {
				return writeStructuredReadFailure(cmd, g, err)
			}
			e, cleanup, err := connect(cmd, g, cfg, p, newUI(cmd, g))
			if err != nil {
				return writeStructuredReadFailure(cmd, g, err)
			}
			defer cleanup()
			target, err := e.ResolveRuntimeTarget(args[0])
			if err != nil {
				return writeStructuredCommandFailure(cmd, g, "logs_failed", "logs could not be read", err)
			}
			if g.Output == "json" {
				var stdout, stderr bytes.Buffer
				err = e.Logs(cmd.Context(), args[0], false, tail, &stdout, &stderr)
				data := map[string]any{
					"target": target, "stdout": stdout.String(), "stderr": stderr.String(),
					"passthrough_unredacted": true,
				}
				if err != nil {
					publicErr := publicError(err, "logs_failed", "logs could not be read")
					publicErr.Details = data
					if writeErr := writeFiniteOutcome(cmd, g, cliOutcomeError, nil, publicErr); writeErr != nil {
						return writeErr
					}
					return withExitCode(err, 1)
				}
				return writeFiniteSuccess(cmd, g, data)
			}
			if g.Output == "ndjson" {
				stream := newCLIRecordStream(cmd.OutOrStdout(), commandName(cmd))
				err = e.Logs(cmd.Context(), args[0], follow, tail, stream.channelWriter("stdout"), stream.channelWriter("stderr"))
				data := map[string]any{"target": target, "passthrough_unredacted": true}
				if err != nil {
					if writeErr := stream.terminal(cliOutcomeError, nil, publicError(err, "logs_failed", "logs could not be read")); writeErr != nil {
						return writeErr
					}
					return withExitCode(err, 1)
				}
				return stream.terminal(cliOutcomeSuccess, data, nil)
			}
			return e.Logs(cmd.Context(), args[0], follow, tail, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	logsCmd.Flags().BoolVarP(&follow, "follow", "f", false, "stream")
	logsCmd.Flags().IntVar(&tail, "tail", 100, "lines per service")
	root.AddCommand(logsCmd)

	// exec
	var execReason string
	execCmd := &cobra.Command{
		Use:   "exec <workload|service> -- <command...>",
		Short: "run a command inside a workload or service container",
		Long:  "Run a command inside a running container.\n\nNames a workload or a supporting service. This is an escape hatch: it does not\nclaim convergence, rollback, idempotence, or output redaction. Onebox journals\nthe reason, target, target kind, operator, outcome, and command digest — never\nthe command bytes or passthrough output.\n\nArguments are passed as a literal vector, so a shell metacharacter is not\ninterpreted: write `ob exec --reason 'inspect queue' web -- sh -c 'a && b'`\nrather than passing the pipeline as one word. Reasons are durable metadata; do\nnot put credentials or other sensitive values in them.",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc := operationsService(cmd, g)
			request := onebox.ExecRequest{Target: args[0], Command: execCommand(args[1:]), Reason: execReason}
			if g.Output == "ndjson" {
				stream := newCLIRecordStream(cmd.OutOrStdout(), commandName(cmd))
				result, err := svc.Exec(cmd.Context(), request, stream.channelWriter("stdout"), stream.channelWriter("stderr"))
				data := map[string]any{"invocation": result, "passthrough_unredacted": true}
				if err != nil {
					publicErr := publicError(err, "exec_failed", "command could not be executed")
					publicErr.Details = data
					outcome := cliOutcomeError
					exitCode := 1
					if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
						outcome, exitCode = cliOutcomeCancelled, 2
					}
					if writeErr := stream.terminal(outcome, nil, publicErr); writeErr != nil {
						return writeErr
					}
					return withExitCode(err, exitCode)
				}
				return stream.terminal(cliOutcomeSuccess, data, nil)
			}
			_, err := svc.Exec(cmd.Context(), request, cmd.OutOrStdout(), cmd.ErrOrStderr())
			return err
		},
	}
	execCmd.Flags().StringVar(&execReason, "reason", "", "single-line operational justification (max 256 bytes; journaled)")
	_ = execCmd.MarkFlagRequired("reason")
	root.AddCommand(execCmd)
}

// execCommand rebuilds an argument vector for the single `sh -c` the remote
// side runs it through.
//
// Joining argv with spaces let that shell re-split it: `ob exec web -- sh -c
// 'echo one; echo two'` arrived as `sh -c echo one; echo two`, so echo ran with
// no arguments and "one" became the shell's $0. Quoting each element means the
// remote shell rebuilds exactly what was typed — and, deliberately, that a
// single argument carrying shell metacharacters is now one literal word.
func execCommand(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, shellquote.Quote(arg))
	}
	return strings.Join(quoted, " ")
}

// loadConfigOnly skips compose loading and inference (secrets edit needs no
// host or compose — only cfg.Secrets). CUE validation already ran in Load;
// component/order checks in Validate can't run without the compose project.
func loadConfigOnly(g *globalFlags) (*app.Spec, error) {
	return app.Load(g.ConfigPath)
}

func loadResolvedConfigOnly(g *globalFlags) (*app.Resolved, error) {
	spec, err := loadConfigOnly(g)
	if err != nil {
		return nil, err
	}
	return spec.Resolve(g.Env)
}

func editableSecretDeclaration(spec *app.Resolved, entryID string) (app.SecretDeclaration, error) {
	var editable []app.SecretDeclaration
	for _, declaration := range spec.SecretDeclarationGraph() {
		if declaration.Provider != "sops" || declaration.SourceFile == "" {
			continue
		}
		if entryID != "" && declaration.ID == entryID {
			return declaration, nil
		}
		editable = append(editable, declaration)
	}
	if entryID != "" {
		return app.SecretDeclaration{}, fmt.Errorf("secret entry %q is not editable in environment %q — run `ob secrets list`", entryID, spec.Env)
	}
	if len(editable) == 1 {
		return editable[0], nil
	}
	if len(editable) > 1 {
		return app.SecretDeclaration{}, fmt.Errorf("%d editable secret declarations exist — run `ob secrets list`, then `ob secrets edit <entry-id>`", len(editable))
	}
	return app.SecretDeclaration{}, nil
}

// specSopsSource remains the doctor probe for whether the project-level default
// declares at least one SOPS input; editor selection uses the resolved graph.
func specSopsSource(spec *app.Spec) string {
	if spec.Runtime == nil {
		return ""
	}
	for _, entry := range spec.Runtime.EnvFiles {
		if entry.Provider == "sops" {
			return entry.File
		}
	}
	return ""
}
