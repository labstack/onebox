// Package ui is ob's output layer: line-oriented, phase-structured, and
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
	"github.com/charmbracelet/x/ansi"
	"golang.org/x/term"
)

type UI struct {
	mu      sync.Mutex
	out     io.Writer
	tty     bool
	verbose bool
	now     func() time.Time

	// Nested steps share one live line. A child wait keeps its parent's measured
	// progress visible; stopping it restores the parent without showing the cursor.
	busy  []*busyState
	width func() int

	sHeader lipgloss.Style
	sOK     lipgloss.Style
	sFail   lipgloss.Style
	sWarn   lipgloss.Style
	sDim    lipgloss.Style
	sBold   lipgloss.Style
	sActive lipgloss.Style
}

type busyState struct {
	label, detail    string
	completed, total int
	started          time.Time
	stopped          bool
}

func New(out io.Writer, verbose bool) *UI {
	r := lipgloss.NewRenderer(out) // profile per writer: buffer/pipe → plain
	tty := false
	if f, ok := out.(*os.File); ok {
		tty = term.IsTerminal(int(f.Fd()))
	}
	tty = tty && os.Getenv("TERM") != "dumb" && os.Getenv("ONEBOX_NO_ANIMATION") == "" &&
		(os.Getenv("CI") == "" || os.Getenv("CI") == "false")
	return &UI{
		out:     out,
		tty:     tty,
		verbose: verbose,
		now:     time.Now,
		width: func() int {
			if f, ok := out.(*os.File); ok {
				if w, _, err := term.GetSize(int(f.Fd())); err == nil && w > 0 {
					return w
				}
			}
			return 80
		},
		sHeader: r.NewStyle().Bold(true),
		sOK:     r.NewStyle().Foreground(lipgloss.Color("2")),
		sFail:   r.NewStyle().Foreground(lipgloss.Color("1")),
		sWarn:   r.NewStyle().Foreground(lipgloss.Color("3")),
		sDim:    r.NewStyle().Faint(true),
		sBold:   r.NewStyle().Bold(true),
		sActive: r.NewStyle().Foreground(lipgloss.Color("6")),
	}
}

func (u *UI) println(s string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.printlnLocked(s)
}

func (u *UI) printlnLocked(s string) {
	if len(u.busy) > 0 {
		_, _ = io.WriteString(u.out, "\r\x1b[K") // clear the spinner line first
	}
	_, _ = io.WriteString(u.out, s+"\n")
}

// Header opens a section: ── title ───────────────
func (u *UI) Header(title string) {
	line := "── " + title + " " + strings.Repeat("─", max(0, 50-len(title)))
	if u.tty {
		line = ansi.Truncate(line, max(1, u.width()-1), "…")
	}
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
		u.println(u.sFail.Render("✗") + " " + label + u.sDim.Render("  "+FmtDur(d)))
		return
	}
	u.println(u.sOK.Render("✓") + " " + label + u.sDim.Render("  "+FmtDur(d)))
}

// Step times a step: call the returned func with the outcome. Announced steps
// get a live elapsed-time spinner on a TTY and a durable Begin line elsewhere.
func (u *UI) Step(label string, announce bool) func(error) {
	start := u.now()
	stop := func() {}
	if announce {
		_, stop = u.Busy(label)
	}
	return func(err error) {
		stop()
		u.Done(label, u.now().Sub(start), err)
	}
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
	case d < time.Hour:
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
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

var spinFrames = []string{"▖", "▘", "▝", "▗"}

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
	set, stop := u.startBusy(label, 0)
	return func(label string) { set(0, label) }, stop
}

// Progress displays completed units out of a known total. Small totals get one
// segment per unit; larger totals use a compact ten-segment bar. A total <= 0
// falls back to a spinner. Off a TTY, only distinct count/detail updates print.
// Completion of the units does not imply success: the caller still owns Done.
func (u *UI) Progress(label string, total int) (update func(int, string), stop func()) {
	return u.startBusy(label, max(0, total))
}

