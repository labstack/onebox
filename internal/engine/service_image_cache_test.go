package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/transport"
)

func TestExactServiceImageCachedRequiresMatchingDockerContentID(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	image := "postgres@" + digest
	for _, test := range []struct {
		name   string
		result transport.Result
		want   bool
	}{
		{"exact", transport.Result{Stdout: digest + "\n"}, true},
		{"wrong-content", transport.Result{Stdout: "sha256:" + strings.Repeat("b", 64) + "\n"}, false},
		{"missing", transport.Result{ExitCode: 1}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &transport.Fake{Dynamic: func(command string) (transport.Result, bool) {
				if strings.HasPrefix(command, "docker image inspect") {
					return test.result, true
				}
				return transport.Result{}, false
			}}
			engine := protectionLockTestEngine(fake)
			got, err := engine.ExactServiceImageCached(context.Background(), image)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("cache result = %v, want %v", got, test.want)
			}
		})
	}
}

func TestExactServiceImageCachedRejectsMutableTag(t *testing.T) {
	fake := &transport.Fake{}
	engine := protectionLockTestEngine(fake)
	if _, err := engine.ExactServiceImageCached(context.Background(), "postgres:17"); err == nil {
		t.Fatal("mutable tag was accepted for exact cache evidence")
	}
	if len(fake.Commands) != 0 {
		t.Fatal("mutable tag reached Docker cache inspection")
	}
}
