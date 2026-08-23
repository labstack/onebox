package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
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
	SSHJumpName string
	SSHUserName string
	SSHPortName string // falls back to 22 when TargetName is set
	state       map[string]string
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
	result := f.evalLocked(cmd)
	if result.ExitCode == 0 {
		f.recordInputStateLocked(cmd, stdin)
	}
	return result, nil
}

func (f *Fake) RunStream(ctx context.Context, cmd string, stdout, stderr io.Writer) error {
	res, err := f.Run(ctx, cmd)
	if err != nil {
		return err
	}
	_, _ = io.WriteString(stdout, res.Stdout)
	_, _ = io.WriteString(stderr, res.Stderr)
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
	result := f.evalLocked(cmd)
	if result.ExitCode == 0 && strings.Contains(cmd, "rm -f") && strings.Contains(cmd, "/activation.json") {
		delete(f.state, "activation")
	}
	if result.ExitCode == 0 && strings.Contains(cmd, "rm -f") && strings.Contains(cmd, "/secret-activation.json") {
		delete(f.state, "secret-activation")
	}
	return result, nil
}

// evalLocked resolves a command's canned result. Callers hold f.mu; Dynamic
// closures may read f.Commands but must never call back into f (would deadlock).
func (f *Fake) evalLocked(cmd string) Result {
	if f.Dynamic != nil {
		if res, ok := f.Dynamic(cmd); ok {
			return res
		}
	}
	if strings.Contains(cmd, "/manifest.json") && strings.Contains(cmd, "mode=%s") {
		id := releaseIDFromManifestCommand(cmd)
		if body, ok := f.state["manifest:"+id]; ok {
			return Result{Stdout: "mode=600\n" + body}
		}
		return Result{ExitCode: 3}
	}
	if strings.Contains(cmd, "/activation.json") && strings.Contains(cmd, "mode=%s") {
		if body, ok := f.state["activation"]; ok {
			return Result{Stdout: "mode=600\n" + body}
		}
		return Result{ExitCode: 3}
	}
	if strings.Contains(cmd, "/secret-activation.json") && strings.Contains(cmd, "mode=%s") {
		if body, ok := f.state["secret-activation"]; ok {
			return Result{Stdout: "mode=600\n" + body}
		}
		return Result{ExitCode: 3}
	}
	for _, r := range f.Script {
		if r.Match.MatchString(cmd) {
			return r.Result
		}
	}
	// Engine epoch probes default to an absent file on a fresh fake host. Tests
	// can override this default through Dynamic or Script.
	if strings.HasPrefix(cmd, ": ob-epoch-probe;") {
		return Result{ExitCode: 3}
	}
	return Result{ExitCode: 0}
}

func (f *Fake) recordInputStateLocked(cmd, input string) {
	if f.state == nil {
		f.state = map[string]string{}
	}
	var envelope struct {
		SchemaVersion string `json:"schema_version"`
		ID            string `json:"id"`
		ReleaseID     string `json:"release_id"`
	}
	if json.Unmarshal([]byte(input), &envelope) != nil {
		return
	}
	switch {
	case envelope.SchemaVersion == "onebox.run/release-manifest/v1alpha1" && envelope.ID != "" && strings.Contains(cmd, "/manifest.json.tmp"):
		f.state["manifest:"+envelope.ID] = input
	case envelope.SchemaVersion == "onebox.run/activation-checkpoint/v1alpha1" && strings.Contains(cmd, "/activation.json.tmp"):
		f.state["activation"] = input
	case envelope.SchemaVersion == "onebox.run/secret-checkpoint/v1alpha1" && envelope.ReleaseID != "" && strings.Contains(cmd, "/secret-activation.json.tmp"):
		f.state["secret-activation"] = input
	}
}

func releaseIDFromManifestCommand(cmd string) string {
	const marker = "/releases/"
	start := strings.Index(cmd, marker)
	if start < 0 {
		return ""
	}
	rest := cmd[start+len(marker):]
	end := strings.Index(rest, "/manifest.json")
	if end < 0 {
		return ""
	}
	return rest[:end]
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
func (f *Fake) SSHJump() string { return f.SSHJumpName }

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
