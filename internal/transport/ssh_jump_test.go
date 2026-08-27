package transport

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	obtarget "github.com/labstack/onebox/internal/target"
)

func obtargetRoute(target, jump obtarget.Address) obtarget.Route {
	return obtarget.Route{Target: target, Jump: &jump}
}

func TestRouteWithAJumpReachesTheTargetThroughIt(t *testing.T) {
	home, clientKey := sshTestHome(t)
	jump := newTestSSHServer(t, clientKey, true)
	target := newTestSSHServer(t, clientKey, false)
	writeKnownHosts(t, home, jump, target)

	route := obtargetRoute(addressOf(t, target, "root"), addressOf(t, jump, "deploy"))
	conn, err := NewSSHRoute(context.Background(), route)
	if err != nil {
		t.Fatalf("connect through jump: %v", err)
	}
	defer conn.Close()

	if connections, forwarded := jump.stats(); connections != 1 || len(forwarded) != 1 {
		t.Fatalf("jump saw %d connections and forwards %v, want one of each", connections, forwarded)
	}
	if forwarded := mustForward(t, jump); forwarded != target.addr() {
		t.Fatalf("jump forwarded to %q, want %q", forwarded, target.addr())
	}
	if connections, _ := target.stats(); connections != 1 {
		t.Fatalf("target saw %d connections, want 1", connections)
	}
}

func mustForward(t *testing.T, server *testSSHServer) string {
	t.Helper()
	_, forwarded := server.stats()
	if len(forwarded) != 1 {
		t.Fatalf("forwards = %v, want exactly one", forwarded)
	}
	return forwarded[0]
}

func TestDirectRouteStillConnectsWithoutAJump(t *testing.T) {
	home, clientKey := sshTestHome(t)
	target := newTestSSHServer(t, clientKey, false)
	writeKnownHosts(t, home, target)

	conn, err := NewSSHContext(context.Background(), "root@"+target.addr())
	if err != nil {
		t.Fatalf("direct connect: %v", err)
	}
	defer conn.Close()
	if connections, _ := target.stats(); connections != 1 {
		t.Fatalf("target saw %d connections, want 1", connections)
	}
}

// An untrusted bastion must be refused before it is ever told which private
// address to reach: the forwarding request itself discloses the target.
func TestUnknownJumpHostKeyFailsBeforeTheTargetIsNamed(t *testing.T) {
	home, clientKey := sshTestHome(t)
	jump := newTestSSHServer(t, clientKey, true)
	target := newTestSSHServer(t, clientKey, false)
	writeKnownHosts(t, home, target) // the jump is deliberately unpinned

	_, err := NewSSHRoute(context.Background(), obtargetRoute(addressOf(t, target, "root"), addressOf(t, jump, "deploy")))
	if err == nil {
		t.Fatal("connected through an unpinned jump host")
	}
	assertStage(t, err, "jump ssh", "host key")
	if _, forwarded := jump.stats(); len(forwarded) != 0 {
		t.Fatalf("jump was asked to forward to %v before its key was trusted", forwarded)
	}
	if connections, _ := target.stats(); connections != 0 {
		t.Fatalf("target saw %d connections, want 0", connections)
	}
}

func TestJumpAuthenticationFailureIsReportedAgainstTheJump(t *testing.T) {
	home, clientKey := sshTestHome(t)
	jump := newTestSSHServer(t, generateSigner(t).PublicKey(), true) // authorizes a key we do not hold
	target := newTestSSHServer(t, clientKey, false)
	writeKnownHosts(t, home, jump, target)

	_, err := NewSSHRoute(context.Background(), obtargetRoute(addressOf(t, target, "root"), addressOf(t, jump, "deploy")))
	if err == nil {
		t.Fatal("authenticated to a jump host that rejects our key")
	}
	assertStage(t, err, "jump ssh", "authenticate")
}

// A bastion that will not forward is a target-reachability problem. Reporting
// it against the jump sends the operator to fix credentials that already work.
func TestUnreachableTargetIsReportedAgainstTheTarget(t *testing.T) {
	home, clientKey := sshTestHome(t)
	jump := newTestSSHServer(t, clientKey, false) // accepts us, refuses to forward
	target := newTestSSHServer(t, clientKey, false)
	writeKnownHosts(t, home, jump, target)

	_, err := NewSSHRoute(context.Background(), obtargetRoute(addressOf(t, target, "root"), addressOf(t, jump, "deploy")))
	if err == nil {
		t.Fatal("connected through a jump host that refuses to forward")
	}
	assertStage(t, err, "target ssh", "not reachable from the jump host")
	jump.waitForClosed(t, 1)
}

