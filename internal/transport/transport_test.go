package transport

import (
	"context"
	"errors"
	"regexp"
	"testing"
)

func TestLocalRunCapturesExitAndOutput(t *testing.T) {
	tr := NewLocal()
	res, err := tr.Run(context.Background(), "echo hi; exit 3")
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 || res.Stdout != "hi\n" {
		t.Fatalf("got %+v", res)
	}
}

func TestNewSSHContextHonorsPreCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewSSHContext(ctx, "example.invalid"); !errors.Is(err, context.Canceled) {
		t.Fatalf("NewSSHContext error = %v; want context cancellation", err)
	}
}

func TestParseAddr(t *testing.T) {
	cases := []struct{ in, user, host, port string }{
		{"deploy@app.example.com", "deploy", "app.example.com", "22"},
		{"app.example.com:2222", "", "app.example.com", "2222"},
		{"root@10.0.0.5:22", "root", "10.0.0.5", "22"},
	}
	for _, c := range cases {
		u, h, p := ParseAddr(c.in)
		if u != c.user || h != c.host || p != c.port {
			t.Fatalf("%s -> %s,%s,%s", c.in, u, h, p)
		}
	}
}

func TestFakeScriptsAndRecords(t *testing.T) {
	f := &Fake{Script: []Rule{
		{Match: regexp.MustCompile(`docker inspect`), Result: Result{Stdout: "healthy\n"}},
	}}
	res, _ := f.Run(context.Background(), "docker inspect -f x abc")
	if res.Stdout != "healthy\n" {
		t.Fatalf("scripted reply not used: %+v", res)
	}
	res, _ = f.Run(context.Background(), "docker stop abc")
	if res.ExitCode != 0 {
		t.Fatalf("default should be exit 0: %+v", res)
	}
	if len(f.Commands) != 2 || f.Commands[1] != "docker stop abc" {
		t.Fatalf("recording broken: %v", f.Commands)
	}
}
