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

// stagingRoot is the directory a transfer lands in before it is visible under
// its real name. It is a hidden directory beside the destination — NOT outside
// the destination's parent, which an earlier version of this comment claimed.
//
// For a release that means `releases/.uploads/<id>`, i.e. inside the very
// directory `release.list` enumerates with `ls -1`. Two things keep it from
// being read as a release, and both are load-bearing: `ls -1` does not list
// dot-entries, and `release.IsID` rejects the leading dot. Staging is not
// somewhere nothing looks; it is somewhere two specific filters exclude.
//
// A first attempt put staging at `<remoteDir>.partial`, which had neither
// protection: the debris was a plain entry in `releases/`, so `Previous()`
// handed it to `ob rollback` and retention counted it and evicted a real
// release to keep it.
const stagingRoot = ".uploads"

func stagingPath(remoteDir string) string {
	return path.Join(path.Dir(remoteDir), stagingRoot, path.Base(remoteDir))
}

// uploadSentinel is written as the final archive entry by transports that
// stream, so the receiver can tell a complete payload from a truncated one.
// See uploadScript.
const uploadSentinel = ".ob-upload-complete"

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
//
// Staging is removed when the script exits or is signalled. It has to be
// removed here rather than
// by the caller, because this is the only place that knows the staging path:
// the secrets, protection-credential and proxy uploads all clean up the
// *destination* they asked for, so anything left beside it survives them.
// Their payloads are plaintext — an app's .env, a protection credentials.env —
// and the leaf name carries an epoch or a fence token that changes every run,
// so a leak is never overwritten by the next attempt.
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
	//
	// Checking again after the transfer narrows the window but does not close
	// it: a destination created between the second check and the `mv` still
	// gets the payload nested inside it. What actually prevents that is the
	// host lock — one operation per app — plus destinations that are unique per
	// operation. This is a guard against a stale directory, not against a
	// concurrent writer.
	return strings.Join([]string{
		"if [ -e " + target + " ]; then echo 'upload destination already exists: refusing to replace it' >&2; exit 1; fi",
		"rm -rf " + staging + " || exit 1",
		"mkdir -p " + staging + " || exit 1",
		// The handler is quoted twice on purpose. `staging` is already
		// single-quoted for the shell that reads this script; wrapping it in a
		// second pair of literal quotes would close that quoting rather than nest
		// it, leaving the path bare — a destination holding `;` then ran as a
		// command on the target host, and one merely holding a space produced
		// `trap: invalid signal specification` and installed no handler at all.
		// shq applied to the whole handler escapes the inner quotes so the string
		// survives to the shell that evaluates it when the trap fires.
		//
		// EXIT alone does not cover a killed shell: cancelling closes the SSH
		// connection, sshd sends SIGHUP, and an untrapped signal skips the EXIT
		// handler — which is exactly the interrupted transfer whose payload most
		// needs removing.
		"trap " + shq("rm -rf "+staging) + " EXIT HUP INT TERM",
		transfer(staging) + " || exit 1",
		"mkdir -p " + shq(path.Dir(remoteDir)) + " || exit 1",
		"if [ -e " + target + " ]; then echo 'upload destination appeared during transfer' >&2; exit 1; fi",
		"mv " + staging + " " + target + " || exit 1",
		"trap - EXIT",
	}, "\n"), nil
}
