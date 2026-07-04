// Package ui is yeet's output layer: line-oriented, phase-structured, and
// CI-honest. Color/styling degrades automatically (lipgloss renderer per
// writer: a pipe or CI log gets plain text, NO_COLOR is honored) and there is
// deliberately NO screen-repainting TUI — deploy output must survive
// scrollback, `just` wrappers, and CI logs verbatim.
package ui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"
)

type UI struct {
	mu      sync.Mutex
	out     io.Writer
	tty     bool
	verbose bool
	now     func() time.Time

	// one live spinner line at a time; println clears it so narrative and
	// spinner never collide, and the spinner repaints on its next tick
	spinLabel string
	spinOn    bool

	sHeader lipgloss.Style
	sOK     lipgloss.Style
	sFail   lipgloss.Style
	sWarn   lipgloss.Style
	sDim    lipgloss.Style
	sBold   lipgloss.Style
}

func New(out io.Writer, verbose bool) *UI {
	r := lipgloss.NewRenderer(out) // profile per writer: buffer/pipe → plain
	tty := false
	if f, ok := out.(*os.File); ok {
		tty = term.IsTerminal(int(f.Fd()))
	}
	return &UI{
		out:     out,
		tty:     tty,
		verbose: verbose,
		now:     time.Now,
		sHeader: r.NewStyle().Bold(true),
		sOK:     r.NewStyle().Foreground(lipgloss.Color("2")),
		sFail:   r.NewStyle().Foreground(lipgloss.Color("1")),
		sWarn:   r.NewStyle().Foreground(lipgloss.Color("3")),
		sDim:    r.NewStyle().Faint(true),
		sBold:   r.NewStyle().Bold(true),
	}
}

func (u *UI) println(s string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.spinOn {
		_, _ = io.WriteString(u.out, "\r\x1b[K") // clear the spinner line first
	}
	_, _ = io.WriteString(u.out, s+"\n")
}

// Header opens a section: ── title ───────────────
func (u *UI) Header(title string) {
	line := "── " + title + " " + strings.Repeat("─", max(0, 50-len(title)))
	u.println(u.sHeader.Render(line))
}

// Infof is the plain narrative line.
func (u *UI) Infof(format string, a ...any) {
	u.println("→ " + fmt.Sprintf(format, a...))
}

// Warnf is a stated-but-not-fatal condition.
func (u *UI) Warnf(format string, a ...any) {
	u.println(u.sWarn.Render("⚠ " + fmt.Sprintf(format, a...)))
}

// Begin announces a long-running step (a role roll, a build hook) so neither
// a terminal nor a CI log sits silent while it runs.
func (u *UI) Begin(label string) {
	u.println(u.sDim.Render("⟳ " + label))
}

// Done closes a step with its outcome and duration.
func (u *UI) Done(label string, d time.Duration, err error) {
	if err != nil {
		u.println(u.sFail.Render("✗ "+label) + u.sDim.Render("  "+FmtDur(d)))
		return
	}
	u.println(u.sOK.Render("✓ "+label) + u.sDim.Render("  "+FmtDur(d)))
}

// Step times a step: call the returned func with the outcome. announce prints
// a Begin line for steps long enough that silence reads as a hang.
func (u *UI) Step(label string, announce bool) func(error) {
	if announce {
		u.Begin(label)
	}
	start := u.now()
	return func(err error) { u.Done(label, u.now().Sub(start), err) }
}

// Cmd is the forensic command log — verbose only, dimmed.
func (u *UI) Cmd(host, cmd string) {
	if !u.verbose {
		return
	}
	u.println(u.sDim.Render("[" + host + "] $ " + cmd))
}

// Successf is the closing line of a successful operation.
func (u *UI) Successf(format string, a ...any) {
	u.println(u.sOK.Render(u.sBold.Render("✓ " + fmt.Sprintf(format, a...))))
}

// Failf is the closing line of a failed operation (the error itself travels
// up as the command's exit).
func (u *UI) Failf(format string, a ...any) {
	u.println(u.sFail.Render("✗ " + fmt.Sprintf(format, a...)))
}

// FmtDur renders operator-scale durations: 2.1s / 12s / 2m14s.
func FmtDur(d time.Duration) string {
	switch {
	case d < 10*time.Second:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	default:
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
}

// Println writes a pre-styled line (compose with OK/Warn/Dim/Bold).
func (u *UI) Println(s string) { u.println(s) }

// OK, Warn, Dim, Bold style fragments for callers composing their own lines
// (plan tables, diffs) without ui growing a method per table.
func (u *UI) OK(s string) string   { return u.sOK.Render(s) }
func (u *UI) Warn(s string) string { return u.sWarn.Render(s) }
func (u *UI) Dim(s string) string  { return u.sDim.Render(s) }
func (u *UI) Bold(s string) string { return u.sBold.Render(s) }

// Diff prints a unified diff with the conventional coloring: additions green,
// removals red, hunk headers dim, file headers bold. Content is untouched.
func (u *UI) Diff(diff string) {
	for _, line := range strings.Split(strings.TrimRight(diff, "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
			u.println(u.sBold.Render(line))
		case strings.HasPrefix(line, "@@"):
			u.println(u.sDim.Render(line))
		case strings.HasPrefix(line, "+"):
			u.println(u.sOK.Render(line))
		case strings.HasPrefix(line, "-"):
			u.println(u.sFail.Render(line))
		default:
			u.println(line)
		}
	}
}

var spinFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

const (
	hideCursor = "\x1b[?25l"
	showCursor = "\x1b[?25h"
)

// RestoreCursor re-shows the terminal cursor if w is a TTY — for signal
// handlers: an interrupt mid-spinner must not leave the terminal cursorless.
func RestoreCursor(w io.Writer) {
	if f, ok := w.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		_, _ = io.WriteString(w, showCursor)
	}
}

// Busy shows a live spinner while a long step runs. On a TTY it repaints one
// line in place (cleared by any interleaved output, repainted next tick); off
// a TTY it prints one ⟳ line per distinct label — CI logs stay line-honest.
// update relabels the spinner; stop erases it.
func (u *UI) Busy(label string) (update func(string), stop func()) {
	if !u.tty {
		u.Begin(label)
		last := label
		return func(l string) {
			if l != last {
				u.Begin(l)
				last = l
			}
		}, func() {}
	}
	u.mu.Lock()
	u.spinLabel, u.spinOn = label, true
	_, _ = io.WriteString(u.out, hideCursor) // the blinking cursor at line end is just noise
	u.mu.Unlock()
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(120 * time.Millisecond)
		defer t.Stop()
		i := 0
		for {
			select {
			case <-done:
				u.mu.Lock()
				_, _ = io.WriteString(u.out, "\r\x1b[K"+showCursor)
				u.spinOn = false
				u.mu.Unlock()
				return
			case <-t.C:
				u.mu.Lock()
				if u.spinOn {
					_, _ = io.WriteString(u.out, "\r\x1b[K"+u.sDim.Render(spinFrames[i%len(spinFrames)]+" "+u.spinLabel))
				}
				u.mu.Unlock()
				i++
			}
		}
	}()
	var once sync.Once
	return func(l string) {
			u.mu.Lock()
			u.spinLabel = l
			u.mu.Unlock()
		}, func() {
			once.Do(func() { close(done); <-finished })
		}
}
