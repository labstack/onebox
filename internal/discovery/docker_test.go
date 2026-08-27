package discovery

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func dockerTestServer(t *testing.T, handler http.Handler) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ob-discovery-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "docker.sock")
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		_ = listener.Close()
	})
	return socket
}

func TestDockerClientReadsOnlyRequiredContainerState(t *testing.T) {
	var mu sync.Mutex
	var requests []string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if r.Method != http.MethodGet {
			t.Errorf("controller issued mutating method %s", r.Method)
		}
		switch r.URL.Path {
		case "/containers/json":
			if !strings.Contains(r.URL.Query().Get("filters"), "com.docker.compose.project=shop") {
				t.Errorf("application filter = %s", r.URL.Query().Get("filters"))
			}
			fmt.Fprint(w, `[{"Id":"container-1"}]`)
		case "/containers/container-1/json":
			// Env is intentionally present in Docker's response. The client
			// decodes no field for it and exposes only labels, lifecycle and IP.
			fmt.Fprint(w, `{
                  "Id":"container-1",
                  "Created":"2026-08-27T12:00:00Z",
                  "Config":{"Env":["SECRET=must-not-cross-boundary"],"Labels":{"traefik.enable":"true"}},
                  "State":{"Status":"running","Health":{"Status":"healthy"}},
                  "NetworkSettings":{"Networks":{"ob-ingress":{"IPAddress":"172.20.0.8"}}}
                }`)
		default:
			http.NotFound(w, r)
		}
	})
	client := NewDockerClient(dockerTestServer(t, handler))
	containers, err := client.Containers(context.Background(), "shop")
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) != 1 || containers[0].Health != "healthy" || containers[0].Networks["ob-ingress"] != "172.20.0.8" {
		t.Fatalf("containers = %+v", containers)
	}
	if got := fmt.Sprint(containers[0]); strings.Contains(got, "must-not-cross-boundary") {
		t.Fatalf("environment escaped the Docker boundary: %s", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(requests, ",") != "GET /containers/json,GET /containers/container-1/json" {
		t.Fatalf("Docker API surface = %v", requests)
	}
}

func TestDockerClientFallsBackToGlobalIPv6Address(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/containers/json":
			fmt.Fprint(w, `[{"Id":"container-v6"}]`)
		case "/containers/container-v6/json":
			fmt.Fprint(w, `{
                  "Id":"container-v6",
                  "Created":"2026-08-27T12:00:00Z",
                  "Config":{"Labels":{"traefik.enable":"true"}},
                  "State":{"Status":"running"},
                  "NetworkSettings":{"Networks":{"ob-ingress":{"IPAddress":"","GlobalIPv6Address":"2001:db8::8"}}}
                }`)
		default:
			http.NotFound(w, r)
		}
	})
	client := NewDockerClient(dockerTestServer(t, handler))
	containers, err := client.Containers(context.Background(), "shop")
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) != 1 || containers[0].Networks["ob-ingress"] != "2001:db8::8" {
		t.Fatalf("IPv6-only endpoint = %+v", containers)
	}
}

func TestDockerEventsUsesReadOnlyFilteredReplay(t *testing.T) {
	var query url.Values
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/events" {
			t.Errorf("events request = %s %s", r.Method, r.URL.Path)
		}
		query = r.URL.Query()
		fmt.Fprintln(w, `{"Type":"container","Action":"health_status: healthy"}`)
	})
	client := NewDockerClient(dockerTestServer(t, handler))
	triggers := 0
	since := time.Unix(1_788_000_000, 0)
	if err := client.Events(context.Background(), "shop", since, func() error {
		triggers++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if triggers != 1 {
		t.Fatalf("event triggers = %d", triggers)
	}
	if query.Get("since") != fmt.Sprint(since.Unix()) || !strings.Contains(query.Get("filters"), "com.docker.compose.project=shop") {
		t.Fatalf("event query = %v", query)
	}
}
