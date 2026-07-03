package transport

import (
	"context"
	"fmt"
	"io"
	"regexp"
)

type Rule struct {
	Match  *regexp.Regexp
	Result Result
}

// Fake is the test double: Dynamic (if set) answers first, then Script rules
// first-match-wins, then a default exit-0 Result. Every Run is recorded.
type Fake struct {
	Script     []Rule
	Dynamic    func(cmd string) (Result, bool)
	Commands   []string
	Inputs     []string // stdin passed to RunInput calls
	Uploads    []string
	HostName   string
	TargetName string // full user@host; falls back to HostName
}

func (f *Fake) RunInput(ctx context.Context, cmd, stdin string) (Result, error) {
	f.Inputs = append(f.Inputs, stdin)
	return f.Run(ctx, cmd)
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
	f.Commands = append(f.Commands, cmd)
	if f.Dynamic != nil {
		if res, ok := f.Dynamic(cmd); ok {
			return res, nil
		}
	}
	for _, r := range f.Script {
		if r.Match.MatchString(cmd) {
			return r.Result, nil
		}
	}
	return Result{ExitCode: 0}, nil
}

func (f *Fake) Upload(_ context.Context, localDir, remoteDir string) error {
	f.Uploads = append(f.Uploads, fmt.Sprintf("%s -> %s", localDir, remoteDir))
	return nil
}

func (f *Fake) Host() string {
	if f.HostName == "" {
		return "fake"
	}
	return f.HostName
}

func (f *Fake) Target() string {
	if f.TargetName != "" {
		return f.TargetName
	}
	return f.Host()
}

func (f *Fake) Close() error { return nil }
