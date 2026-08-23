package app

import (
	"strings"
	"testing"
)

// A route without a jump must render exactly what Destination() renders:
// plans and approvals are sealed against that spelling, so any drift would
// invalidate every artifact created before jump hosts existed.
func TestRouteWithoutAJumpRendersTheDestinationVerbatim(t *testing.T) {
	servers := map[string]Server{
		"user and port":  {User: "deploy", Host: "example.com", Port: 2222},
		"port only":      {Host: "example.com", Port: 2222},
		"no port":        {User: "root", Host: "example.com"},
		"ipv6 with port": {User: "root", Host: "2a01:4ff::1", Port: 2222},
		"ipv6 no port":   {User: "root", Host: "2a01:4ff::1"},
	}
	for name, server := range servers {
		t.Run(name, func(t *testing.T) {
			e := Environment{Server: server}
			if got, want := e.Route().String(), e.Destination(); got != want {
				t.Fatalf("route = %q, destination = %q", got, want)
			}
		})
	}
}

func TestRouteWithoutAJumpCarriesNoJumpAddress(t *testing.T) {
	route := Environment{Server: Server{Host: "example.com"}}.Route()
	if route.Jump != nil {
		t.Fatalf("route.Jump = %#v, want nil", route.Jump)
	}
	if route.Target.Port != "22" || route.Target.ExplicitPort {
		t.Fatalf("target = %#v, want the implicit default port", route.Target)
	}
}

func TestRouteNamesTheDeclaredJump(t *testing.T) {
	e := Environment{
		Server: Server{User: "root", Host: "10.20.0.10"},
		Jump:   &Jump{User: "deploy", Host: "bastion.example.com", Port: 2222},
	}
	if route := e.Route(); route.Jump == nil {
		t.Fatal("route.Jump = nil, want the declared bastion")
	}
	want := "root@10.20.0.10 via deploy@bastion.example.com:2222"
	if got := e.Route().String(); got != want {
		t.Fatalf("route = %q, want %q", got, want)
	}
}

func TestRouteJumpDefaultsToPort22(t *testing.T) {
	e := Environment{
		Server: Server{User: "root", Host: "10.20.0.10"},
		Jump:   &Jump{Host: "bastion.example.com"},
	}
	route := e.Route()
	if route.Jump.Port != "22" || route.Jump.ExplicitPort {
		t.Fatalf("jump = %#v, want the implicit default port", route.Jump)
	}
	if got, want := route.String(), "root@10.20.0.10 via bastion.example.com"; got != want {
		t.Fatalf("route = %q, want %q", got, want)
	}
}

// `server: root@host:2222` is a scalar the loader has always accepted. The
// port has to survive into the route, or the connection is attempted against
// a hostname with a colon in it.
func TestScalarServerPortReachesTheRoute(t *testing.T) {
	resolved, err := LoadBytes([]byte("api_version: onebox.run/v1\napp: ledger\n"+
		"environments: {production: {server: root@10.20.0.10:2222}}\n"+
		"image: nginx\ndomain: d.example.com\nport: 8080\n"), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	environment := resolved.Environments["production"]
	route := environment.Route()
	if route.Target.Host != "10.20.0.10" || route.Target.Port != "2222" {
		t.Fatalf("target = %#v, want host 10.20.0.10 port 2222", route.Target)
	}
	if got, want := route.String(), environment.Destination(); got != want {
		t.Fatalf("route = %q, destination = %q", got, want)
	}
	if want := "root@10.20.0.10:2222"; route.String() != want {
		t.Fatalf("route = %q, want %q", route.String(), want)
	}
}

// A bracketed IPv6 scalar now normalises to the same address the object form
// produces: brackets belong to the written grammar, not to the hostname. The
// rendered destination therefore drops them when no port is written, exactly
// as `{host: "2001:db8::1"}` always has.
func TestBracketedIPv6ScalarNormalisesLikeTheObjectForm(t *testing.T) {
	load := func(server string) Environment {
		t.Helper()
		resolved, err := LoadBytes([]byte("api_version: onebox.run/v1\napp: ledger\n"+
			"environments: {production: {server: "+server+"}}\n"+
			"image: nginx\ndomain: d.example.com\nport: 8080\n"), "ob.yml")
		if err != nil {
			t.Fatal(err)
		}
		return resolved.Environments["production"]
	}
	scalar := load(`"root@[2001:db8::1]"`)
	object := load(`{host: "2001:db8::1", user: root}`)
	if scalar.Server != object.Server {
		t.Fatalf("scalar = %#v, object = %#v", scalar.Server, object.Server)
	}
	if got := scalar.Route().String(); got != "root@2001:db8::1" {
		t.Fatalf("route = %q, want %q", got, "root@2001:db8::1")
	}
	withPort := load(`"root@[2001:db8::1]:2222"`)
	if got := withPort.Route().String(); got != "root@[2001:db8::1]:2222" {
		t.Fatalf("route = %q, want the bracketed form when a port is written", got)
	}
}

// A bracketed IPv6 `server.host` was dialable before routes existed, because
// every connection re-parsed the rendered destination. Nothing re-parses it
// now, so the brackets have to come off while the project is read or
// JoinHostPort builds [[2001:db8::1]]:22.
func TestBracketedIPv6ServerHostNormalises(t *testing.T) {
	resolved, err := LoadBytes([]byte("api_version: onebox.run/v1\napp: ledger\n"+
		"environments: {production: {server: {host: \"[2001:db8::1]\", user: root}}}\n"+
		"image: nginx\ndomain: d.example.com\nport: 8080\n"), "ob.yml")
	if err != nil {
		t.Fatal(err)
	}
	environment := resolved.Environments["production"]
	if environment.Server.Host != "2001:db8::1" {
		t.Fatalf("server host = %q, want the bare literal", environment.Server.Host)
	}
	if got := environment.Route().Target.Host; got != "2001:db8::1" {
		t.Fatalf("route host = %q, want the bare literal", got)
	}
}

// A server address the transport could never dial should be reported against
// the field the author wrote, not as a DNS lookup failure at connect time.
func TestInvalidServerAddressIsRejectedAtLoad(t *testing.T) {
	invalid := map[string]string{
		"host with a port": `{host: "example.com:2222"}`,
		"host with a user": `{host: "root@example.com"}`,
		"bad user":         `{host: example.com, user: "bad/user"}`,
	}
	for name, server := range invalid {
		t.Run(name, func(t *testing.T) {
			_, err := LoadBytes([]byte("api_version: onebox.run/v1\napp: ledger\n"+
				"environments: {production: {server: "+server+"}}\n"+
				"image: nginx\ndomain: d.example.com\nport: 8080\n"), "ob.yml")
			if err == nil {
				t.Fatalf("server %q was accepted", server)
			}
			if !strings.Contains(err.Error(), "server") {
				t.Fatalf("error does not name the server field: %v", err)
			}
		})
	}
}
