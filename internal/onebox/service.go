package onebox

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"time"

	ctypes "github.com/compose-spec/compose-go/v2/types"

	"github.com/labstack/onebox/internal/config"
	"github.com/labstack/onebox/internal/engine"
	"github.com/labstack/onebox/internal/transport"
)

type Connector func(context.Context, string) (transport.Transport, error)

type Options struct {
	ConfigPath  string
	Environment string
	Now         func() time.Time
	Connect     Connector
	Entropy     io.Reader
}

type Service struct {
	configPath  string
	environment string
	now         func() time.Time
	connect     Connector
	entropy     io.Reader
	entropyMu   sync.Mutex
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
	return &Service{
		configPath: opts.ConfigPath, environment: opts.Environment,
		now: opts.Now, connect: opts.Connect, entropy: opts.Entropy,
	}
}

func (s *Service) readEntropy(buf []byte) error {
	s.entropyMu.Lock()
	defer s.entropyMu.Unlock()
	_, err := io.ReadFull(s.entropy, buf)
	return err
}

func (s *Service) engine(ctx context.Context, lp *loadedProject, environment string) (*engine.Engine, func(), string, error) {
	env, err := lp.config.Environment(environment)
	if err != nil {
		return nil, nil, "", err
	}
	target := env.Hosts[0]
	t, err := s.connect(ctx, target)
	if err != nil {
		return nil, nil, "", err
	}
	e := engine.New(lp.config, lp.project, t, engine.Options{
		Out:        io.Discard,
		LocalDir:   filepath.Dir(lp.configPath),
		Now:        s.now,
		ConfigHash: engine.HashBytes(lp.configBytes),
		GitSHA:     gitShortSHA(ctx, filepath.Dir(lp.configPath)),
	})
	return e, func() { _ = t.Close() }, target, nil
}

func resolvedReplicas(role config.Role) int { return role.Count() }

func serviceImage(p *ctypes.Project, name string) string {
	svc, ok := p.Services[name]
	if !ok {
		return ""
	}
	return svc.Image
}

func ensureEnvironment(cfg *config.Config, name string) error {
	if _, err := cfg.Environment(name); err != nil {
		return fmt.Errorf("environment: %w", err)
	}
	return nil
}
