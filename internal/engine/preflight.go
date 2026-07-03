package engine

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/labstack/yeet/internal/release"
)

const minDiskKiB = 1 << 20 // 1 GiB

// Preflight asserts the host is deployable. Nothing mutates except mkdir -p
// of yeet's own base dir (design §04: "nothing mutates").
func (e *Engine) Preflight(ctx context.Context) error {
	if res, err := e.T.Run(ctx, "docker version -f '{{.Server.Version}}'"); err != nil || res.ExitCode != 0 {
		return fmt.Errorf("docker daemon unreachable on %s: %v %s", e.T.Host(), err, res.Stderr)
	}
	if res, err := e.T.Run(ctx, "docker compose version --short"); err != nil || res.ExitCode != 0 {
		return fmt.Errorf("docker compose plugin missing on %s", e.T.Host())
	}
	base := release.PathsFor(e.Cfg.App).Base
	if res, err := e.T.Run(ctx, "mkdir -p "+q(base)+" && test -w "+q(base)); err != nil || res.ExitCode != 0 {
		return fmt.Errorf("%s not writable by deploy user", base)
	}
	res, err := e.T.Run(ctx, "df -Pk "+q(base)+" | awk 'NR==2{print $4}'")
	if err != nil {
		return err
	}
	if kib, _ := strconv.Atoi(strings.TrimSpace(res.Stdout)); kib > 0 && kib < minDiskKiB {
		return fmt.Errorf("disk headroom %d KiB < 1 GiB on %s", kib, e.T.Host())
	}
	for _, acc := range e.Cfg.Accessories {
		id, err := e.containerID(ctx, acc)
		if err != nil {
			return err
		}
		if id == "" {
			return fmt.Errorf("accessory %q not running — start it first (bootstrap/accessory apply are M1+)", acc)
		}
		health, err := e.healthOf(ctx, id)
		if err != nil {
			return err
		}
		switch health {
		case "healthy":
		case "none":
			e.logf("warn: accessory %q has no healthcheck; asserting running-only", acc)
		default:
			return fmt.Errorf("accessory %q is %s, refusing to deploy", acc, health)
		}
	}
	return nil
}

// validID: container ids are hex (docker) or alnum test doubles; anything
// else is never interpolated back into a shell command (injection rule).
var validID = regexp.MustCompile(`^[0-9a-zA-Z]{1,64}$`)

// containerID returns the newest running container for a compose service.
func (e *Engine) containerID(ctx context.Context, svc string) (string, error) {
	ids, err := e.containerIDs(ctx, svc)
	if err != nil || len(ids) == 0 {
		return "", err
	}
	return ids[0], nil
}

func (e *Engine) containerIDs(ctx context.Context, svc string) ([]string, error) {
	res, err := e.T.Run(ctx,
		"docker ps -q --filter label=com.docker.compose.project="+e.Cfg.App+
			" --filter label=com.docker.compose.service="+svc)
	if err != nil {
		return nil, err
	}
	return splitIDs(res.Stdout)
}

func splitIDs(out string) ([]string, error) {
	var ids []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			if !validID.MatchString(l) {
				return nil, fmt.Errorf("suspicious container id %q from docker ps — refusing to reuse in a command", l)
			}
			ids = append(ids, l)
		}
	}
	return ids, nil
}

func (e *Engine) healthOf(ctx context.Context, id string) (string, error) {
	res, err := e.T.Run(ctx,
		"docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "+id)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Stdout), nil
}

func q(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
