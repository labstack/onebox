package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestExecutionCommands(t *testing.T) {
	root := &cobra.Command{Use: "ob"}
	addExecutionCommands(root, &globalFlags{})
	for _, name := range []string{"list", "inspect", "resume", "abandon"} {
		t.Run(name, func(t *testing.T) {
			cmd, rest, err := root.Find([]string{"execution", name})
			if err != nil || len(rest) != 0 || cmd.Name() != name {
				t.Fatalf("command missing: %v %v", rest, err)
			}
			if cmd.RunE == nil {
				t.Fatal("missing implementation")
			}
			if name == "list" {
				if err := cmd.Args(cmd, nil); err != nil {
					t.Fatal(err)
				}
				if err := cmd.Args(cmd, []string{"unexpected"}); err == nil {
					t.Fatal("list accepted argument")
				}
				flag := cmd.Flags().Lookup("count")
				if flag == nil || flag.DefValue != "20" || flag.Shorthand != "n" {
					t.Fatalf("unexpected count flag: %#v", flag)
				}
			} else {
				if err := cmd.Args(cmd, nil); err == nil {
					t.Fatal("missing ID accepted")
				}
				if err := cmd.Args(cmd, []string{"id"}); err != nil {
					t.Fatal(err)
				}
				if err := cmd.Args(cmd, []string{"id", "extra"}); err == nil {
					t.Fatal("extra argument accepted")
				}
			}
			for _, flag := range []string{"input", "release", "job"} {
				if cmd.Flags().Lookup(flag) != nil {
					t.Fatalf("%s permits changing original %s", name, flag)
				}
			}
			if name == "resume" || name == "abandon" {
				if cmd.Flags().Lookup("break-lock") == nil {
					t.Fatal("missing break-lock")
				}
			}
			if (cmd.Flags().Lookup("wait") != nil) != (name == "resume") {
				t.Fatal("wait must apply only to resume")
			}
		})
	}
}

func TestExecutionListRejectsInvalidCountBeforeConnecting(t *testing.T) {
	for _, count := range []string{"0", "-1", "1001"} {
		t.Run(count, func(t *testing.T) {
			root := &cobra.Command{Use: "ob", SilenceErrors: true, SilenceUsage: true}
			addExecutionCommands(root, &globalFlags{})
			var output bytes.Buffer
			root.SetOut(&output)
			root.SetErr(&output)
			root.SetArgs([]string{"execution", "list", "--count", count})
			if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "between 1 and 1000") {
				t.Fatalf("got %v; output %s", err, output.String())
			}
		})
	}
}
