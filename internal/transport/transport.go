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
	"path"
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
	script, err := uploadScript(remoteDir, func(staging string) string {
		return "cp -a " + shq(localDir) + "/. " + staging + "/"
	})
	if err != nil {
		return err
	}
	res, err := l.Run(ctx, script)
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

// stagingRoot is where a transfer lands before it is visible under its real
// name. It is a sibling of the application directory, deliberately NOT beside
// the destination.
//
// A first attempt at this put staging at `<remoteDir>.partial`, which for a
// release meant inside `releases/`. That directory is enumerated with `ls -1`
// and every entry is taken to be a release id, so the debris became a release:
// `Previous()` handed it to `ob rollback`, and retention counted it and evicted
// a real release to keep it. Debris belongs outside any namespace something
// else enumerates.
const stagingRoot = ".uploads"

func stagingPath(remoteDir string) string {
	return path.Join(path.Dir(remoteDir), stagingRoot, path.Base(remoteDir))
}

// uploadScript wraps a transfer so an interrupted one cannot be mistaken for a
// finished one.
//
// Writing straight into the destination leaves a directory that exists and is
// incomplete, which nothing downstream can distinguish from a complete one — and
// `ob resume` decides whether a transfer finished by testing that the directory
// exists. Staging elsewhere and moving into place means the destination appears
// whole or not at all.
//
// The destination is never deleted. An earlier version ran `rm -rf <target>`
// before the move so it could replace an existing directory, which opened a
// window where the previous release was gone and the new one not yet installed —
// a worse state than either. Every caller uploads to a path that is unique per
// operation, so a destination that already exists means something is wrong;
// `mv` fails and says so rather than destroying what is there.
func uploadScript(remoteDir string, transfer func(quotedStaging string) string) (string, error) {
	// This helper exists to be safe, so it validates rather than trusting its
	// caller two packages away. Cleaning first removes the trailing slash that
	// would otherwise make staging a child of the target.
	remoteDir = path.Clean(remoteDir)
	if !path.IsAbs(remoteDir) {
		return "", fmt.Errorf("upload destination %q is not absolute", remoteDir)
	}
	if path.Dir(remoteDir) == remoteDir {
		return "", fmt.Errorf("upload destination %q is a filesystem root", remoteDir)
	}

	staging := shq(stagingPath(remoteDir))
	target := shq(remoteDir)
	// `mv a b` where b is an existing directory moves a *inside* b rather than
	// failing, which would bury the payload one level down and report success.
	// `mv -T` says what is meant but is GNU-only, so the guard is explicit.
	guard := "if [ -e " + target + " ]; then echo 'upload destination already exists: " +
		"refusing to replace it' >&2; exit 1; fi"
	return guard +
		" && mkdir -p " + shq(path.Dir(stagingPath(remoteDir))) +
		" && rm -rf " + staging +
		" && mkdir -p " + staging +
		" && " + transfer(staging) +
		" && mkdir -p " + shq(path.Dir(remoteDir)) +
		" && if [ -e " + target + " ]; then echo 'upload destination appeared during transfer' >&2; exit 1; fi" +
		" && mv " + staging + " " + target, nil
}
