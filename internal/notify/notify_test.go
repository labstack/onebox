package notify

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/yeet/internal/config"
)

func TestSendPostsPayload(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content-type: %s", ct)
		}
	}))
	defer srv.Close()

	n := &config.Notify{Webhook: srv.URL, On: []string{"failure", "success"}}
	err := Send(n, Payload{
		App: "monk", Env: "production", Verb: "deploy", DeployID: "R1",
		Status: "fail", Error: "verify: gate closed — HALT-AND-PAGE ...",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got["app"] != "monk" || got["verb"] != "deploy" || got["status"] != "fail" {
		t.Fatalf("payload: %v", got)
	}
	// the human-readable line for Slack/Discord/ntfy compatibility
	text, _ := got["text"].(string)
	if !strings.Contains(text, "monk") || !strings.Contains(text, "deploy") || !strings.Contains(text, "FAILED") {
		t.Fatalf("text summary: %q", text)
	}
	if !strings.Contains(text, "HALT-AND-PAGE") {
		t.Fatalf("error must reach the text line: %q", text)
	}
}

func TestSendFiltersByOn(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer srv.Close()

	n := &config.Notify{Webhook: srv.URL, On: []string{"failure"}}
	if err := Send(n, Payload{App: "monk", Verb: "deploy", Status: "ok"}); err != nil {
		t.Fatal(err) // filtered out is not an error
	}
	if hits != 0 {
		t.Fatal("success must be filtered when on: [failure]")
	}
	if err := Send(n, Payload{App: "monk", Verb: "deploy", Status: "fail", Error: "x"}); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Fatal("failure must fire")
	}
	// nil config: no-op
	if err := Send(nil, Payload{Status: "fail"}); err != nil {
		t.Fatal(err)
	}
}

func TestSendFailOpen(t *testing.T) {
	// dead endpoint: returns an error for the caller to WARN on — never panics,
	// never hangs past the timeout
	n := &config.Notify{Webhook: "http://127.0.0.1:1", On: []string{"failure"}}
	start := time.Now()
	if err := Send(n, Payload{App: "monk", Verb: "deploy", Status: "fail", Error: "x"}); err == nil {
		t.Fatal("dead webhook must surface an error for the warn log")
	}
	if time.Since(start) > 6*time.Second {
		t.Fatal("send must respect the timeout")
	}

	// non-2xx is also reported
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	n = &config.Notify{Webhook: srv.URL, On: []string{"failure"}}
	if err := Send(n, Payload{Status: "fail"}); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("non-2xx must be reported: %v", err)
	}
}
