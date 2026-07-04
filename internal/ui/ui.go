// Package ui is yeet's output layer: line-oriented, phase-structured, and
// CI-honest. Color/styling degrades automatically (lipgloss renderer per
// writer: a pipe or CI log gets plain text, NO_COLOR is honored) and there is
// deliberately NO screen-repainting TUI — deploy output must survive
// scrollback, `just` wrappers, and CI logs verbatim.
package ui

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/lipgloss"
)

type UI struct {
	mu      sync.Mutex
	out     io.Writer
	verbose bool
	now     func() time.Time

	sHeader lipgloss.Style
	sOK     lipgloss.Style
	sFail   lipgloss.Style
	sWarn   lipgloss.Style
	sDim    lipgloss.Style
	sBold   lipgloss.Style
}

func New(out io.Writer, verbose bool) *UI {
	r := lipgloss.NewRenderer(out) // profile per writer: buffer/pipe → plain
	return &UI{
		out:     out,
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
