package engine

import (
	"context"
	"fmt"
	"strings"
)

// Verify runs host-side checks against the container network — never through
// the edge (design §04: an edge blip must not fail a healthy release).
func (e *Engine) Verify(ctx context.Context) error {
	for _, chk := range e.Cfg.Verify {
		role, ok := e.Cfg.Roles[chk.Role]
		if !ok {
			return fmt.Errorf("verify: unknown role %q", chk.Role)
		}
		id, err := e.containerID(ctx, role.Service)
		if err != nil {
			return err
		}
		if id == "" {
			return fmt.Errorf("verify %s: no running container", chk.Role)
		}
		switch {
		case chk.HTTP != "":
			port := chk.Port
			if port == 0 && role.Ready != nil {
				port = role.Ready.Port
			}
			if port == 0 {
				return fmt.Errorf("verify %s: no port (set verify.port or the role's ready.port)", chk.Role)
			}
			res, err := e.T.Run(ctx, "docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}' "+id)
			if err != nil {
				return err
			}
			fields := strings.Fields(res.Stdout)
			if len(fields) == 0 {
				return fmt.Errorf("verify %s: container %s has no network address", chk.Role, id)
			}
			ip := fields[0]
			if strings.ContainsAny(ip, ";|&$`'\"") {
				return fmt.Errorf("verify %s: suspicious address %q", chk.Role, ip)
			}
			cres, err := e.T.Run(ctx, fmt.Sprintf("curl -fsS -m 5 http://%s:%d%s", ip, port, chk.HTTP))
			if err != nil {
				return err
			}
			if cres.ExitCode != 0 {
				return fmt.Errorf("verify %s: GET %s -> exit %d %s", chk.Role, chk.HTTP, cres.ExitCode, strings.TrimSpace(cres.Stderr))
			}
		case chk.Exec != "":
			res, err := e.T.Run(ctx, "docker exec "+id+" sh -c "+q(chk.Exec))
			if err != nil {
				return err
			}
			if res.ExitCode != 0 {
				return fmt.Errorf("verify %s: exec failed (%d): %s", chk.Role, res.ExitCode, strings.TrimSpace(res.Stderr))
			}
		}
		e.logf("verify %s: ok", chk.Role)
	}
	return nil
}
