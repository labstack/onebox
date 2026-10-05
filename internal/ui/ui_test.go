package ui

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
)

func TestPlainWriterGetsNoANSI(t *testing.T) {
	var out bytes.Buffer
	u := New(&out, false)
	u.Header("deploy R1")
	u.Infof("phase %s", "release")
	u.Warnf("service %q has no healthcheck", "ofelia")
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
		"⚠ service \"ofelia\" has no healthcheck",
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
		420 * time.Millisecond:  "0.4s",
		2100 * time.Millisecond: "2.1s",
		12 * time.Second:        "12s",
		84 * time.Second:        "1m24s",
		134 * time.Second:       "2m14s",
		61 * time.Minute:        "1h1m",
		8 * time.Hour:           "8h0m",
	}
	for d, want := range cases {
		if got := FmtDur(d); got != want {
			t.Fatalf("FmtDur(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestAnnouncedStepIsLineHonestOffTTY(t *testing.T) {
	var out bytes.Buffer
	u := New(&out, false)
	u.now = func() time.Time { return time.Unix(0, 0) }
	done := u.Step("job catalog-refresh", true)
	u.now = func() time.Time { return time.Unix(61, 0) }
	done(nil)

	s := out.String()
	if strings.ContainsAny(s, "\r\x1b") {
		t.Fatalf("non-TTY step must not emit control sequences: %q", s)
	}
	for _, want := range []string{"⟳ job catalog-refresh", "✓ job catalog-refresh", "1m1s"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in:\n%s", want, s)
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

func TestDiffPlainOnBuffer(t *testing.T) {
	var out bytes.Buffer
	u := New(&out, false)
	u.Diff("--- live (R1)\n+++ planned (R2)\n@@ -1,2 +1,2 @@\n context\n-old line\n+new line\n")
	s := out.String()
	if strings.Contains(s, "\x1b[") {
		t.Fatalf("buffer must stay plain: %q", s)
	}
	for _, want := range []string{"--- live (R1)", "+new line", "-old line", "@@ -1,2 +1,2 @@"} {
		if !strings.Contains(s, want) {
			t.Fatalf("diff content lost %q:\n%s", want, s)
		}
	}
}

func TestStyleAccessorsPlainOnBuffer(t *testing.T) {
	var out bytes.Buffer
	u := New(&out, false)
	u.Println(u.Bold("images:"))
	u.Println("  server  " + u.Dim("ghcr.io/x@sha256:abc") + "  " + u.OK("[pinned]"))
	u.Println("  worker  ghcr.io/x:tag  " + u.Warn("[TAG-BOUND]"))
	s := out.String()
	if strings.Contains(s, "\x1b[") {
		t.Fatalf("plain writer: %q", s)
	}
	if !strings.Contains(s, "images:") || !strings.Contains(s, "[pinned]") || !strings.Contains(s, "[TAG-BOUND]") {
		t.Fatalf("content lost:\n%s", s)
	}
}

func TestBusyNonTTY(t *testing.T) {
	var out bytes.Buffer
	u := New(&out, false)
	update, stop := u.Busy("pinning images")
	update("staging release")
	update("staging release") // same label: no repeat
	stop()
	s := out.String()
	if strings.Contains(s, "\x1b[") || strings.Contains(s, "\r") {
		t.Fatalf("non-TTY busy must not emit control sequences: %q", s)
	}
	if strings.Count(s, "⟳ pinning images") != 1 || strings.Count(s, "⟳ staging release") != 1 {
		t.Fatalf("busy labels must print once each:\n%s", s)
	}
}

func TestProgressNonTTYReportsDistinctMeasuredCounts(t *testing.T) {
	var out bytes.Buffer
	u := New(&out, false)
	update, stop := u.Progress("server rolling", 3)
	update(-1, "starting replica 1")
	update(2, "draining old replica")
	update(2, "draining old replica")
	update(99, "replicas converged")
	stop()
	update(1, "late update")
	stop()
	s := out.String()
	if strings.ContainsAny(s, "\r\x1b") || strings.ContainsAny(s, "■□") {
		t.Fatalf("plain progress must stay line-oriented: %q", s)
	}
	for _, want := range []string{"server rolling · 0/3", "server rolling · 2/3", "server rolling · 3/3"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in %s", want, s)
		}
	}
	if strings.Count(s, "draining old replica") != 1 || strings.Contains(s, "late update") {
		t.Fatalf("duplicate or stopped progress update: %s", s)
	}
}

func TestProgressUnknownTotalFallsBackToSpinner(t *testing.T) {
	var out bytes.Buffer
	u := New(&out, false)
	update, stop := u.Progress("packing archive", 0)
	defer stop()
	update(42, "waiting for remote extraction")
	if s := out.String(); strings.Contains(s, "/0") || !strings.Contains(s, "waiting for remote extraction") {
		t.Fatalf("unknown total must not invent a percentage: %s", s)
	}
}

func TestNestedBusyKeepsProgressAndRestoresParent(t *testing.T) {
	var out bytes.Buffer
	u := New(&out, false)
	u.tty = true
	_, stopStep := u.Busy("server rolling ×3")
	defer stopStep()
	update, stopProgress := u.Progress("server rolling", 3)
	defer stopProgress()
	update(2, "assigning slots")
	_, stopWait := u.Busy("replica 3 healthcheck")
	defer stopWait()
	u.mu.Lock()
	line := u.busyLineLocked(0, 79)
	u.mu.Unlock()
	for _, want := range []string{"server rolling", "■■□", "2/3", "replica 3 healthcheck"} {
		if !strings.Contains(line, want) {
			t.Fatalf("nested wait lost %q: %s", want, line)
		}
	}
	u.Infof("an interleaved log")
	stopWait()
	u.mu.Lock()
	line = u.busyLineLocked(0, 79)
	shown := strings.Contains(out.String(), showCursor)
	u.mu.Unlock()
	if shown || strings.Contains(line, "healthcheck") || !strings.Contains(line, "assigning slots") {
		t.Fatalf("child stop must restore its parent with cursor hidden: %s", line)
	}
	stopProgress()
	u.mu.Lock()
	line = u.busyLineLocked(0, 79)
	u.mu.Unlock()
	if !strings.Contains(line, "server rolling ×3") || strings.Contains(line, "2/3") {
		t.Fatalf("progress stop did not restore the outer step: %s", line)
	}
	stopStep()
	if strings.Count(out.String(), hideCursor) != 1 || strings.Count(out.String(), showCursor) != 1 {
		t.Fatalf("nested spinners must share cursor ownership: %q", out.String())
	}
}

func TestStoppingOuterBusyDoesNotEraseChild(t *testing.T) {
	var out bytes.Buffer
	u := New(&out, false)
	u.tty = true
	updateOuter, stopOuter := u.Busy("outer step")
	defer stopOuter()
	_, stopChild := u.Busy("child wait")
	defer stopChild()
	stopOuter()
	updateOuter("late outer update")
	u.mu.Lock()
	line := u.busyLineLocked(0, 79)
	shown := strings.Contains(out.String(), showCursor)
	u.mu.Unlock()
	if shown || !strings.Contains(line, "child wait") || strings.Contains(line, "late outer") {
		t.Fatalf("outer stop must leave child active: %s", line)
	}
	stopChild()
	if strings.Count(out.String(), showCursor) != 1 {
		t.Fatalf("cursor must return once the last activity stops: %q", out.String())
	}
}

func TestBusyLineFitsNarrowAndUnicodeTerminals(t *testing.T) {
	u := New(&bytes.Buffer{}, false)
	u.busy = []*busyState{{
		label: "server 界界界界界界界界界界 rolling", detail: "healthcheck\nstarting",
		completed: 2, total: 3, started: u.now().Add(-12 * time.Second),
	}}
	for _, width := range []int{1, 8, 20, 40, 79} {
		line := u.busyLineLocked(0, width)
		if lipgloss.Width(line) > width || strings.ContainsAny(line, "\r\n") {
			t.Fatalf("width %d: spinner wraps or contains newlines: %q", width, line)
		}
		if width >= 8 && (!strings.Contains(line, "2/3") || !strings.Contains(line, "12s")) {
			t.Fatalf("width %d: measured progress and timing must survive truncation: %s", width, line)
		}
	}
}

func TestAnimationControls(t *testing.T) {
	for _, tc := range []struct {
		name, term, ci, noAnimation string
		want                        bool
	}{
		{"interactive", "xterm-256color", "", "", true},
		{"ci-false", "xterm-256color", "false", "", true},
		{"ci", "xterm-256color", "true", "", false},
		{"dumb", "dumb", "", "", false},
		{"static", "xterm-256color", "", "1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TERM", tc.term)
			t.Setenv("CI", tc.ci)
			t.Setenv("ONEBOX_NO_ANIMATION", tc.noAnimation)
			if got := animationAllowed(); got != tc.want {
				t.Fatalf("animationAllowed() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHeaderFitsInteractiveTerminal(t *testing.T) {
	var out bytes.Buffer
	u := New(&out, false)
	u.tty = true
	u.width = func() int { return 24 }
	u.Header("deploy catalog → production · R42")
	if line := strings.TrimSuffix(out.String(), "\n"); lipgloss.Width(line) > 23 {
		t.Fatalf("header wraps in a narrow terminal: %q", line)
	}
}