func TestUnknownTargetHostKeyFailsThroughATrustedJump(t *testing.T) {
	home, clientKey := sshTestHome(t)
	jump := newTestSSHServer(t, clientKey, true)
	target := newTestSSHServer(t, clientKey, false)
	writeKnownHosts(t, home, jump) // the target is deliberately unpinned

	_, err := NewSSHRoute(context.Background(), obtargetRoute(addressOf(t, target, "root"), addressOf(t, jump, "deploy")))
	if err == nil {
		t.Fatal("a trusted jump host let an unpinned target through")
	}
	assertStage(t, err, "target ssh", "host key")
	jump.waitForClosed(t, 1)
}

func TestTargetAuthenticationFailureIsReportedAgainstTheTarget(t *testing.T) {
	home, clientKey := sshTestHome(t)
	jump := newTestSSHServer(t, clientKey, true)
	target := newTestSSHServer(t, generateSigner(t).PublicKey(), false)
	writeKnownHosts(t, home, jump, target)

	_, err := NewSSHRoute(context.Background(), obtargetRoute(addressOf(t, target, "root"), addressOf(t, jump, "deploy")))
	if err == nil {
		t.Fatal("authenticated to a target that rejects our key")
	}
	assertStage(t, err, "target ssh", "authenticate")
	jump.waitForClosed(t, 1)
}

func TestClosingAJumpedConnectionReleasesBothHops(t *testing.T) {
	home, clientKey := sshTestHome(t)
	jump := newTestSSHServer(t, clientKey, true)
	target := newTestSSHServer(t, clientKey, false)
	writeKnownHosts(t, home, jump, target)

	conn, err := NewSSHRoute(context.Background(), obtargetRoute(addressOf(t, target, "root"), addressOf(t, jump, "deploy")))
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	target.waitForClosed(t, 1)
	jump.waitForClosed(t, 1)
}

func TestCancelledContextStopsAJumpedConnection(t *testing.T) {
	home, clientKey := sshTestHome(t)
	jump := newTestSSHServer(t, clientKey, true)
	target := newTestSSHServer(t, clientKey, false)
	writeKnownHosts(t, home, jump, target)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewSSHRoute(ctx, obtargetRoute(addressOf(t, target, "root"), addressOf(t, jump, "deploy"))); !errors.Is(err, context.Canceled) {
		t.Fatalf("connect with a cancelled context = %v, want context.Canceled", err)
	}
}

func assertStage(t *testing.T, err error, stage, phase string) {
	t.Helper()
	if !strings.HasPrefix(err.Error(), stage+" ") {
		t.Fatalf("error %q does not name the %s hop", err, stage)
	}
	if !strings.Contains(err.Error(), phase) {
		t.Fatalf("error %q does not name the %q phase", err, phase)
	}
}

func TestJumpedConnectionReportsTheBastionAndTargetSeparately(t *testing.T) {
	home, clientKey := sshTestHome(t)
	jump := newTestSSHServer(t, clientKey, true)
	target := newTestSSHServer(t, clientKey, false)
	writeKnownHosts(t, home, jump, target)

	conn, err := NewSSHRoute(context.Background(), obtargetRoute(addressOf(t, target, "root"), addressOf(t, jump, "deploy")))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	host, port, _ := net.SplitHostPort(target.addr())
	if conn.Host() != host || conn.SSHPort() != port || conn.SSHUser() != "root" {
		t.Fatalf("target identity = %s@%s:%s, want root@%s", conn.SSHUser(), conn.Host(), conn.SSHPort(), target.addr())
	}
	jumpHost, jumpPort, _ := net.SplitHostPort(jump.addr())
	if want := "deploy@" + net.JoinHostPort(jumpHost, jumpPort); conn.SSHJump() != want {
		t.Fatalf("SSHJump() = %q, want %q", conn.SSHJump(), want)
	}
}

