package transport

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	obtarget "github.com/labstack/onebox/internal/target"
)

// testSSHServer is an in-process sshd: enough of one to exercise host-key
// verification, publickey auth, and direct-tcpip forwarding without depending
// on anything installed on the machine running the tests.
type testSSHServer struct {
	t          *testing.T
	listener   net.Listener
	hostKey    ssh.Signer
	authorized ssh.PublicKey

	// forwardTo, when set, is dialled for every direct-tcpip channel instead
	// of the address the client asked for — the fake bastion.
	forward bool

	mu             sync.Mutex
	connections    int
	closed         int
	forwarded      []string
	rejectForwards bool
}

func (s *testSSHServer) addr() string { return s.listener.Addr().String() }

func (s *testSSHServer) stats() (connections int, forwarded []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connections, append([]string(nil), s.forwarded...)
}

// waitForClosed blocks until the server has seen want connections end, which is
// how a test observes that the client actually released the hop rather than
// leaving it open for the process to reap.
func (s *testSSHServer) waitForClosed(t *testing.T, want int) {
	t.Helper()
	for range 200 {
		s.mu.Lock()
		closed := s.closed
		s.mu.Unlock()
		if closed >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t.Fatalf("server closed %d connections, want %d", s.closed, want)
}

// newTestSSHServer starts a server that accepts only authorized and answers
// direct-tcpip when forward is set.
func newTestSSHServer(t *testing.T, authorized ssh.PublicKey, forward bool) *testSSHServer {
	t.Helper()
	return newTestSSHServerOn(t, "127.0.0.1:0", authorized, forward)
}

// newTestSSHServerOn binds an explicit address so a test can exercise the IPv6
// spelling the address grammar brackets.
func newTestSSHServerOn(t *testing.T, address string, authorized ssh.PublicKey, forward bool) *testSSHServer {
	t.Helper()
	hostKey := generateSigner(t)
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", address)
	if err != nil {
		t.Skipf("cannot listen on %s: %v", address, err)
	}
	s := &testSSHServer{t: t, listener: listener, hostKey: hostKey, authorized: authorized, forward: forward}
	t.Cleanup(func() { _ = listener.Close() })
	go s.serve()
	return s
}

func (s *testSSHServer) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.connections++
		s.mu.Unlock()
		go s.handle(conn)
	}
}

func (s *testSSHServer) handle(conn net.Conn) {
	defer func() {
		_ = conn.Close()
		s.mu.Lock()
		s.closed++
		s.mu.Unlock()
	}()
	config := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if s.authorized != nil && string(key.Marshal()) == string(s.authorized.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("unauthorized key")
		},
	}
	config.AddHostKey(s.hostKey)
	serverConn, channels, requests, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return
	}
	defer serverConn.Close()
	go ssh.DiscardRequests(requests)
	for channel := range channels {
		switch channel.ChannelType() {
		case "direct-tcpip":
			s.handleForward(channel)
		default:
			_ = channel.Reject(ssh.UnknownChannelType, channel.ChannelType())
		}
	}
}

func (s *testSSHServer) handleForward(request ssh.NewChannel) {
	var payload struct {
		Host  string
		Port  uint32
		Orig  string
		Oport uint32
	}
	if err := ssh.Unmarshal(request.ExtraData(), &payload); err != nil {
		_ = request.Reject(ssh.ConnectionFailed, "bad payload")
		return
	}
	destination := net.JoinHostPort(payload.Host, fmt.Sprint(payload.Port))
	s.mu.Lock()
	s.forwarded = append(s.forwarded, destination)
	reject := s.rejectForwards || !s.forward
	s.mu.Unlock()
	if reject {
		_ = request.Reject(ssh.ConnectionFailed, "administratively prohibited")
		return
	}
	upstream, err := (&net.Dialer{}).DialContext(s.t.Context(), "tcp", destination)
	if err != nil {
		_ = request.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	channel, requests, err := request.Accept()
	if err != nil {
		upstream.Close()
		return
	}
	go ssh.DiscardRequests(requests)
	// Either direction ending tears down both, the way a real sshd propagates
	// a channel close to the forwarded connection. Closing only the direction
	// that ended would leave the far end blocked on a read that never returns.
	teardown := func() {
		_ = channel.Close()
		_ = upstream.Close()
	}
	go func() {
		defer teardown()
		_, _ = io.Copy(upstream, channel)
	}()
	go func() {
		defer teardown()
		_, _ = io.Copy(channel, upstream)
	}()
}

func generateSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

// sshTestHome points the transport's fixed ~/.ssh lookups at a temporary
// directory holding the client key and the known_hosts under test.
func sshTestHome(t *testing.T) (home string, clientKey ssh.PublicKey) {
	t.Helper()
	home = t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := ssh.MarshalPrivateKey(private, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".ssh", "id_ed25519"), pem.EncodeToMemory(der), 0o600); err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("SSH_AUTH_SOCK", "")
	return home, signer.PublicKey()
}

