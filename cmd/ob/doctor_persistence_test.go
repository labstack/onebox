package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/buildinfo"
)

func doctorPersistenceProject(t *testing.T, services string) (string, *app.Spec) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "onebox.yml")
	manifest := `apiVersion: onebox.run/v1alpha2
kind: Application
metadata: {name: shop}
spec:
  environments: {production: {server: deploy@example.com}}
  workloads: {web: {image: 'nginx:1.27', strategy: Recreate}}
  services:
` + services
	if err := os.WriteFile(path, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := app.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, cfg
}

func TestDoctorServiceBackupsRespectAuthoredPersistence(t *testing.T) {
	for name, tc := range map[string]struct {
		services string
		status   doctorStatus
		checks   int
	}{
		"ephemeral": {"    cache: {driver: redis, version: '8', persistence: {mode: Ephemeral}}\n", doctorPass, 1},
		"durable":   {"    cache: {driver: redis, version: '8', persistence: {mode: Durable}}\n", doctorWarning, 1},
		"default":   {"    cache: {driver: redis, version: '8'}\n", doctorWarning, 1},
		"mixed":     {"    cache: {driver: redis, version: '8', persistence: {mode: Ephemeral}}\n    durable: {driver: valkey, version: '8'}\n", doctorWarning, 2},
	} {
		t.Run(name, func(t *testing.T) {
			path, cfg := doctorPersistenceProject(t, tc.services)
			report := inspectDoctorBackups(cfg, path, doctorTestDependencies(t))
			if report.Status != tc.status || len(report.Checks) != tc.checks {
				t.Fatalf("backups = %+v", report)
			}
			for _, check := range report.Checks {
				ephemeral := name == "ephemeral" || name == "mixed" && check.Workload == "cache"
				want := doctorWarning
				if ephemeral {
					want = doctorPass
					if !strings.Contains(check.Message, "declared ephemeral") || strings.Contains(check.Message, ".backup") {
						t.Fatalf("ephemeral service got misleading advice: %+v", check)
					}
				}
				if check.Status != want || check.Available || check.Mechanism != "backup" {
					t.Fatalf("service check = %+v, want %s without a backup", check, want)
				}
			}
			if name == "ephemeral" && strings.Contains(report.Message, "mechanisms are available") {
				t.Fatalf("summary claims an available backup: %s", report.Message)
			}
		})
	}
}

func TestDoctorStillReportsDeclaredServiceBackup(t *testing.T) {
	cfg := &app.Spec{Services: map[string]app.Service{
		"database": {Backup: &app.BackupPolicy{RecoveryKind: "pitr", Target: "offsite"}},
	}}
	report := inspectDoctorBackups(cfg, "onebox.yml", doctorTestDependencies(t))
	if report.Status != doctorPass || len(report.Checks) != 1 || !report.Checks[0].Available ||
		!strings.Contains(report.Checks[0].Message, "declares pitr backup to target offsite") ||
		!strings.Contains(report.Checks[0].Message, "ob backup status database") {
		t.Fatalf("declared backup = %+v", report)
	}
}

func TestDoctorEphemeralServicePassesHumanAndJSONReports(t *testing.T) {
	path, _ := doctorPersistenceProject(t, "    cache: {driver: redis, version: '8', persistence: {mode: Ephemeral}}\n")
	deps := doctorTestDependencies(t)
	current, err := deps.executable()
	if err != nil {
		t.Fatal(err)
	}
	deps.pathValue = filepath.Dir(current)
	deps.lookPath = func(name string) (string, error) {
		if name == "ob" {
			return current, nil
		}
		return "", errors.New("not found")
	}
	deps.inspectBinary = func(string) (buildinfo.Info, error) { return deps.runner.Info, nil }
	deps.loadConfig = app.Load
	previous := newDoctorDependencies
	newDoctorDependencies = func() doctorDependencies { return deps }
	t.Cleanup(func() { newDoctorDependencies = previous })
	for _, format := range []string{"human", "json"} {
		t.Run(format, func(t *testing.T) {
			cmd := newRootCmd()
			var out, stderr bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&stderr)
			args := []string{"doctor", "-c", path}
			if format == "json" {
				args = append(args, "--output", "json")
			}
			cmd.SetArgs(args)
			if err := cmd.Execute(); err != nil {
				t.Fatalf("doctor: %v: %s", err, out.String())
			}
			if format == "human" {
				if !strings.Contains(out.String(), "Onebox doctor: PASS") || !strings.Contains(out.String(), "cache/backup") || !strings.Contains(out.String(), "declared ephemeral") {
					t.Fatalf("human doctor = %s", out.String())
				}
				return
			}
			var envelope struct {
				Data doctorReport `json:"data"`
			}
			if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
				t.Fatalf("decode doctor: %v: %s", err, out.String())
			}
			if envelope.Data.Status != doctorPass || envelope.Data.Backups.Status != doctorPass || stderr.Len() != 0 {
				t.Fatalf("JSON doctor = %+v, stderr = %s", envelope.Data, stderr.String())
			}
		})
	}
}
