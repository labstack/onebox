// Package engine orchestrates the M0 deploy lifecycle:
// preflight → transfer → pre-release → release → verify → finalize.
package engine

import (
	"context"
	"io"
	"os"
	"time"

	ctypes "github.com/compose-spec/compose-go/v2/types"

	"github.com/labstack/onebox/internal/config"
	"github.com/labstack/onebox/internal/transport"
	"github.com/labstack/onebox/internal/ui"
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
	// HTTPTimeout bounds runner-side url verify checks (default 10s).
	HTTPTimeout time.Duration
	// LockTTL is the deploy lock's freshness window on the host clock
	// (default 10m); the heartbeat touches at TTL/10.
	LockTTL time.Duration
	// NoRollback: verify failures always halt, never auto-rollback.
	NoRollback bool
	// ForceLock breaks a held, unexpired lock (prints the holder first).
	ForceLock bool
	// GitSHA and ConfigHash ride into the journal and lock metadata.
	GitSHA     string
	ConfigHash string
	// DeployPrecondition runs after this runner owns the lock and fence but
	// before any journaled deployment effect. State-bound adapters use it to
	// close the observation-to-lock race.
	DeployPrecondition func(context.Context, *Engine) error
	// UI is the output layer; built from Out+Verbose when unset (cmd shares
	// one instance so the command log and narrative interleave in order).
	UI *ui.UI
}

type Engine struct {
	Cfg     *config.Config
	Project *ctypes.Project
	T       transport.Transport
	Opts    Options
	ui      *ui.UI

	// fenceVal is "<deploy-id> <epoch>" once WriteFence has stamped the host;
	// mutate() guards every mutating command with it.
	fenceVal      string
	lockVal       string
	hostLockVal   string
	hostLockToken string
	// gateOpen is the explicit no-effect result; rollbackCovered also includes
	// the interrupted deploy's typed policy promises. Resume restores both from
	// the journal. They are closed by default — fail safe.
	gateOpen        bool
	rollbackCovered bool // aggregate explicit/policy coverage for every effect attempt
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
	if o.HTTPTimeout == 0 {
		o.HTTPTimeout = 10 * time.Second
	}
	if o.UI == nil {
		o.UI = ui.New(o.Out, o.Verbose)
	}
	return &Engine{Cfg: cfg, Project: p, T: t, Opts: o, ui: o.UI}
}

func (e *Engine) logf(format string, a ...any) {
	e.ui.Infof(format, a...)
}

func (e *Engine) warnf(format string, a ...any) {
	e.ui.Warnf(format, a...)
}

// sleepBusy is a labeled Sleep — the deliberate protocol waits (converge
// windows, drain bleeds) show as a spinner instead of dead air.
func (e *Engine) sleepBusy(label string, d time.Duration) {
	if d <= 0 {
		return
	}
	_, stop := e.ui.Busy(label)
	e.Opts.Sleep(d)
	stop()
}
