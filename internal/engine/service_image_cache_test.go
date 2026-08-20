package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/labstack/onebox/internal/transport"
)

func TestExactServiceImageCachedRequiresMatchingRepositoryDigest(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	image := "postgres@" + digest
	for _, test := range []struct {
		name   string
		result transport.Result
		want   bool
	}{
		{"exact", transport.Result{Stdout: `["docker.io/library/postgres@` + digest + `"]` + "\n"}, true},
		{"one-of-several", transport.Result{Stdout: `["mirror.example/postgres@sha256:` + strings.Repeat("b", 64) + `","docker.io/library/postgres@` + digest + `"]`}, true},
		{"wrong-manifest", transport.Result{Stdout: `["docker.io/library/postgres@sha256:` + strings.Repeat("b", 64) + `"]` + "\n"}, false},
		{"wrong-repository", transport.Result{Stdout: `["registry.example/other@` + digest + `"]` + "\n"}, false},
		{"configuration-id-only", transport.Result{Stdout: `"` + digest + `"` + "\n"}, false},
		{"missing", transport.Result{ExitCode: 1}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &transport.Fake{Dynamic: func(command string) (transport.Result, bool) {
				if strings.HasPrefix(command, "docker image inspect") {
					return test.result, true
				}
				return transport.Result{}, false
			}}
			engine := backupLockTestEngine(fake)
			got, err := engine.ExactServiceImageCached(context.Background(), image)
			if err != nil && test.name != "configuration-id-only" {
				t.Fatal(err)
			}
			if test.name == "configuration-id-only" && err == nil {
				t.Fatal("configuration ID was accepted as RepoDigests metadata")
			}
			if got != test.want {
				t.Fatalf("cache result = %v, want %v", got, test.want)
			}
		})
	}
}

func TestExactServiceImageCachedRejectsMutableTag(t *testing.T) {
	fake := &transport.Fake{}
	engine := backupLockTestEngine(fake)
	if _, err := engine.ExactServiceImageCached(context.Background(), "postgres:17"); err == nil {
		t.Fatal("mutable tag was accepted for exact cache evidence")
	}
	if len(fake.Commands) != 0 {
		t.Fatal("mutable tag reached Docker cache inspection")
	}
}
