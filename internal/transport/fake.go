package transport

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"sync"
)

type Rule struct {
	Match  *regexp.Regexp
	Result Result
}

// Fake is the test double: Dynamic (if set) answers first, then Script rules
// first-match-wins, then a default exit-0 Result. Every Run is recorded.
// The mutex makes it safe under concurrent Run calls — the engine fans status
// reads out over the transport, so the double must tolerate the same.
type Fake struct {
	mu      sync.Mutex
	Script  []Rule
	Dynamic func(cmd string) (Result, bool)
	// Err (if set and non-nil for a command) makes Run/RunInput return that
	// error — the only way to model a transport-level failure, since Result
	// carries an exit code but not an error.
	Err         func(cmd string) error
	Commands    []string
	Inputs      []string // stdin passed to RunInput calls
	Uploads     []string
	HostName    string
	TargetName  string // full user@host; falls back to HostName
	SSHUserName string
	SSHPortName string // falls back to 22 when TargetName is set
}

func (f *Fake) RunInput(_ context.Context, cmd, stdin string) (Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Inputs = append(f.Inputs, stdin)
	f.Commands = append(f.Commands, cmd)
	if f.Err != nil {
		if err := f.Err(cmd); err != nil {
			return Result{}, err
		}
	}
	return f.evalLocked(cmd), nil
}

func (f *Fake) RunStream(ctx context.Context, cmd string, out io.Writer) error {
	res, err := f.Run(ctx, cmd)
	if err != nil {
		return err
	}
	_, _ = io.WriteString(out, res.Stdout)
	return nil
}

func (f *Fake) Run(_ context.Context, cmd string) (Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Commands = append(f.Commands, cmd)
	if f.Err != nil {
		if err := f.Err(cmd); err != nil {
			return Result{}, err
		}
	}
	return f.evalLocked(cmd), nil
}

// evalLocked resolves a command's canned result. Callers hold f.mu; Dynamic
// closures may read f.Commands but must never call back into f (would deadlock).
func (f *Fake) evalLocked(cmd string) Result {
	if f.Dynamic != nil {
		if res, ok := f.Dynamic(cmd); ok {
			return res
		}
	}
	for _, r := range f.Script {
		if r.Match.MatchString(cmd) {
			return r.Result
		}
	}
	return Result{ExitCode: 0}
}

func (f *Fake) Upload(_ context.Context, localDir, remoteDir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Uploads = append(f.Uploads, fmt.Sprintf("%s -> %s", localDir, remoteDir))
	return nil
}

func (f *Fake) Host() string {
	if f.HostName == "" {
		return "fake"
	}
	return f.HostName
}

func (f *Fake) Destination() string {
	if f.TargetName != "" {
		return f.TargetName
	}
	return f.Host()
}

func (f *Fake) SSHUser() string { return f.SSHUserName }

func (f *Fake) SSHPort() string {
	if f.SSHPortName != "" {
		return f.SSHPortName
	}
	if f.TargetName != "" {
		return "22"
	}
	return ""
}

func (f *Fake) Close() error { return nil }
