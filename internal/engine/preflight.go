package engine

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/labstack/onebox/internal/release"
)

const minDiskKiB = 1 << 20 // 1 GiB

// Preflight asserts the host is deployable. Nothing mutates except mkdir -p
// of Onebox's own base directory; preflight never mutates the target.
func (e *Engine) Preflight(ctx context.Context) error {
	if res, err := e.T.Run(ctx, "docker version -f '{{.Server.Version}}'"); err != nil || res.ExitCode != 0 {
		return fmt.Errorf("docker daemon unreachable on %s: %v %s", e.T.Host(), err, res.Stderr)
	}
	if res, err := e.T.Run(ctx, "docker compose version --short"); err != nil || res.ExitCode != 0 {
		return fmt.Errorf("docker compose plugin missing on %s", e.T.Host())
	}
	base := release.PathsFor(e.App.App).Base
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
	if e.App.Proxy.Managed {
		ids, err := e.proxyContainerIDs(ctx)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return fmt.Errorf("managed proxy not running on %s — run `ob bootstrap` (first contact) or `ob proxy apply`", e.T.Host())
		}
		if h, err := e.healthOf(ctx, ids[0]); err != nil {
			return err
		} else if h != "healthy" {
			return fmt.Errorf("managed proxy is %s, refusing to deploy (ob proxy apply to converge it)", h)
		}
	}
	for _, acc := range e.App.ServiceNames() {
		id, err := e.containerID(ctx, acc)
		if err != nil {
			return err
		}
		if id == "" {
			return fmt.Errorf("accessory %q not running — start it with `ob bootstrap` or `ob accessory apply`", acc)
		}
		health, err := e.healthOf(ctx, id)
		if err != nil {
			return err
		}
		switch health {
		case "healthy":
		case "none":
			e.warnf("accessory %q has no healthcheck; asserting running-only", acc)
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
		"docker ps -q --filter label=com.docker.compose.project="+q(e.App.App)+
			" --filter label=com.docker.compose.service="+q(svc))
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("docker ps for service %q failed (exit %d): %s", svc, res.ExitCode, strings.TrimSpace(res.Stderr))
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

// svcContainer is one container's status as docker ps reports it (a "down"
// container is present but not serving, so not strictly running).
type svcContainer struct {
	id      string
	release string // the ob.release label ("" for a non-ob container)
	health  string // healthy | unhealthy | starting | none | down (not running)
}

// projectContainers reads every running container in the app's compose project
// — id, service, ob.release label, and health — in ONE docker ps, grouped by
// service in docker's newest-first order. This is the whole of status's app
// side: `docker ps` already carries the release label and a health hint in
// `.Status`, so no per-container `docker inspect` is needed. (A single batched
// inspect emitting BOTH the ob.release label and health was tried and can't be
// relied on: over multiple containers, a template that reads .Config.Labels and
// guards .State.Health errors — "map has no entry for key Health" — on any
// container without a healthcheck. A single-id health inspect is fine, but that
// puts us back to one round trip per container.)
func (e *Engine) projectContainers(ctx context.Context) (map[string][]svcContainer, error) {
	res, err := e.T.Run(ctx,
		"docker ps --filter label=com.docker.compose.project="+q(e.App.App)+
			" --format '{{.ID}}|{{.Label \"com.docker.compose.service\"}}|{{.Label \"ob.release\"}}|{{.Status}}'")
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("docker ps failed (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	byService := map[string][]svcContainer{}
	for _, line := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 4)
		if len(parts) != 4 || parts[1] == "" {
			continue // blank line, or a container with no compose-service label
		}
		id := parts[0]
		// ids aren't reused in a command anymore, but validate defensively so a
		// future caller can't reintroduce injection through this map.
		if !validID.MatchString(id) {
			return nil, fmt.Errorf("suspicious container id %q from docker ps — refusing to reuse in a command", id)
		}
		byService[parts[1]] = append(byService[parts[1]], svcContainer{
			id: id, release: parts[2], health: healthFromStatus(parts[3]),
		})
	}
	return byService, nil
}

// healthFromStatus maps a `docker ps` .Status string to a health word. For a
// running container it mirrors `docker inspect .State.Health.Status`
// (healthy | unhealthy | starting), and "none" for one with no healthcheck.
// A container that is present but NOT serving — Restarting (crash-looping) or
// Paused — reports "down": status treats anything other than healthy/none as a
// problem, so a container flapping AFTER a deploy is surfaced rather than
// mistaken for a healthy no-healthcheck one (both were "none" before).
//
// The callers run `docker ps` without -a, so in practice only Up/Restarting/
// Paused reach here; a fully Exited/Created/Dead container drops out of ps and
// surfaces as NOT RUNNING (len==0). Those states are mapped defensively anyway.
func healthFromStatus(status string) string {
	switch {
	case strings.Contains(status, "(healthy)"):
		return "healthy"
	case strings.Contains(status, "(unhealthy)"):
		return "unhealthy"
	case strings.Contains(status, "health: starting"):
		return "starting"
	case strings.Contains(status, "(Paused)"): // up but not serving; must precede the Up prefix
		return "down"
	case strings.HasPrefix(strings.TrimSpace(status), "Up"):
		return "none" // running, no healthcheck
	default:
		return "down" // Restarting (crash loop) / Exited / Created / Dead / Removing — not serving
	}
}

func q(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