func (u *UI) startBusy(label string, total int) (func(int, string), func()) {
	s := &busyState{label: label, total: total, started: u.now()}
	if !u.tty {
		last := plainBusy(s)
		u.Begin(last)
		return func(completed int, text string) {
				u.mu.Lock()
				defer u.mu.Unlock()
				if s.stopped {
					return
				}
				setBusy(s, completed, text)
				if line := plainBusy(s); line != last {
					u.printlnLocked(u.sDim.Render("⟳ " + line))
					last = line
				}
			}, func() {
				u.mu.Lock()
				defer u.mu.Unlock()
				s.stopped = true
			}
	}
	u.mu.Lock()
	if len(u.busy) == 0 {
		_, _ = io.WriteString(u.out, hideCursor)
	}
	u.busy = append(u.busy, s)
	u.renderBusyLocked(0)
	u.mu.Unlock()
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(120 * time.Millisecond)
		defer t.Stop()
		i := 1
		for {
			select {
			case <-done:
				return
			case <-t.C:
				u.mu.Lock()
				if len(u.busy) > 0 && u.busy[len(u.busy)-1] == s {
					u.renderBusyLocked(i)
				}
				u.mu.Unlock()
				i++
			}
		}
	}()
	var once sync.Once
	return func(completed int, text string) {
			u.mu.Lock()
			if !s.stopped {
				setBusy(s, completed, text)
				u.renderBusyLocked(0)
			}
			u.mu.Unlock()
		}, func() {
			once.Do(func() {
				u.mu.Lock()
				s.stopped = true
				for i, item := range u.busy {
					if item == s {
						u.busy = append(u.busy[:i], u.busy[i+1:]...)
						break
					}
				}
				if len(u.busy) == 0 {
					_, _ = io.WriteString(u.out, "\r\x1b[K"+showCursor)
				} else {
					u.renderBusyLocked(0)
				}
				u.mu.Unlock()
				close(done)
				<-finished
			})
		}
}

func setBusy(s *busyState, completed int, text string) {
	if s.total > 0 {
		s.completed, s.detail = min(max(0, completed), s.total), text
	} else {
		s.label = text
	}
}

func plainBusy(s *busyState) string {
	line := s.label
	if s.total > 0 {
		line += fmt.Sprintf(" · %d/%d", s.completed, s.total)
		if s.detail != "" {
			line += " · " + s.detail
		}
	}
	return line
}

func (u *UI) renderBusyLocked(frame int) {
	_, _ = io.WriteString(u.out, "\r\x1b[K"+u.busyLineLocked(frame, max(1, u.width()-1)))
}

// Keep one cell free to avoid automatic wrapping at the terminal's right edge.
// Timing and counts survive truncation; only the narrative text is shortened.
func (u *UI) busyLineLocked(frame, width int) string {
	current := u.busy[len(u.busy)-1]
	display := current
	for i := len(u.busy) - 1; i >= 0; i-- {
		if u.busy[i].total > 0 {
			display = u.busy[i]
			break
		}
	}
	detail := display.detail
	if current != display {
		detail = current.label
	}
	text := display.label
	if detail != "" {
		text += " · " + detail
	}
	text = strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(text)
	prefix := u.sActive.Render(spinFrames[frame%len(spinFrames)]) + " "
	suffix := u.sDim.Render("  " + FmtDur(u.now().Sub(display.started)))
	if display.total > 0 {
		count := fmt.Sprintf(" %d/%d", display.completed, display.total)
		bar := ""
		if width >= 60 && display.total > 1 {
			segments := min(display.total, 10)
			filled := int(float64(display.completed) / float64(display.total) * float64(segments))
			bar = u.sActive.Render(strings.Repeat("■", filled)) + u.sDim.Render(strings.Repeat("□", segments-filled))
		}
		suffix = "  " + bar + u.sDim.Render(count) + suffix
	}
	available := width - lipgloss.Width(prefix) - lipgloss.Width(suffix)
	if available < 1 {
		return ansi.Truncate(prefix+text+suffix, width, "…")
	}
	return prefix + ansi.Truncate(text, available, "…") + suffix
}
