package onebox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	ctypes "github.com/compose-spec/compose-go/v2/types"

	"github.com/labstack/onebox/internal/compose"
	"github.com/labstack/onebox/internal/config"
)

type composeLoader func(context.Context, string, string, ...string) (*ctypes.Project, error)

type loadedProject struct {
	config       *config.Config
	project      *ctypes.Project
	configPath   string
	composePath  string
	configBytes  []byte
	composeBytes []byte
}

func loadProject(ctx context.Context, configPath string, lenient bool) (*loadedProject, error) {
	absConfig, err := filepath.Abs(configPath)
	if err != nil {
		return nil, fmt.Errorf("resolve config path: %w", err)
	}
	loader := composeLoader(compose.Load)
	if lenient {
		loader = compose.LoadLenient
	}

	cfgBytes, err := os.ReadFile(absConfig)
	if err != nil {
		return nil, err
	}
	var cfg *config.Config
	if strings.HasSuffix(absConfig, ".cue") {
		cfg, err = config.LoadCUEBytes(cfgBytes, absConfig)
	} else {
		cfg, err = config.LoadBytes(cfgBytes, absConfig)
	}
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(absConfig)
	if cfg.App == "" {
		cfg.App = config.DefaultApp(absConfig)
	}
	if cfg.Compose == "" {
		cfg.Compose = config.FindCompose(dir)
	}
	composePath := cfg.Compose
	if !filepath.IsAbs(composePath) {
		composePath = filepath.Join(dir, composePath)
	}
	composePath, err = filepath.Abs(composePath)
	if err != nil {
		return nil, fmt.Errorf("resolve compose path: %w", err)
	}
	composeDir := filepath.Dir(composePath)
	var envFiles []string
	for _, ef := range cfg.EnvFiles {
		abs := ef
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(composeDir, ef)
		}
		if rel, relErr := filepath.Rel(composeDir, abs); relErr != nil || strings.HasPrefix(rel, "..") {
			return nil, fmt.Errorf("env_files: %q resolves outside the project (%s) — it must live with the compose file so it ships with the release", ef, composeDir)
		}
		envFiles = append(envFiles, abs)
	}
	composeBytes, err := os.ReadFile(composePath)
	if err != nil {
		return nil, err
	}
	p, err := loader(ctx, composePath, cfg.App, envFiles...)
	if err != nil {
		return nil, err
	}
	compose.Infer(cfg, p)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := compose.Classify(p, cfg); err != nil {
		return nil, err
	}
	composeAfter, err := os.ReadFile(composePath)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(composeBytes, composeAfter) {
		return nil, fmt.Errorf("compose changed while it was being loaded; retry the operation")
	}
	return &loadedProject{
		config: cfg, project: p, configPath: absConfig, composePath: composePath,
		configBytes: cfgBytes, composeBytes: composeBytes,
	}, nil
}
