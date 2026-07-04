package ui

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPlainWriterGetsNoANSI(t *testing.T) {
	var out bytes.Buffer
	u := New(&out, false)
	u.Header("deploy R1")
	u.Infof("phase %s", "release")
	u.Warnf("accessory %q has no healthcheck", "ofelia")
	u.Begin("server rolling ×3")
	u.Done("server rolling ×3", 84*time.Second, nil)
	u.Done("verify", 2300*time.Millisecond, errors.New("boom"))
	u.Successf("deployed R1 in %s", FmtDur(134*time.Second))

	s := out.String()
	if strings.Contains(s, "\x1b[") {
		t.Fatalf("non-TTY writer must get no ANSI:\n%q", s)
	}
	for _, want := range []string{
		"── deploy R1 ─",
		"→ phase release",
		"⚠ accessory \"ofelia\" has no healthcheck",
		"⟳ server rolling ×3",
		"✓ server rolling ×3", "1m24s",
		"✗ verify", "2.3s",
		"✓ deployed R1 in 2m14s",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in:\n%s", want, s)
		}
	}
}

func TestCmdOnlyWhenVerbose(t *testing.T) {
	var out bytes.Buffer
	u := New(&out, false)
	u.Cmd("h", "docker ps")
	if out.Len() != 0 {
		t.Fatalf("quiet mode must not print commands: %q", out.String())
	}
	u = New(&out, true)
	u.Cmd("h", "docker ps")
	if !strings.Contains(out.String(), "[h] $ docker ps") {
		t.Fatalf("verbose must print the command: %q", out.String())
	}
}

func TestFmtDur(t *testing.T) {
	cases := map[time.Duration]string{
		420 * time.Millisecond: "0.4s",
		2100 * time.Millisecond: "2.1s",
		12 * time.Second:        "12s",
		84 * time.Second:        "1m24s",
		134 * time.Second:       "2m14s",
		61 * time.Minute:        "61m0s",
	}
	for d, want := range cases {
		if got := FmtDur(d); got != want {
			t.Fatalf("FmtDur(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestStepHelper(t *testing.T) {
	var out bytes.Buffer
	u := New(&out, false)
	u.now = func() time.Time { return time.Unix(0, 0) }
	done := u.Step("preflight", false) // announce=false: nothing at start
	if out.Len() != 0 {
		t.Fatalf("silent step start must print nothing: %q", out.String())
	}
	u.now = func() time.Time { return time.Unix(3, 0) }
	done(nil)
	if !strings.Contains(out.String(), "✓ preflight") || !strings.Contains(out.String(), "3.0s") {
		t.Fatalf("step completion: %q", out.String())
	}
}
