package onebox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/buildinfo"
	"github.com/labstack/onebox/internal/engine"
	"github.com/labstack/onebox/internal/release"
	"github.com/labstack/onebox/internal/shellquote"
	"github.com/labstack/onebox/internal/transport"
)

type Connector func(context.Context, transport.Route) (transport.Transport, error)

type Options struct {
	ConfigPath  string
	Environment string
	Now         func() time.Time
	Connect     Connector
	Entropy     io.Reader
	// EngineOptions configures the shared execution engine for adapters. MCP
	// leaves this zero-valued (output is discarded); the CLI supplies its UI,
	// output stream, verbosity, and break-glass flags.
	EngineOptions engine.Options
	Runner        buildinfo.Runner
	// ScheduledLifecycleExecutor is the bounded driver backend reached only
	// Images resolves build-sourced workloads to the reference whatever built
	// them produced. Production never builds, so a workload declaring `build:`
	// has no image until one is supplied here — and without it the project
	// cannot be rendered at all, let alone planned.
	Images app.Images
}

type Service struct {
	configPath   string
	environment  string
	images       app.Images
	now          func() time.Time
	connect      Connector
	entropy      io.Reader
	entropyMu    sync.Mutex
	engineOpts   engine.Options
	runner       buildinfo.Runner
	operationSeq uint64
}

func (s *Service) newOperationID(now time.Time, gitSHA string, kind OperationKind) string {
	sequence := atomic.AddUint64(&s.operationSeq, 1)
	base := release.NewID(now, gitSHA) + "-" + string(kind)
	nonce := make([]byte, 6)
	if err := s.readEntropy(nonce); err == nil {
		return base + "-" + hex.EncodeToString(nonce)
	}
	// Entropy failure must not make emergency local operations unavailable.
	// Nanoseconds plus the per-service sequence still avoid the old same-second
	// lock identity collision in the fallback path.
	return fmt.Sprintf("%s-%x-%x", base, now.UnixNano(), sequence)
}

func New(opts Options) *Service {
	if opts.ConfigPath == "" {
		opts.ConfigPath = "ob.yml"
	}
	if opts.Environment == "" {
		opts.Environment = "production"
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Connect == nil {
		opts.Connect = func(ctx context.Context, route transport.Route) (transport.Transport, error) {
			return transport.NewSSHRoute(ctx, route)
		}
	}
	if opts.Entropy == nil {
		opts.Entropy = rand.Reader
	}
	if opts.Runner.Version == "" {
		opts.Runner = CurrentRunnerProvenance()
	} else if len(opts.Runner.SupportedExecutablePlanSchemas) == 0 {
		opts.Runner.SupportedExecutablePlanSchemas = SupportedExecutableDeployPlanSchemas()
	}
	return &Service{
		configPath: opts.ConfigPath, environment: opts.Environment,
		images: opts.Images,
		now:    opts.Now, connect: opts.Connect, entropy: opts.Entropy,
		engineOpts: opts.EngineOptions, runner: opts.Runner,
	}
}

func (s *Service) readEntropy(buf []byte) error {
	s.entropyMu.Lock()
	defer s.entropyMu.Unlock()
	_, err := io.ReadFull(s.entropy, buf)
	return err
}

func (s *Service) newSecretGeneration() (string, error) {
	bytes := make([]byte, 12)
	if err := s.readEntropy(bytes); err != nil {
		return "", err
	}
	return "sg-" + hex.EncodeToString(bytes), nil
}

func (s *Service) engine(ctx context.Context, lp *loadedProject, environment string) (*engine.Engine, func(), string, error) {
	return s.engineWith(ctx, lp, environment, nil)
}

func (s *Service) engineWith(ctx context.Context, lp *loadedProject, environment string, configure func(*engine.Options)) (*engine.Engine, func(), string, error) {
	env, err := lp.resolved.Environment(environment)
	if err != nil {
		return nil, nil, "", err
	}
	route := env.Route()
	target := route.String()
	t, err := s.connect(ctx, route)
	if err != nil {
		return nil, nil, "", err
	}
	engineOpts := s.engineOpts
	if engineOpts.Out == nil {
		engineOpts.Out = io.Discard
	}
	engineOpts.LocalDir = filepath.Dir(lp.configPath)
	engineOpts.Now = s.now
	engineOpts.ConfigHash = engine.HashBytes(lp.configBytes)
	engineOpts.GitSHA = gitShortSHA(ctx, filepath.Dir(lp.configPath))
	engineOpts.Runner = s.runner
	engineOpts.Environment = environment
	if engineOpts.SecretGeneration == nil {
		engineOpts.SecretGeneration = s.newSecretGeneration
	}
	if configure != nil {
		configure(&engineOpts)
	}
	e := engine.New(lp.resolved, lp.compose, t, engineOpts)
	return e, func() { _ = t.Close() }, target, nil
}

func ensureEnvironment(cfg *app.Resolved, name string) error {
	if _, err := cfg.Environment(name); err != nil {
		return fmt.Errorf("environment: %w", err)
	}
	return nil
}

// gitShortSHA is the working tree's revision, recorded on every operation so a
// journal entry can be traced to the code that produced it. An unavailable or
// dirty revision is simply absent rather than guessed at.
func gitShortSHA(ctx context.Context, dir string) string {
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--short=7", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func noneIfEmpty(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func quote(s string) string { return shellquote.Quote(s) }
