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
	"io"
	"os/exec"
	"strings"
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
	// Target is the normalized configured destination (user@host[:port]) — what a hook
	// needs to reach the same host, so nothing is hardcoded in ob.yml.
	Target() string
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

func (l *Local) Upload(ctx context.Context, localDir, remoteDir string) error {
	_, err := l.Run(ctx, "mkdir -p "+shq(remoteDir)+" && cp -a "+shq(localDir)+"/. "+shq(remoteDir)+"/")
	return err
}

func (l *Local) Host() string   { return "local" }
func (l *Local) Target() string { return "local" }
func (l *Local) Close() error   { return nil }

// shq single-quotes a shell argument.
func shq(s string) string {
	b := []byte{'\''}
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			b = append(b, '\'', '\\', '\'', '\'', '\'')
		} else {
			b = append(b, s[i])
		}
	}
	return string(append(b, '\''))
}