// writeKnownHostsFor pins one key at an arbitrary address, for a server reached
// through something other than its own listener.
func writeKnownHostsFor(t *testing.T, home, address string, key ssh.PublicKey) {
	t.Helper()
	line := knownhosts.Line([]string{knownhosts.Normalize(address)}, key) + "\n"
	if err := os.WriteFile(filepath.Join(home, ".ssh", "known_hosts"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
}

func appendKnownHostsFor(t *testing.T, home, address string, key ssh.PublicKey) {
	t.Helper()
	path := filepath.Join(home, ".ssh", "known_hosts")
	existing, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	line := knownhosts.Line([]string{knownhosts.Normalize(address)}, key) + "\n"
	if err := os.WriteFile(path, append(existing, line...), 0o600); err != nil {
		t.Fatal(err)
	}
}

func addressAt(t *testing.T, address, user string) obtarget.Address {
	t.Helper()
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	return obtarget.Address{User: user, Host: host, Port: port, ExplicitPort: true}
}

// writeKnownHosts pins each server at its own listening address, which is what
// the transport verifies against.
func writeKnownHosts(t *testing.T, home string, servers ...*testSSHServer) {
	t.Helper()
	var lines string
	for _, server := range servers {
		lines += knownhosts.Line([]string{knownhosts.Normalize(server.addr())}, server.hostKey.PublicKey()) + "\n"
	}
	if err := os.WriteFile(filepath.Join(home, ".ssh", "known_hosts"), []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
}

func addressOf(t *testing.T, server *testSSHServer, user string) obtarget.Address {
	t.Helper()
	host, port, err := net.SplitHostPort(server.addr())
	if err != nil {
		t.Fatal(err)
	}
	return obtarget.Address{User: user, Host: host, Port: port, ExplicitPort: true}
}

// wedgeProxy forwards TCP to an upstream until it is wedged, after which it
// silently stops moving bytes in both directions without closing anything.
// That is what an unresponsive bastion looks like from the client: writes are
// accepted by the kernel, reads never complete, and no close is ever
// acknowledged. It is the only way to exercise the paths where releasing an
// SSH channel is not enough, because a channel close is just a message sent
// through the very connection that has stopped answering.
type wedgeProxy struct {
	listener net.Listener
	wedged   chan struct{}
	once     sync.Once
}

func newWedgeProxy(t *testing.T, upstream string) *wedgeProxy {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &wedgeProxy{listener: listener, wedged: make(chan struct{})}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			downstream, err := listener.Accept()
			if err != nil {
				return
			}
			up, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", upstream)
			if err != nil {
				_ = downstream.Close()
				return
			}
			go p.pump(downstream, up)
			go p.pump(up, downstream)
		}
	}()
	return p
}

func (p *wedgeProxy) addr() string { return p.listener.Addr().String() }

func (p *wedgeProxy) wedge() { p.once.Do(func() { close(p.wedged) }) }

func (p *wedgeProxy) pump(from, to net.Conn) {
	buf := make([]byte, 32*1024)
	for {
		n, err := from.Read(buf)
		if n > 0 {
			select {
			case <-p.wedged:
				var never chan struct{}
				<-never // parked: these bytes, and every later one, are never delivered
			default:
			}
			if _, werr := to.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}
