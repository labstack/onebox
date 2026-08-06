package compose

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestStagePayloadRewritesProjectRelativeSources(t *testing.T) {
	p, err := Load(context.Background(), "testdata/payload/docker-compose.yaml", "demo")
	if err != nil {
		t.Fatal(err)
	}
	staging := t.TempDir()
	rewrites, err := StagePayload(p, staging)
	if err != nil {
		t.Fatal(err)
	}
	body, err := yaml.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	out := string(RewriteSources(body, rewrites))

	// project-relative bind mount: staged + rewritten
	if _, err := os.Stat(filepath.Join(staging, "conf", "app.conf")); err != nil {
		t.Fatalf("conf/app.conf not staged: %v", err)
	}
	if !strings.Contains(out, "./conf/app.conf") {
		t.Fatalf("bind source not rewritten to release-relative:\n%s", out)
	}
	if strings.Contains(out, "testdata/payload/conf") {
		t.Fatalf("absolute runner path leaked into rendered compose:\n%s", out)
	}
	// host-absolute mounts untouched
	if !strings.Contains(out, "/var/run/docker.sock") {
		t.Fatalf("host-absolute mount must stay:\n%s", out)
	}
	// env_file: either folded into environment (values present) or staged+rewritten
	if strings.Contains(out, "env_file") {
		if _, err := os.Stat(filepath.Join(staging, ".env")); err != nil {
			t.Fatalf("env_file survived render but .env not staged: %v", err)
		}
		if strings.Contains(out, "testdata/payload/.env") {
			t.Fatalf("env_file path not rewritten:\n%s", out)
		}
	} else if !strings.Contains(out, "FOO") {
		t.Fatalf("env_file neither preserved nor folded into environment:\n%s", out)
	}
}

func TestStagePayloadContextHonorsCancellation(t *testing.T) {
	p, err := Load(context.Background(), "testdata/payload/docker-compose.yaml", "demo")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := StagePayloadContext(ctx, p, t.TempDir(), nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("StagePayloadContext error = %v; want context cancellation", err)
	}
}

func TestStagePayloadStagesProjectRootBind(t *testing.T) {
	projectDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, "app.txt"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := LoadBytes(context.Background(), []byte(`
services:
  app:
    image: busybox
    volumes:
      - .:/app
`), "demo", projectDir, nil)
	if err != nil {
		t.Fatal(err)
	}

	staging := t.TempDir()
	rewrites, err := StagePayload(p, staging)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(staging, "app.txt"))
	if err != nil {
		t.Fatalf("root payload not staged: %v", err)
	}
	if string(got) != "payload" {
		t.Fatalf("staged payload = %q, want payload", got)
	}
	if rewrites[projectDir] != "." {
		t.Fatalf("root rewrite = %q, want .", rewrites[projectDir])
	}
	if got := string(RewriteSources([]byte("source: "+projectDir), rewrites)); got != "source: ." {
		t.Fatalf("rewritten source = %q", got)
	}
	hostPath := projectDir + "-host"
	embedded := "https://example.invalid" + projectDir
	input := []byte("project: " + projectDir + "\nhost: " + hostPath + "\nurl: " + embedded + "\n")
	want := "project: .\nhost: " + hostPath + "\nurl: " + embedded + "\n"
	if got := string(RewriteSources(input, rewrites)); got != want {
		t.Fatalf("prefix-sharing host path was corrupted:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestStagePayloadAcceptsDotDotPrefixedProjectPath(t *testing.T) {
	projectDir := t.TempDir()
	source := filepath.Join(projectDir, "..cache")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "data"), []byte("cache"), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := LoadBytes(context.Background(), []byte(`
services:
  app:
    image: busybox
    volumes:
      - ./..cache:/cache
`), "demo", projectDir, nil)
	if err != nil {
		t.Fatal(err)
	}

	staging := t.TempDir()
	rewrites, err := StagePayload(p, staging)
	if err != nil {
		t.Fatal(err)
	}
	if rewrites[source] != "./..cache" {
		t.Fatalf("rewrite = %q, want ./..cache", rewrites[source])
	}
	if _, err := os.Stat(filepath.Join(staging, "..cache", "data")); err != nil {
		t.Fatalf("..cache payload not staged: %v", err)
	}
}

func TestStagePayloadRefusesSymlinkEscape(t *testing.T) {
	projectDir := t.TempDir()
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "credential")
	if err := os.WriteFile(outside, []byte("do not copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(projectDir, "credential")); err != nil {
		t.Fatal(err)
	}
	p, err := LoadBytes(context.Background(), []byte(`
services:
  app:
    image: busybox
    volumes:
      - .:/app
`), "demo", projectDir, nil)
	if err != nil {
		t.Fatal(err)
	}

	staging := t.TempDir()
	if _, err := StagePayload(p, staging); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink escape must be refused, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(staging, "credential")); !os.IsNotExist(err) {
		t.Fatalf("external credential was staged: %v", err)
	}
}
