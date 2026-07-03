// Package engine orchestrates the M0 deploy lifecycle:
// preflight → transfer → pre-release → release → verify → finalize.
package engine

import (
	"fmt"
	"io"
	"os"
	"time"

	ctypes "github.com/compose-spec/compose-go/v2/types"

	"github.com/labstack/yeet/internal/config"
	"github.com/labstack/yeet/internal/transport"
)

type Options struct {
	Verbose bool
	Out     io.Writer
	// Sleep and Now are injectable for tests.
	Sleep func(time.Duration)
	Now   func() time.Time
	// ConvergeBuffer is the bounded wait for the proxy to observe a health
	// change (rev 5 traffic-shift protocol, the "converged" step).
	ConvergeBuffer time.Duration
	// LocalDir is the config file's directory — cwd for local hooks.
	LocalDir string
}

type Engine struct {
	Cfg     *config.Config
	Project *ctypes.Project
	T       transport.Transport
	Opts    Options
}

func New(cfg *config.Config, p *ctypes.Project, t transport.Transport, o Options) *Engine {
	if o.Out == nil {
		o.Out = os.Stdout
	}
	if o.Sleep == nil {
		o.Sleep = time.Sleep
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.ConvergeBuffer == 0 {
		o.ConvergeBuffer = 3 * time.Second
	}
	return &Engine{Cfg: cfg, Project: p, T: t, Opts: o}
}

func (e *Engine) logf(format string, a ...any) {
	_, _ = io.WriteString(e.Opts.Out, "→ "+fmt.Sprintf(format, a...)+"\n")
}
