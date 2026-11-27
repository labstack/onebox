package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/proxy"
)

// proxy.trustedProxies is rendered into the managed static configuration and
// loaded by the real Traefik image exactly as staged. A request from a peer in
// a trusted prefix keeps the X-Forwarded-For it arrived with, and Traefik
// appends the peer; a request from any other peer has the header replaced by
// the peer's own address. The unit tests can only assert the YAML; this proves
// Traefik reads it that way on an Onebox-declared entrypoint.
func TestTrustedProxiesKeepForwardedHeadersOnlyFromTrustedPeers(t *testing.T) {
	gate(t)
	ctx := context.Background()
	pid := os.Getpid()
	application := fmt.Sprintf("obtp%d", pid)
	network := application + "_net"
	backend, traefik := application+"-backend", application+"-traefik"
	// A private /24 chosen per run so that every address, and so trust, is
	// fixed by the test rather than by Docker's address pool.
	subnet := fmt.Sprintf("10.%d.%d", 200+pid%50, pid%250)
	trusted := subnet + ".16/28"
	trustedClient, untrustedClient := subnet+".20", subnet+".40"

	body := fmt.Sprintf(`apiVersion: onebox.run/v1alpha2
kind: Application
metadata: {name: %s}
spec:
  environments:
    production: {server: root@localhost}
  workloads:
    web:
      image: busybox:1.37
      routes:
        - {hostname: app.example.com, port: 80, entrypoint: plain, tls: None}
  proxy:
    entrypoints: {plain: {port: 8081}}
    trustedProxies: [%s]
`, application, trusted)
	project, err := app.LoadBytes([]byte(body), filepath.Join(t.TempDir(), "ob.yml"))
	if err != nil {
		t.Fatal(err)
	}
	staging := t.TempDir()
	if _, err := proxy.StageForAppManaged("", staging, "", "", project.Name, network, project.Proxy.Entrypoints,
		project.HasExactTerminatingTLS(), project.HasWildcardTerminatingTLS(), project.Proxy.DNSChallenge, project.Proxy.TrustedProxies); err != nil {
		t.Fatal(err)
	}
	// Discovery's output is replaced by one router, so the request's path to
	// the backend does not depend on Host matching inside busybox wget.
	router := "http:\n  routers:\n    probe:\n      rule: PathPrefix(`/`)\n      entryPoints: [plain]\n      service: probe\n" +
		"  services:\n    probe:\n      loadBalancer:\n        servers:\n          - url: http://" + backend + ":80\n"
	if err := os.WriteFile(filepath.Join(staging, "dynamic", "probe.yml"), []byte(router), 0o644); err != nil {
		t.Fatal(err)
	}
	// The staged files are written for the proxy's non-root user; this test
	// runs Traefik as the image's default user, which must be able to read them.
	for _, dir := range []string{filepath.Join(staging, "config"), filepath.Join(staging, "dynamic")} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				if err := os.Chmod(filepath.Join(dir, entry.Name()), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
	}

	docker := func(args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	must := func(args ...string) string {
		t.Helper()
		out, err := docker(args...)
		if err != nil {
			t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := testCleanupContext()
		defer cancel()
		_ = exec.CommandContext(cleanupCtx, "docker", "rm", "-f", traefik, backend).Run()
		_ = exec.CommandContext(cleanupCtx, "docker", "network", "rm", network).Run()
	})
	must("network", "create", "--subnet", subnet+".0/24", network)
	// The backend answers with the X-Forwarded-For header it received.
	must("run", "-d", "--name", backend, "--network", network, "--ip", subnet+".3", "busybox:1.37", "sh", "-c",
		`mkdir -p /www/cgi-bin && printf '#!/bin/sh\necho Content-Type: text/plain\necho\necho "xff=$HTTP_X_FORWARDED_FOR"\n' > /www/cgi-bin/h && chmod +x /www/cgi-bin/h && exec httpd -f -p 80 -h /www`)
	must("run", "-d", "--name", traefik, "--network", network, "--ip", subnet+".2",
		"-v", filepath.Join(staging, "config")+":/etc/traefik:ro",
		"-v", filepath.Join(staging, "dynamic")+":/etc/traefik/dynamic:ro",
		"--tmpfs", proxy.AcmeStorageDir,
		proxy.DefaultImage)

	request := func(client string) (string, error) {
		return docker("run", "--rm", "--network", network, "--ip", client, "busybox:1.37",
			"wget", "-qO-", "-T", "2", "--header", "X-Forwarded-For: 203.0.113.7", "http://"+traefik+":8081/cgi-bin/h")
	}
	deadline := time.Now().Add(60 * time.Second)
	got, err := request(trustedClient)
	for err != nil && time.Now().Before(deadline) {
		time.Sleep(time.Second)
		got, err = request(trustedClient)
	}
	if err != nil {
		logs, _ := docker("logs", traefik)
		t.Fatalf("no response through Traefik: %v\n%s\ntraefik:\n%s", err, got, logs)
	}
	if want := "xff=203.0.113.7, " + trustedClient; got != want {
		t.Errorf("trusted peer: backend saw %q, want %q", got, want)
	}
	got, err = request(untrustedClient)
	if err != nil {
		t.Fatalf("untrusted request: %v\n%s", err, got)
	}
	if want := "xff=" + untrustedClient; got != want {
		t.Errorf("untrusted peer: backend saw %q, want %q", got, want)
	}
}
