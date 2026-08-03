package onebox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	ctypes "github.com/compose-spec/compose-go/v2/types"

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/buildinfo"
	"github.com/labstack/onebox/internal/engine"
	"github.com/labstack/onebox/internal/release"
	"github.com/labstack/onebox/internal/transport"
)

type Connector func(context.Context, string) (transport.Transport, error)

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
}

type Service struct {
	configPath   string
	environment  string
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
		opts.Connect = func(ctx context.Context, target string) (transport.Transport, error) {
			return transport.NewSSHContext(ctx, target)
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
		now: opts.Now, connect: opts.Connect, entropy: opts.Entropy,
		engineOpts: opts.EngineOptions, runner: opts.Runner,
	}
}

func (s *Service) readEntropy(buf []byte) error {
	s.entropyMu.Lock()
	defer s.entropyMu.Unlock()
	_, err := io.ReadFull(s.entropy, buf)
	return err
}

func (s *Service) engine(ctx context.Context, lp *loadedProject, environment string) (*engine.Engine, func(), string, error) {
	return s.engineWith(ctx, lp, environment, nil)
}

func (s *Service) engineWith(ctx context.Context, lp *loadedProject, environment string, configure func(*engine.Options)) (*engine.Engine, func(), string, error) {
	env, err := lp.resolved.Environment(environment)
	if err != nil {
		return nil, nil, "", err
	}
	target := env.Target()
	t, err := s.connect(ctx, target)
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
	if configure != nil {
		configure(&engineOpts)
	}
	e := engine.New(lp.resolved, lp.compose, t, engineOpts)
	return e, func() { _ = t.Close() }, target, nil
}

func serviceImage(p *ctypes.Project, name string) string {
	svc, ok := p.Services[name]
	if !ok {
		return ""
	}
	return svc.Image
}

func ensureEnvironment(cfg *app.Resolved, name string) error {
	if _, err := cfg.Environment(name); err != nil {
		return fmt.Errorf("environment: %w", err)
	}
	return nil
}
