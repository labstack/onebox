package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/labstack/onebox/internal/buildinfo"
	"github.com/labstack/onebox/internal/onebox"
)

type versionReport struct {
	buildinfo.Info
	SupportedExecutablePlanSchemas []string `json:"supported_executable_plan_schemas"`
}

func currentVersionReport() versionReport {
	return versionReport{
		Info:                           buildinfo.Read(),
		SupportedExecutablePlanSchemas: onebox.SupportedExecutableDeployPlanSchemas(),
	}
}

func addVersionCommand(root *cobra.Command, g *globalFlags) {
	cmd := &cobra.Command{
		Use:   "version",
		Short: "print version and build provenance",
		Long:  "Print the version and build provenance of this binary.\n\nEnvironment policy can require a released runner, so a commit-derived or\ndirty build is reported as such rather than as a version.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			report := currentVersionReport()
			if isStructuredOutput(g) {
				return writeFiniteSuccess(cmd, g, report)
			}
			return writeVersion(cmd.OutOrStdout(), report)
		},
	}
	root.AddCommand(cmd)
}

func writeVersion(out io.Writer, report versionReport) error {
	fmt.Fprintf(out, "ob version %s\n", report.Version)
	fmt.Fprintf(out, "vcs revision: %s\n", known(report.VCSRevision))
	fmt.Fprintf(out, "dirty: %t\n", report.Dirty)
	fmt.Fprintf(out, "vcs time: %s\n", known(report.VCSTime))
	fmt.Fprintf(out, "build time: %s\n", known(report.BuildTime))
	fmt.Fprintf(out, "go version: %s\n", known(report.GoVersion))
	fmt.Fprintln(out, "supported executable plan schemas:")
	for _, schema := range report.SupportedExecutablePlanSchemas {
		fmt.Fprintf(out, "  - %s\n", schema)
	}
	return nil
}

func known(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}

func formatRunnerProvenance(runner buildinfo.Runner) string {
	revision := strings.TrimSpace(runner.VCSRevision)
	if revision == "" {
		revision = "unknown"
	} else if len(revision) > 12 {
		revision = revision[:12]
	}
	if runner.Dirty {
		revision += "+dirty"
	}
	return fmt.Sprintf("ob %s (%s)", runner.Version, revision)
}
