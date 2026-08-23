package target

import "net"

// String renders the canonical `[user@]host[:port]` form. The port appears
// only when the author wrote one, so a target that never named a port keeps
// the exact spelling every plan and approval already carries. An IPv6 literal
// is bracketed only alongside a port, where its own colons would otherwise be
// read as the port separator.
func (a Address) String() string {
	host := a.Host
	if a.ExplicitPort {
		host = net.JoinHostPort(host, a.Port)
	}
	if a.User == "" {
		return host
	}
	return a.User + "@" + host
}

// Route is one deployment target and, optionally, the single jump host the
// connection is tunnelled through. One hop is structural: a Route holds an
// Address, not another Route, so no configuration can describe a chain.
type Route struct {
	Target Address
	Jump   *Address
}

// String names the whole connection. Without a jump it is the target alone,
// so a direct route reads exactly as it did before jump hosts existed and
// approvals sealed against the old spelling still verify.
func (r Route) String() string {
	if r.Jump == nil {
		return r.Target.String()
	}
	return r.Target.String() + " via " + r.Jump.String()
}
