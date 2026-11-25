package e2e

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/discovery"
	"github.com/labstack/onebox/internal/proxy"
	"gopkg.in/yaml.v3"
)

// A TCP passthrough route with proxyProtocol: 2 makes the real Traefik image
// open each backend connection with a PROXY v2 header before the client's TLS
// bytes. The route's labels come from the rendered release and the dynamic
// document from discovery, loaded by Traefik's file provider exactly as
// written, so this proves the generated servers transport and the plain-name
// reference resolve, which the unit tests can only assert as YAML.
func TestTCPRouteSendsProxyProtocolV2(t *testing.T) {
	gate(t)
	ctx := context.Background()
	application := fmt.Sprintf("obpp%d", os.Getpid())
	network := application + "_default"
	backend, traefik := application+"-backend", application+"-traefik"

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
        - {hostname: device.example.com, port: 7581, protocol: tcp, tls: Passthrough, proxyProtocol: 2}
`, application)
	project, err := app.LoadBytes([]byte(body), filepath.Join(t.TempDir(), "ob.yml"))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := project.Resolve("production")
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := resolved.Render("production", "R1", nil)
	if err != nil {
		t.Fatal(err)
	}
	var compose struct {
		Services map[string]struct {
			Labels map[string]string `yaml:"labels"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(rendered.Bytes, &compose); err != nil {
		t.Fatal(err)
	}
	labels := compose.Services["web"].Labels
	if len(labels) == 0 {
		t.Fatalf("the rendered web service has no labels:\n%s", rendered.Bytes)
	}

	docker := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := testCleanupContext()
		defer cancel()
		_ = exec.CommandContext(cleanupCtx, "docker", "rm", "-f", traefik, backend).Run()
		_ = exec.CommandContext(cleanupCtx, "docker", "network", "rm", network).Run()
	})
	docker("network", "create", network)
	// The backend records the first bytes of its first connection as hex.
	docker("run", "-d", "--name", backend, "--network", network,
		"busybox:1.37", "sh", "-c", "nc -l -p 7581 | head -c 16 | od -An -tx1")
	backendIP := docker("inspect", "-f", "{{(index .NetworkSettings.Networks \""+network+"\").IPAddress}}", backend)

	document, err := discovery.Build([]discovery.Container{{
		ID: backend, Created: time.Now(), Running: true, Health: "healthy",
		Labels: labels, Networks: map[string]string{network: backendIP},
	}}, network)
	if err != nil {
		t.Fatal(err)
	}
	dynamicBody, err := yaml.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	dynamic := t.TempDir()
	if err := os.WriteFile(filepath.Join(dynamic, "onebox.yml"), dynamicBody, 0o644); err != nil {
		t.Fatal(err)
	}
	// The route's entrypoint is whatever the release rendered; the test proxy
	// defines that one name and publishes it on an ephemeral loopback port.
	entrypoint := ""
	for key, value := range labels {
		if strings.HasPrefix(key, "traefik.tcp.routers.") && strings.HasSuffix(key, ".entrypoints") {
			entrypoint = value
		}
	}
	if entrypoint == "" {
		t.Fatalf("no TCP router entrypoint in the rendered labels: %v", labels)
	}
	docker("run", "-d", "--name", traefik, "--network", network,
		"-p", "127.0.0.1::8443", "-v", dynamic+":/dynamic:ro", proxy.DefaultImage,
		"--providers.file.directory=/dynamic", "--entrypoints."+entrypoint+".address=:8443")
	address := docker("port", traefik, "8443/tcp")
	if i := strings.IndexByte(address, '\n'); i >= 0 {
		address = address[:i]
	}

	// Retry until Traefik has loaded the route and the backend has a capture.
	deadline := time.Now().Add(60 * time.Second)
	var capture string
	for time.Now().Before(deadline) {
		dialer := net.Dialer{Timeout: time.Second}
		if conn, err := dialer.DialContext(ctx, "tcp", address); err == nil {
			hello, cancel := context.WithTimeout(ctx, 2*time.Second)
			// The backend is not TLS, so the handshake fails; only the bytes
			// Traefik forwards matter.
			_ = tls.Client(conn, &tls.Config{ServerName: "device.example.com"}).HandshakeContext(hello)
			cancel()
			_ = conn.Close()
		}
		capture = strings.Join(strings.Fields(docker("logs", backend)), " ")
		if capture != "" {
			break
		}
		time.Sleep(time.Second)
	}
	// v2 signature, then version 2 + PROXY command (0x21), TCP over IPv4 (0x11).
	const want = "0d 0a 0d 0a 00 0d 0a 51 55 49 54 0a 21 11"
	if !strings.HasPrefix(capture, want) {
		t.Fatalf("backend's first bytes = %q, want a PROXY v2 TCPv4 header %q\ndynamic:\n%s\ntraefik:\n%s",
			capture, want, dynamicBody, docker("logs", traefik))
	}
}