func TestDirectConnectionReportsNoJump(t *testing.T) {
	home, clientKey := sshTestHome(t)
	target := newTestSSHServer(t, clientKey, false)
	writeKnownHosts(t, home, target)

	conn, err := NewSSHContext(context.Background(), "root@"+target.addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if conn.SSHJump() != "" {
		t.Fatalf("SSHJump() = %q, want empty", conn.SSHJump())
	}
}

// A target that accepts the forwarded connection and then says nothing cannot
// be timed out with SetDeadline: an SSH channel rejects deadlines. Without an
// independent bound the CLI would wait on it forever, since an interactive
// context carries no deadline of its own.
func TestSilentTargetBehindAJumpIsBoundedByTheHandshakeTimeout(t *testing.T) {
	home, clientKey := sshTestHome(t)
	jump := newTestSSHServer(t, clientKey, true)
	silent := newSilentListener(t)
	writeKnownHosts(t, home, jump)

	previous := handshakeTimeout
	handshakeTimeout = 200 * time.Millisecond
	t.Cleanup(func() { handshakeTimeout = previous })

	host, port, err := net.SplitHostPort(silent.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	route := obtargetRoute(obtarget.Address{User: "root", Host: host, Port: port, ExplicitPort: true}, addressOf(t, jump, "deploy"))

	done := make(chan error, 1)
	go func() {
		_, err := NewSSHRoute(context.Background(), route)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a silent target completed a handshake")
		}
		if !errors.Is(err, errHandshakeAbandoned) {
			t.Fatalf("error = %v, want the handshake timeout", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("connect to a silent target never returned")
	}
}

// newSilentListener accepts connections and never speaks.
func newSilentListener(t *testing.T) net.Listener {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = conn.Close() })
		}
	}()
	return listener
}

// An IPv6 target is bracketed in known_hosts and in the dialled address but
// never in the SSH destination, and the two hops must agree on that.
func TestJumpAndTargetOnIPv6Loopback(t *testing.T) {
	home, clientKey := sshTestHome(t)
	jump := newTestSSHServerOn(t, "[::1]:0", clientKey, true)
	target := newTestSSHServerOn(t, "[::1]:0", clientKey, false)
	writeKnownHosts(t, home, jump, target)

	conn, err := NewSSHRoute(context.Background(), obtargetRoute(addressOf(t, target, "root"), addressOf(t, jump, "deploy")))
	if err != nil {
		t.Fatalf("connect over IPv6: %v", err)
	}
	defer conn.Close()
	if conn.Host() != "::1" {
		t.Fatalf("Host() = %q, want the unbracketed literal", conn.Host())
	}
	if forwarded := mustForward(t, jump); forwarded != target.addr() {
		t.Fatalf("jump forwarded to %q, want %q", forwarded, target.addr())
	}
}

// When the bastion itself stops answering, releasing the tunnel is only a
// message sent through the connection that has stopped answering. The hop-2
// handshake must still be bounded, or `ob` waits on it forever.
func TestHandshakeIsBoundedWhenTheJumpStopsAnswering(t *testing.T) {
	home, clientKey := sshTestHome(t)
	jump := newTestSSHServer(t, clientKey, true)
	proxy := newWedgeProxy(t, jump.addr())
	silent := newSilentListener(t)
	writeKnownHostsFor(t, home, proxy.addr(), jump.hostKey.PublicKey())

	previous := handshakeTimeout
	handshakeTimeout = 500 * time.Millisecond
	t.Cleanup(func() { handshakeTimeout = previous })

	route := obtargetRoute(addressAt(t, silent.Addr().String(), "root"), addressAt(t, proxy.addr(), "deploy"))
	done := make(chan error, 1)
	go func() {
		_, err := NewSSHRoute(context.Background(), route)
		done <- err
	}()
	// The forward request proves hop 1 finished, so wedging now strands hop 2.
	waitForForward(t, jump)
	proxy.wedge()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("connected through a jump host that stopped answering")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("connect never returned once the jump stopped answering")
	}
}

