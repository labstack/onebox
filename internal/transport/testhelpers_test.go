package transport

import (
	"context"
	"fmt"

	obtarget "github.com/labstack/onebox/internal/target"
)

// Helpers only tests call. They wrap or compose production code, and live
// here so the shipped binary carries no function nothing reaches.

// ParseAddr splits [user@]host[:port]; port defaults to 22.
func ParseAddr(addr string) (user, host, port string) {
	parsed, err := obtarget.Parse(addr)
	if err != nil {
		return "", "", ""
	}
	return parsed.User, parsed.Host, parsed.Port
}

// NewSSHContext dials and completes the SSH handshake within the caller's
// cancellation/deadline. A bounded fallback prevents an MCP tool from hanging
// indefinitely when its target is unreachable or stops during handshake.
func NewSSHContext(ctx context.Context, addr string) (*SSH, error) {
	parsed, err := obtarget.Parse(addr)
	if err != nil {
		return nil, fmt.Errorf("target %q: %w", addr, err)
	}
	return NewSSHRoute(ctx, obtarget.Route{Target: parsed})
}
