package app

import (
	"strconv"

	obtarget "github.com/labstack/onebox/internal/target"
)

// Route is the whole connection this environment needs: the server, and the
// jump host it is reached through when one is declared. It is built from the
// declared fields rather than by reparsing Destination(), so a route without a
// jump renders byte-for-byte what Destination() has always rendered.
func (e Environment) Route() obtarget.Route {
	route := obtarget.Route{Target: address(e.Server.User, e.Server.Host, e.Server.Port)}
	if e.Jump != nil {
		jump := address(e.Jump.User, e.Jump.Host, e.Jump.Port)
		route.Jump = &jump
	}
	return route
}

// address carries the authored port through as explicit and otherwise leaves
// the SSH default implicit, which is the distinction Destination() draws and
// every sealed plan already spells.
func address(user, host string, port int) obtarget.Address {
	a := obtarget.Address{User: user, Host: host, Port: "22"}
	if port != 0 {
		a.Port, a.ExplicitPort = strconv.Itoa(port), true
	}
	return a
}
