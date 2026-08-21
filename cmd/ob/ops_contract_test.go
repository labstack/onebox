package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/transport"
)

func writeOpsContractProject(t *testing.T, dir string, encrypted bool) string {
	t.Helper()
	runtime := ""
	if encrypted {
		runtime = "runtime:\n  env_files: [{file: secrets.env, provider: sops}]\n"
		if err := os.WriteFile(filepath.Join(dir, "secrets.env"), []byte("encrypted-placeholder\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "project.yml")
	if err := os.WriteFile(path, []byte(`api_version: onebox.run/v1
app: shop
environments:
  production: {server: deploy@example.invalid}
`+runtime+`image: nginx
`), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSecretsEditExit200IsJSONNoOp(t *testing.T) {
	dir := t.TempDir()
	config := writeOpsContractProject(t, dir, true)
	sops := filepath.Join(dir, "sops")
	if err := os.WriteFile(sops, []byte("#!/bin/sh\nexit 200\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var stdout, stderr bytes.Buffer
	root := newRootCmd()
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"--config", config, "--output", "json", "secrets", "edit"})
	if err := root.Execute(); err != nil {
		t.Fatalf("secrets edit: %v\nstderr: %s", err, stderr.String())
	}
	var envelope cliEnvelope
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("decode editor result: %v\n%s", err, stdout.String())
	}
	if envelope.Command != "ob secrets edit" || envelope.Outcome != cliOutcomeNoOp || envelope.Error != nil {
		t.Fatalf("editor result = %+v", envelope)
	}
}

func TestSecretsListReturnsStableValueFreeEntries(t *testing.T) {
	dir := t.TempDir()
	config := writeOpsContractProject(t, dir, true)
	var stdout bytes.Buffer
	root := newRootCmd()
	root.SetOut(&stdout)
	root.SetArgs([]string{"--config", config, "--output", "json", "secrets", "list"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Data struct {
			Entries []struct {
				ID         string `json:"id"`
				SourceFile string `json:"source_file"`
			}
		} `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Data.Entries) != 1 || !strings.HasPrefix(envelope.Data.Entries[0].ID, "secret_") || envelope.Data.Entries[0].SourceFile != "secrets.env" {
		t.Fatalf("entries = %+v", envelope.Data.Entries)
	}
}

func TestSecretsEditRequiresIDWhenSeveralEntriesExist(t *testing.T) {
	dir := t.TempDir()
	config := writeOpsContractProject(t, dir, true)
	data, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte("runtime:\n  env_files: [{file: secrets.env, provider: sops}]"), []byte("runtime:\n  env_files: [{file: secrets.env, provider: sops}, {file: other.env, provider: sops}]"), 1)
	if err := os.WriteFile(config, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "other.env"), []byte("encrypted-placeholder\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := newRootCmd()
	root.SetArgs([]string{"--config", config, "secrets", "edit"})
	err = root.Execute()
	if err == nil || !strings.Contains(err.Error(), "ob secrets edit <entry-id>") {
		t.Fatalf("ambiguous edit = %v", err)
	}
}

func TestExecMissingReasonFailsBeforeTargetContact(t *testing.T) {
	previousConnector := cliConnector
	contacts := 0
	cliConnector = func(context.Context, string) (transport.Transport, error) {
		contacts++
		return nil, errors.New("target must not be contacted")
	}
	t.Cleanup(func() { cliConnector = previousConnector })

	root := newRootCmd()
	root.SetArgs([]string{"exec", "web", "--", "true"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "required flag") {
		t.Fatalf("missing reason = %v", err)
	}
	if contacts != 0 {
		t.Fatalf("missing reason contacted target %d times", contacts)
	}
}

func TestDestroyConfirmationMismatchIsCancelledBeforeTargetContact(t *testing.T) {
	dir := t.TempDir()
	config := writeOpsContractProject(t, dir, false)
	previousConnector := cliConnector
	contacts := 0
	cliConnector = func(context.Context, string) (transport.Transport, error) {
		contacts++
		return nil, errors.New("target must not be contacted")
	}
	t.Cleanup(func() { cliConnector = previousConnector })

	var stdout, stderr bytes.Buffer
	root := newRootCmd()
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetIn(strings.NewReader("wrong\n"))
	root.SetArgs([]string{"--config", config, "--output", "json", "destroy"})
	err := root.Execute()
	if err == nil {
		t.Fatal("confirmation mismatch returned success")
	}
	var coded interface{ ExitCode() int }
	if !errors.As(err, &coded) || coded.ExitCode() != 2 {
		t.Fatalf("destroy cancellation = %v, want exit 2", err)
	}
	if contacts != 0 {
		t.Fatalf("destroy contacted target %d times before confirmation", contacts)
	}
	var envelope cliEnvelope
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("decode cancellation: %v\n%s", err, stdout.String())
	}
	if envelope.Command != "ob destroy" || envelope.Outcome != cliOutcomeCancelled || envelope.Error == nil || envelope.Error.Code != "cancelled" {
		t.Fatalf("destroy result = %+v", envelope)
	}
}

func TestServiceLogsAndExecNDJSONTagChannelsAndTargetKind(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "project.yml")
	if err := os.WriteFile(config, []byte(`api_version: onebox.run/v1
app: shop
environments:
  production: {server: deploy@example.invalid}
image: nginx
services: {postgres: 17}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &transport.Fake{HostName: "example.invalid", Dynamic: func(command string) (transport.Result, bool) {
		switch {
		case strings.HasPrefix(command, ": ob-epoch-probe;"):
			return transport.Result{ExitCode: app.ProbeAbsent}, true
		case strings.Contains(command, "/_host/owner"):
			return transport.Result{Stdout: "shop\n"}, true
		case strings.Contains(command, " logs "):
			return transport.Result{Stdout: "log-secret\n", Stderr: "log-warning\n"}, true
		case strings.Contains(command, "docker ps"):
			return transport.Result{Stdout: "PG1\n"}, true
		case strings.Contains(command, "docker exec"):
			return transport.Result{Stdout: "exec-secret\n", Stderr: "exec-warning\n"}, true
		default:
			return transport.Result{}, true
		}
	}}
	previousConnector := cliConnector
	cliConnector = func(context.Context, string) (transport.Transport, error) { return fake, nil }
	t.Cleanup(func() { cliConnector = previousConnector })

	for name, args := range map[string][]string{
		"logs": {"--config", config, "--output", "ndjson", "logs", "postgres"},
		"exec": {"--config", config, "--output", "ndjson", "exec", "--reason", "verify database client", "postgres", "--", "psql", "--version"},
	} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			root := newRootCmd()
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			root.SetArgs(args)
			if err := root.Execute(); err != nil {
				t.Fatalf("%s: %v\nstderr: %s", name, err, stderr.String())
			}
			lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
			if len(lines) != 3 {
				t.Fatalf("%s emitted %d records:\n%s", name, len(lines), stdout.String())
			}
			channels := map[string]bool{}
			terminals := 0
			for index, line := range lines {
				var record cliRecord
				if err := json.Unmarshal([]byte(line), &record); err != nil {
					t.Fatalf("decode %s record: %v", name, err)
				}
				if record.Sequence != uint64(index+1) {
					t.Fatalf("%s sequence = %d at %d", name, record.Sequence, index)
				}
				if record.Kind == "output" {
					channels[record.Channel] = true
				}
				if record.Kind == "terminal" {
					terminals++
					if record.Outcome != cliOutcomeSuccess {
						t.Fatalf("%s terminal = %+v", name, record)
					}
					encoded, _ := json.Marshal(record.Data)
					if !strings.Contains(string(encoded), `"kind":"service"`) {
						t.Fatalf("%s terminal lacks service kind: %s", name, encoded)
					}
				}
			}
			if !channels["stdout"] || !channels["stderr"] || terminals != 1 {
				t.Fatalf("%s channels=%v terminals=%d", name, channels, terminals)
			}
		})
	}
	for _, command := range fake.Commands {
		if strings.Contains(strings.ToLower(command), "password") || strings.Contains(strings.ToLower(command), "credential") {
			t.Fatalf("runtime command contains credentials: %s", command)
		}
	}
}
