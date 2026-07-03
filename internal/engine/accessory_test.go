package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/labstack/yeet/internal/release"
	"github.com/labstack/yeet/internal/transport"
)

func accessoryStaging(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := release.Stage(dir, []byte("services:\n  postgres:\n    image: postgres:18\n"), []byte("snap")); err != nil {
		t.Fatal(err)
	}
	return dir
}

func accFake(mounts string) *transport.Fake {
	f := happyFake()
	base := f.Dynamic
	f.Dynamic = func(cmd string) (transport.Result, bool) {
		if strings.Contains(cmd, ".Mounts") {
			return transport.Result{Stdout: mounts + "\n"}, true
		}
		if strings.Contains(cmd, "cat ") && strings.Contains(cmd, "compose.yaml") {
			return transport.Result{Stdout: "services:\n  postgres:\n    image: postgres:17\n"}, true
		}
		if strings.Contains(cmd, "readlink") {
			return transport.Result{Stdout: "releases/R0\n"}, true
		}
		return base(cmd)
	}
	return f
}

func TestAccessoryApplyConvergesUnderRegime(t *testing.T) {
	f := accFake("") // no mounts on the running accessory → nothing to lose
	var out bytes.Buffer
	e := New(testConfig(), testProject(t), f, Options{Out: &out, Sleep: noSleep})
	if err := e.AccessoryApply(context.Background(), "R9-acc", accessoryStaging(t), false); err != nil {
		t.Fatalf("apply: %v\n%s", err, strings.Join(f.Commands, "\n"))
	}
	seq := strings.Join(f.Commands, "\n")
	if !strings.Contains(seq, "up -d --no-deps postgres") {
		t.Fatalf("converge missing:\n%s", seq)
	}
	for _, c := range f.Commands {
		if strings.Contains(c, "up -d --no-deps postgres") && !strings.Contains(c, "yeet-fenced") {
			t.Fatalf("converge not fenced: %s", c)
		}
	}
	if !strings.Contains(seq, `"phase":"accessory-apply"`) {
		t.Fatalf("not journaled:\n%s", seq)
	}
	if !strings.Contains(out.String(), "postgres:18") {
		t.Fatalf("diff not shown:\n%s", out.String())
	}
}

func TestAccessoryApplyRefusesDestructiveMounts(t *testing.T) {
	// running postgres uses a named volume the new config no longer declares
	f := accFake("volume=pgdata bind=/var/lib/yeet/monk/releases/R0/conf")
	e := New(testConfig(), testProject(t), f, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	err := e.AccessoryApply(context.Background(), "R9-acc", accessoryStaging(t), false)
	if err == nil || !strings.Contains(err.Error(), "pgdata") {
		t.Fatalf("want destructive refusal naming pgdata, got %v", err)
	}
	if strings.Contains(strings.Join(f.Commands, "\n"), "up -d --no-deps") {
		t.Fatal("must not converge after refusal")
	}
	// the per-release payload bind must NOT count as destructive
	if strings.Contains(err.Error(), "/releases/") {
		t.Fatalf("release-relative bind wrongly flagged: %v", err)
	}

	// --force proceeds
	f2 := accFake("volume=pgdata")
	e2 := New(testConfig(), testProject(t), f2, Options{Out: &bytes.Buffer{}, Sleep: noSleep})
	if err := e2.AccessoryApply(context.Background(), "R9-acc", accessoryStaging(t), true); err != nil {
		t.Fatalf("force apply: %v", err)
	}
}
