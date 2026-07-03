package transport

import (
	"context"
	"fmt"
	"regexp"
)

type Rule struct {
	Match  *regexp.Regexp
	Result Result
}

// Fake is the test double: Dynamic (if set) answers first, then Script rules
// first-match-wins, then a default exit-0 Result. Every Run is recorded.
type Fake struct {
	Script   []Rule
	Dynamic  func(cmd string) (Result, bool)
	Commands []string
	Uploads  []string
	HostName string
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

func (f *Fake) Close() error { return nil }
