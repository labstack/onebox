// Package transport abstracts command execution on a deploy target.
//
// Commands are shell strings by nature: SSH exec is always parsed by the
// remote shell, and Local mirrors that for parity. The safety contract lives
// one level up — every token interpolated into a command is either validated
// against a strict pattern or single-quoted (see the plan's command-injection
// rules); hooks are the documented verbatim escape hatch.
package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/labstack/onebox/internal/shellquote"
)

type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

type Transport interface {
	// Run executes cmd; err is a transport failure only — command failures
	// are reported via Result.ExitCode.
	Run(ctx context.Context, cmd string) (Result, error)
	// RunInput is Run with stdin — for secrets that must never appear in a
	// command string (docker login --password-stdin).
	RunInput(ctx context.Context, cmd, stdin string) (Result, error)
	// RunStream runs cmd with combined output streamed to out — for
	// long-running follows (logs -f) where buffering defeats the point.
	RunStream(ctx context.Context, cmd string, out io.Writer) error
	Upload(ctx context.Context, localDir, remoteDir string) error
	// Host is the bare hostname (for display, drift, and error context).
	Host() string
	// Target is the normalized OpenSSH destination (user@host). The port is
	// separate because OpenSSH does not accept user@host:port.
	Target() string
	// SSHUser is the resolved SSH username, exposed separately for tools whose
	// remote-spec grammar differs across implementations (notably IPv6 rsync).
	SSHUser() string
	// SSHPort is the target's SSH port, or empty when SSH is not applicable.
	SSHPort() string
	Close() error
}

// Local runs commands on this machine — used by e2e tests where local docker
// plays the deploy host.
type Local struct {
	Logger func(host, cmd string)
}

func NewLocal() *Local { return &Local{} }

func (l *Local) Run(ctx context.Context, cmd string) (Result, error) {
	return l.RunInput(ctx, cmd, "")
}

func (l *Local) RunInput(ctx context.Context, cmd, stdin string) (Result, error) {
	if l.Logger != nil {
		l.Logger("local", cmd)
	}
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	if stdin != "" {
		c.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	err := c.Run()
	res := Result{Stdout: out.String(), Stderr: errb.String()}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		res.ExitCode = ee.ExitCode()
		return res, nil
	}
	return res, err
}

func (l *Local) RunStream(ctx context.Context, cmd string, out io.Writer) error {
	if l.Logger != nil {
		l.Logger("local", cmd)
	}
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	c.Stdout, c.Stderr = out, out
	return c.Run()
}

// Upload copies a staged directory onto the target.
//
// Run reports a command that ran and failed through Result.ExitCode, reserving
// err for a process that could not be started at all. Reading only err therefore
// reported every failed copy as a successful upload — a full disk, an unwritable
// parent, and a killed shell all returned nil.
func (l *Local) Upload(ctx context.Context, localDir, remoteDir string) error {
	res, err := l.Run(ctx, "mkdir -p "+shq(remoteDir)+" && cp -a "+shq(localDir)+"/. "+shq(remoteDir)+"/")
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		// A cancelled context kills the shell, which arrives here as exit -1
		// with no useful status of its own; the context error says more.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		detail := strings.TrimSpace(res.Stderr)
		if detail == "" {
			detail = strings.TrimSpace(res.Stdout)
		}
		return fmt.Errorf("upload %s to %s: exit %d: %s", localDir, remoteDir, res.ExitCode, detail)
	}
	return nil
}

func (l *Local) Host() string    { return "local" }
func (l *Local) Target() string  { return "local" }
func (l *Local) SSHUser() string { return "" }
func (l *Local) SSHPort() string { return "" }
func (l *Local) Close() error    { return nil }

// shq single-quotes a shell argument.
func shq(s string) string {
	return shellquote.Quote(s)
}