// The same connection, already established, must still be releasable: an
// operator pressing Ctrl-C has to get their terminal back.
func TestCloseReturnsWhenTheJumpStopsAnswering(t *testing.T) {
	home, clientKey := sshTestHome(t)
	jump := newTestSSHServer(t, clientKey, true)
	target := newTestSSHServer(t, clientKey, false)
	proxy := newWedgeProxy(t, jump.addr())
	writeKnownHostsFor(t, home, proxy.addr(), jump.hostKey.PublicKey())
	appendKnownHostsFor(t, home, target.addr(), target.hostKey.PublicKey())

	conn, err := NewSSHRoute(context.Background(), obtargetRoute(addressOf(t, target, "root"), addressAt(t, proxy.addr(), "deploy")))
	if err != nil {
		t.Fatal(err)
	}
	proxy.wedge()

	done := make(chan error, 1)
	go func() { done <- conn.Close() }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("close never returned once the jump stopped answering")
	}
}

// A timeout is not an authentication failure, and the guide tells operators
// that "authenticate" means the server refused their key.
func TestTimeoutIsNotReportedAsAnAuthenticationFailure(t *testing.T) {
	home, clientKey := sshTestHome(t)
	jump := newTestSSHServer(t, clientKey, true)
	silent := newSilentListener(t)
	writeKnownHosts(t, home, jump)

	previous := handshakeTimeout
	handshakeTimeout = 200 * time.Millisecond
	t.Cleanup(func() { handshakeTimeout = previous })

	_, err := NewSSHRoute(context.Background(), obtargetRoute(addressAt(t, silent.Addr().String(), "root"), addressOf(t, jump, "deploy")))
	if err == nil {
		t.Fatal("a silent target completed a handshake")
	}
	if strings.Contains(err.Error(), "authenticate") {
		t.Fatalf("a timeout is reported as an authentication failure: %v", err)
	}
	if !strings.HasPrefix(err.Error(), "target ssh ") {
		t.Fatalf("error %q does not name the target hop", err)
	}
}

// A cancel that arrives mid-handshake must unblock it, not only one that
// arrives before the connection is attempted.
func TestCancelDuringTheTargetHandshakeReturns(t *testing.T) {
	home, clientKey := sshTestHome(t)
	jump := newTestSSHServer(t, clientKey, true)
	silent := newSilentListener(t)
	writeKnownHosts(t, home, jump)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := NewSSHRoute(ctx, obtargetRoute(addressAt(t, silent.Addr().String(), "root"), addressOf(t, jump, "deploy")))
		done <- err
	}()
	waitForForward(t, jump)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel mid-handshake = %v, want context.Canceled", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("cancel mid-handshake never returned")
	}
}

func waitForForward(t *testing.T, server *testSSHServer) {
	t.Helper()
	for range 400 {
		if _, forwarded := server.stats(); len(forwarded) > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the jump host was never asked to forward")
}

// A deadline that expires mid-handshake must still say which hop was being
// established. The guide tells operators to read the hop off the front of the
// error, and a wedged bastion makes this the likeliest failure of all.
func TestExpiredDeadlineNamesTheHop(t *testing.T) {
	home, clientKey := sshTestHome(t)
	jump := newTestSSHServer(t, clientKey, true)
	silent := newSilentListener(t)
	writeKnownHosts(t, home, jump)

	ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()
	_, err := NewSSHRoute(ctx, obtargetRoute(addressAt(t, silent.Addr().String(), "root"), addressOf(t, jump, "deploy")))
	if err == nil {
		t.Fatal("a silent target completed a handshake")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want a deadline failure", err)
	}
	if !strings.HasPrefix(err.Error(), "target ssh ") {
		t.Fatalf("error %q does not name the target hop", err)
	}
}

// A direct TCP connection honors SetDeadline, so its read can return an I/O
// timeout just before the context publishes DeadlineExceeded. The caller's
// deadline remains the public error regardless of which wake-up wins.
func TestDirectDeadlineWinsTheTCPHandshakeRace(t *testing.T) {
	home, _ := sshTestHome(t)
	silent := newSilentListener(t)
	writeKnownHostsFor(t, home, silent.Addr().String(), generateSigner(t).PublicKey())

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := NewSSHContext(ctx, "root@"+silent.Addr().String())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("direct silent handshake = %v, want context deadline", err)
	}
	if !strings.HasPrefix(err.Error(), "ssh ") {
		t.Fatalf("error %q does not name the direct hop", err)
	}
}
