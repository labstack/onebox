package transport

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"

	obtarget "github.com/labstack/onebox/internal/target"
)

// SSH is the production transport: agentless, key-auth only, host keys
// verified against ~/.ssh/known_hosts and never skipped.
type SSH struct {
	client *ssh.Client
	user   string
	host   string
	target string // user@host — valid as an OpenSSH destination
	port   string // separate because user@host:port is invalid for both tools
	Logger func(host, cmd string)

	// jumpClient and jumpTCP are the bastion hop, nil on a direct connection.
	// The raw TCP conn is kept because closing the target client behind a jump
	// only writes a channel-close message *through* the bastion: if the
	// bastion is what is wedged, that write blocks and nothing is released.
	// Closing this conn is what actually unblocks the stack.
	jumpClient *ssh.Client
	jumpTCP    net.Conn
	jump       string // [user@]host[:port] of the bastion, empty when direct
}

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

// NewSSHRoute connects to the route's target, tunnelling through its jump host
// when one is declared. Both hops are verified against known_hosts and
// authenticated independently; the local agent may sign for either, but its
// socket is never forwarded, so a compromised bastion cannot borrow the
// operator's identity. Exactly one hop is possible — a Route holds an address,
// not another route.
func NewSSHRoute(ctx context.Context, route obtarget.Route) (*SSH, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	hostKeys, err := knownhosts.New(filepath.Join(home, ".ssh", "known_hosts"))
	if err != nil {
		return nil, fmt.Errorf("known_hosts (required — ob never skips host verification): %w", err)
	}
	// Gathered once for the whole route rather than per hop: sshAuths leaves
	// the agent connection open for the handshake callback to re-list, so a
	// second call would open a second agent connection and duplicate every
	// diagnostic.
	auths, diag := sshAuths(ctx, home, os.Getenv("SSH_AUTH_SOCK"))
	if len(auths) == 0 {
		msg := "no usable SSH auth found (need an ssh-agent identity or ~/.ssh/id_ed25519|id_rsa)"
		if len(diag) > 0 {
			msg += ": " + strings.Join(diag, "; ")
		}
		return nil, errors.New(msg)
	}

	target := resolveUser(route.Target)
	if route.Jump == nil {
		conn, err := dialTCP(ctx, target)
		if err != nil {
			return nil, hopError(stageDirect, target, phaseDial, err)
		}
		client, err := sshHandshake(ctx, conn, target, auths, hostKeys, stageDirect, nil)
		if err != nil {
			return nil, err
		}
		return newSSHFromClient(client, target, nil, nil, nil), nil
	}

	jump := resolveUser(*route.Jump)
	jumpTCP, err := dialTCP(ctx, jump)
	if err != nil {
		return nil, hopError(stageJump, jump, phaseDial, err)
	}
	jumpClient, err := sshHandshake(ctx, jumpTCP, jump, auths, hostKeys, stageJump, nil)
	if err != nil {
		return nil, err
	}
	// The tunnel is opened by the bastion's sshd, which applies its own
	// policy and its own connect timeout; neither is ours to trust, so the
	// dial carries an explicit bound of its own.
	tunnelCtx, cancelTunnel := context.WithTimeout(ctx, dialTimeout)
	defer cancelTunnel()
	tunnel, err := jumpClient.DialContext(tunnelCtx, "tcp", net.JoinHostPort(target.Host, target.Port))
	if err != nil {
		_ = jumpTCP.Close()
		_ = jumpClient.Close()
		return nil, hopError(stageTarget, target, phaseTunnel, err)
	}
	client, err := sshHandshake(ctx, tunnel, target, auths, hostKeys, stageTarget, func() { _ = jumpTCP.Close() })
	if err != nil {
		_ = tunnel.Close()
		_ = jumpTCP.Close()
		_ = jumpClient.Close()
		return nil, err
	}
	return newSSHFromClient(client, target, &jump, jumpClient, jumpTCP), nil
}

func newSSHFromClient(client *ssh.Client, address obtarget.Address, jump *obtarget.Address, jumpClient *ssh.Client, jumpTCP net.Conn) *SSH {
	s := &SSH{
		client: client, user: address.User, host: address.Host, port: address.Port,
		target:     address.Destination(address.User),
		jumpClient: jumpClient, jumpTCP: jumpTCP,
	}
	if jump != nil {
		s.jump = jump.String()
	}
	return s
}

// resolveUser fills the SSH user the same way OpenSSH's default does when the
// author named none.
func resolveUser(address obtarget.Address) obtarget.Address {
	if address.User == "" {
		address.User = os.Getenv("USER")
	}
	return address
}

const dialTimeout = 10 * time.Second

// handshakeTimeout bounds a hop that connects but never completes its
// handshake. It is a variable so tests can shorten it: this bound is the only
// thing that stops such a hop behind a jump host, because an SSH channel
// refuses SetDeadline outright.
var handshakeTimeout = 15 * time.Second

func dialTCP(ctx context.Context, address obtarget.Address) (net.Conn, error) {
	return (&net.Dialer{Timeout: dialTimeout}).DialContext(ctx, "tcp", net.JoinHostPort(address.Host, address.Port))
}

// sshHandshake authenticates one hop over an already-established connection and
// verifies its host key. conn is a TCP connection for a direct target or a
// bastion, and an SSH channel for a target behind one; the handshake is
// identical either way, which is what makes both hops verified rather than one
// inheriting the other's trust.
func sshHandshake(ctx context.Context, conn net.Conn, address obtarget.Address, auths []ssh.AuthMethod, hostKeys ssh.HostKeyCallback, stage string, hardClose func()) (*ssh.Client, error) {
	dialed := net.JoinHostPort(address.Host, address.Port)
	config := &ssh.ClientConfig{
		User:            address.User,
		Auth:            auths,
		HostKeyCallback: hostKeys,
		// Ask the server for the host-key TYPE we actually have pinned. Without
		// this the client negotiates its own default (often ecdsa/rsa) which may
		// differ from what known_hosts holds (OpenSSH's TOFU writes a single
		// ed25519 line), and knownhosts then reports a spurious "key mismatch".
		HostKeyAlgorithms: knownHostKeyAlgos(hostKeys, dialed),
	}
	handshakeDeadline := time.Now().Add(handshakeTimeout)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(handshakeDeadline) {
		handshakeDeadline = deadline
	}
	// Best effort: a TCP conn reports a clean i/o timeout this way, but an SSH
	// channel refuses deadlines outright ("ssh: tcpChan: deadline not
	// supported"), so the timer below is what actually bounds the second hop.
	_ = conn.SetDeadline(handshakeDeadline)
	timeout := time.NewTimer(time.Until(handshakeDeadline))
	defer timeout.Stop()
	// The watcher and the handshake race to decide the connection's fate, so
	// they settle it under one lock rather than by signalling after the fact.
	// Signalling after closing leaves a window in which the handshake has
	// already returned a client whose connection is being torn down — the
	// failure this guard exists to prevent, arriving opaquely at first use.
	var settle sync.Mutex
	var abandoned, established bool
	cancelWatch := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
		case <-timeout.C:
		case <-cancelWatch:
			return
		}
		settle.Lock()
		defer settle.Unlock()
		if established {
			return
		}
		abandoned = true
		_ = conn.Close()
		// Closing an SSH channel only sends a message through the hop below
		// it, so a channel whose bastion has stopped answering stays blocked
		// on a read that will never complete. hardClose drops the connection
		// that message would have travelled on, which is the only close that
		// still lands.
		if hardClose != nil {
			hardClose()
		}
	}()
	sshConn, channels, requests, err := ssh.NewClientConn(conn, dialed, config)
	close(cancelWatch)
	settle.Lock()
	established = err == nil
	gaveUp := abandoned
	settle.Unlock()
	if err != nil {
		_ = conn.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, hopError(stage, address, phaseNone, ctxErr)
		}
		return nil, hopError(stage, address, phaseOf(err), err)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		_ = sshConn.Close()
		return nil, hopError(stage, address, phaseNone, ctxErr)
	}
	if gaveUp {
		_ = sshConn.Close()
		return nil, hopError(stage, address, phaseNone, errHandshakeAbandoned)
	}
	_ = conn.SetDeadline(time.Time{})
	return ssh.NewClient(sshConn, channels, requests), nil
}

var errHandshakeAbandoned = errors.New("connection closed while completing the handshake")

// Connection failures are reported by the hop they happened on and the phase
// that failed, so an operator can tell a bastion they cannot reach from one
// they cannot authenticate to, and either from a target the bastion refused to
// forward them to.
const (
	stageDirect = "ssh"
	stageJump   = "jump ssh"
	stageTarget = "target ssh"
)

const (
	phaseNone    = ""
	phaseDial    = ""
	phaseTunnel  = "not reachable from the jump host"
	phaseHostKey = "host key"
	phaseAuth    = "authenticate"
)

// phaseOf names a handshake failure only when it can identify one. A timeout,
// a cancelled dial, or a peer that hung up are none of the named phases, and
// labelling them "authenticate" sends the operator to check a key that is
// fine.
func phaseOf(err error) string {
	var keyErr *knownhosts.KeyError
	var revoked *knownhosts.RevokedError
	switch {
	case errors.As(err, &keyErr), errors.As(err, &revoked):
		return phaseHostKey
	case isTransportFailure(err):
		return phaseNone
	default:
		return phaseAuth
	}
}

func isTransportFailure(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) || errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errHandshakeAbandoned)
}

func hopError(stage string, address obtarget.Address, phase string, err error) error {
	where := fmt.Sprintf("%s %s@%s:%s", stage, address.User, address.Host, address.Port)
	if phase == "" {
		return fmt.Errorf("%s: %w", where, err)
	}
	return fmt.Errorf("%s: %s: %w", where, phase, err)
}

// sshAuths gathers publickey auth methods from the ssh-agent and on-disk keys.
// Alongside the methods it returns a diagnostic for every source it found but
// could not use — an empty agent, a passphrase-encrypted key — which the caller
// surfaces only when NO method is usable. That turns what the server would
// otherwise report as a bare "handshake failed" into an actionable message.
//
// ob deliberately does not prompt for or keychain-decrypt on-disk keys: an
// encrypted key is usable only via the agent. The empty-agent
// guard matters because a reachable-but-empty agent's Signers callback offers
// nothing yet would still make len(auths) non-zero, masking that diagnostic.
func sshAuths(ctx context.Context, home, authSock string) (auths []ssh.AuthMethod, diag []string) {
	if authSock != "" {
		conn, err := (&net.Dialer{}).DialContext(ctx, "unix", authSock)
		if err != nil {
			diag = append(diag, fmt.Sprintf("SSH_AUTH_SOCK set but agent unreachable: %v", err))
		} else {
			ag := agent.NewClient(conn)
			switch signers, err := ag.Signers(); {
			case err != nil:
				conn.Close()
				diag = append(diag, fmt.Sprintf("ssh-agent error: %v", err))
			case len(signers) == 0:
				conn.Close()
				diag = append(diag, "ssh-agent has no identities (try: ssh-add ~/.ssh/id_ed25519)")
			default:
				// conn stays open — the callback re-lists at handshake time.
				auths = append(auths, ssh.PublicKeysCallback(ag.Signers))
			}
		}
	}
	for _, name := range []string{"id_ed25519", "id_rsa"} {
		path := filepath.Join(home, ".ssh", name)
		b, err := os.ReadFile(path)
		if err != nil {
			// A missing key is normal; a present-but-unreadable one (wrong owner,
			// mode 000) is the actionable case the generic "no auth" would hide.
			if !errors.Is(err, os.ErrNotExist) {
				diag = append(diag, fmt.Sprintf("%s: unreadable: %v", path, err))
			}
			continue
		}
		signer, err := ssh.ParsePrivateKey(b)
		if err != nil {
			var need *ssh.PassphraseMissingError
			if errors.As(err, &need) {
				diag = append(diag, fmt.Sprintf("%s is passphrase-encrypted — ob can't decrypt on-disk keys; load it into your agent: ssh-add %s", path, path))
			} else {
				diag = append(diag, fmt.Sprintf("%s: %v", path, err))
			}
			continue
		}
		auths = append(auths, ssh.PublicKeys(signer))
	}
	return auths, diag
}

// knownHostKeyAlgos returns the host-key algorithms pinned for addr in
// known_hosts. x/crypto's knownhosts.New doesn't expose this, so we probe the
// callback with a key that can't match: it answers with a *knownhosts.KeyError
// whose Want lists the known keys, and their types are the algorithms to
// request. Returns nil for an unknown host (no pins) so the client falls back
// to its defaults and the callback still rejects the connection.
func knownHostKeyAlgos(cb ssh.HostKeyCallback, addr string) []string {
	var ke *knownhosts.KeyError
	err := cb(addr, &net.TCPAddr{IP: net.IPv4zero}, probeKey{})
	if !errors.As(err, &ke) || len(ke.Want) == 0 {
		return nil
	}
	var algos []string
	seen := map[string]bool{}
	add := func(a string) {
		if !seen[a] {
			seen[a] = true
			algos = append(algos, a)
		}
	}
	for _, k := range ke.Want {
		t := k.Key.Type()
		// A pinned RSA key can also verify the SHA-2 signature algorithms, which
		// modern servers prefer; offer those first so negotiation succeeds.
		if t == ssh.KeyAlgoRSA {
			add(ssh.KeyAlgoRSASHA256)
			add(ssh.KeyAlgoRSASHA512)
		}
		add(t)
	}
	return algos
}

// probeKey is a public key that matches nothing — used only to elicit the
// KeyError whose Want lists the pinned host keys.
type probeKey struct{}

func (probeKey) Type() string                        { return "ob-probe" }
func (probeKey) Marshal() []byte                     { return []byte("ob-probe") }
func (probeKey) Verify([]byte, *ssh.Signature) error { return errors.New("probe") }

func (s *SSH) Run(ctx context.Context, cmd string) (Result, error) {
	return s.RunInput(ctx, cmd, "")
}

func (s *SSH) RunInput(ctx context.Context, cmd, stdin string) (Result, error) {
	if s.Logger != nil {
		s.Logger(s.host, cmd)
	}
	sess, err := s.client.NewSession()
	if err != nil {
		return Result{}, err
	}
	defer sess.Close()
	var out, errb strings.Builder
	sess.Stdout, sess.Stderr = &out, &errb
	if stdin != "" {
		sess.Stdin = strings.NewReader(stdin)
	}
	done := make(chan error, 1)
	go func() { done <- sess.Run(cmd) }()
	select {
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGKILL)
		return Result{}, ctx.Err()
	case err = <-done:
	}
	res := Result{Stdout: out.String(), Stderr: errb.String()}
	if err != nil {
		var ee *ssh.ExitError
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitStatus()
			return res, nil
		}
		return res, err
	}
	return res, nil
}

func (s *SSH) RunStream(ctx context.Context, cmd string, stdout, stderr io.Writer) error {
	if s.Logger != nil {
		s.Logger(s.host, cmd)
	}
	sess, err := s.client.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	sess.Stdout, sess.Stderr = stdout, stderr
	done := make(chan error, 1)
	go func() { done <- sess.Run(cmd) }()
	select {
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGKILL)
		return ctx.Err()
	case err := <-done:
		return err
	}
}

// Upload streams the staging dir as tar.gz into `tar -xzf -` on the host —
// no scp/sftp dependency, one round trip.
func (s *SSH) Upload(ctx context.Context, localDir, remoteDir string) error {
	if s.Logger != nil {
		s.Logger(s.host, "upload "+localDir+" -> "+remoteDir)
	}
	return uploadWithClient(ctx, &sshUploadClient{client: s.client, closeAll: s.Close}, localDir, remoteDir)
}

type uploadClient interface {
	NewSession() (uploadSession, error)
	Close() error
}

type sshUploadClient struct {
	client *ssh.Client
	// closeAll releases the jump hop too. Cancellation relies on closing the
	// connection to unblock a wedged transfer, and behind a bastion the target
	// client alone is not that connection.
	closeAll func() error
}

func (c *sshUploadClient) NewSession() (uploadSession, error) {
	sess, err := c.client.NewSession()
	if err != nil {
		return nil, err
	}
	return &sshUploadSession{Session: sess}, nil
}

func (c *sshUploadClient) Close() error {
	if c.closeAll != nil {
		return c.closeAll()
	}
	return c.client.Close()
}

type uploadSession interface {
	StdinPipe() (io.WriteCloser, error)
	Start(string) error
	Wait() error
	Close() error
	setStderr(io.Writer)
}

type sshUploadSession struct {
	*ssh.Session
}

func (s *sshUploadSession) setStderr(w io.Writer) { s.Stderr = w }

// uploadWithClient installs cancellation before NewSession and force-closes the
// underlying SSH connection. A channel-level Close is only a protocol message
// and a wedged peer need not acknowledge it; closing the connection is what
// guarantees that session creation, writes, and Wait all unblock.
func uploadWithClient(ctx context.Context, client uploadClient, localDir, remoteDir string) error {
	if err := ctx.Err(); err != nil {
		_ = client.Close()
		return err
	}
	stopCancel := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer stopCancel()
	sess, err := client.NewSession()
	if err != nil {
		return uploadError(ctx, err)
	}
	return uploadWithSession(ctx, sess, localDir, remoteDir)
}

// uploadDrainTimeout bounds how long an aborted upload waits for the remote to
// report what it did. The wait exists to collect the remote's stderr, which is
// worth a pause but never worth hanging the CLI. A var so the test that proves
// the wait is bounded does not have to take this long to run.
var uploadDrainTimeout = 30 * time.Second

// tarTransfer is the receiving half of an SSH upload: it extracts the streamed
// archive into staging and refuses to hand back a payload the sender did not
// finish.
//
// The remote cannot tell a truncated archive from a complete one on its own.
// `tar` rejects a stream that stops mid-entry, because the gzip trailer is then
// missing — but an archive of *zero* bytes is not malformed. bsdtar/libarchive
// exits 0 on empty stdin (measured; GNU tar exits 2 and busybox 1), and
// gzip.Writer emits its header only on the first Write, so a walk that fails
// before the first entry sends nothing at all. Without a sentinel that pair
// extracts nothing, exits 0, and publishes an empty directory as a complete
// release. The sentinel is written last, so its presence is the sender's
// statement that the walk finished, and it does not depend on which tar the
// host ships.
func tarTransfer(staging string) string {
	// staging arrives shell-quoted and uploadSentinel is a fixed literal with no
	// metacharacters, so concatenating outside the quotes is safe.
	marker := staging + "/" + uploadSentinel
	return "tar -xzf - -C " + staging +
		" && { [ -f " + marker + " ] || { echo 'upload payload is incomplete: the archive was truncated' >&2; exit 1; }; }" +
		" && rm -f " + marker
}

// uploadWithSession streams the archive after uploadWithClient has installed
// the connection-level cancellation guard.
func uploadWithSession(ctx context.Context, sess uploadSession, localDir, remoteDir string) error {
	if err := ctx.Err(); err != nil {
		_ = sess.Close()
		return err
	}
	defer sess.Close()

	pw, err := sess.StdinPipe()
	if err != nil {
		return uploadError(ctx, err)
	}
	var errb strings.Builder
	sess.setStderr(&errb)
	script, err := uploadScript(remoteDir, tarTransfer)
	if err != nil {
		_ = pw.Close()
		return uploadError(ctx, err)
	}
	if err := sess.Start(script); err != nil {
		_ = pw.Close()
		return uploadError(ctx, err)
	}
	gz := gzip.NewWriter(pw)
	tw := tar.NewWriter(gz)
	walkErr := filepath.Walk(localDir, func(path string, info os.FileInfo, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(localDir, path)
		if err != nil || rel == "." {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(tw, &contextReader{ctx: ctx, r: f})
			closeErr := f.Close()
			if copyErr != nil {
				return copyErr
			}
			return closeErr
		}
		return nil
	})
	if walkErr != nil {
		// Do NOT finalise the archive, and do NOT write the sentinel. tw.Close
		// writes a valid tar EOF marker and gz.Close a valid gzip trailer, so the
		// remote tar would see a well-formed archive of whatever arrived and exit
		// 0. Truncating is what makes the remote fail for a non-empty stream; the
		// missing sentinel is what makes it fail for an empty one.
		_ = pw.Close()
		waitDone := make(chan error, 1)
		go func() { waitDone <- sess.Wait() }()
		select {
		case waitErr := <-waitDone:
			if waitErr != nil {
				return uploadCause(ctx, fmt.Errorf("%w (remote: %s)", walkErr, strings.TrimSpace(errb.String())))
			}
			// The remote exited 0 on a stream that was deliberately truncated. The
			// sentinel check should make this unreachable; if it happens anyway the
			// destination may hold a partial payload, and `ob resume` reads a
			// directory that exists as a finished transfer.
			return uploadCause(ctx, fmt.Errorf("%w — the remote reported success for a transfer that did not finish; "+
				"%s may hold an incomplete payload and must be removed before retrying", walkErr, remoteDir))
		case <-time.After(uploadDrainTimeout):
			// Only a context cancellation closes the connection, and walkErr is
			// usually not a context error, so without this the CLI would wait on a
			// wedged peer forever with nothing printed.
			return uploadCause(ctx, fmt.Errorf("%w — the remote did not report a result within %s, so the state of %s is unknown",
				walkErr, uploadDrainTimeout, remoteDir))
		}
	}
	// Written last so the remote can distinguish a complete payload from a
	// truncated one; the receiving script deletes it before the move.
	if err := tw.WriteHeader(&tar.Header{Name: uploadSentinel, Mode: 0o600, Typeflag: tar.TypeReg}); err != nil {
		return uploadError(ctx, err)
	}
	closeErr := closeUploadWriters(tw, gz, pw)
	if closeErr != nil {
		return uploadError(ctx, closeErr)
	}
	if err := sess.Wait(); err != nil {
		return uploadError(ctx, fmt.Errorf("remote untar: %w (%s)", err, strings.TrimSpace(errb.String())))
	}
	return ctx.Err()
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func closeUploadWriters(tw *tar.Writer, gz *gzip.Writer, pw io.WriteCloser) error {
	var first error
	for _, closeFn := range []func() error{tw.Close, gz.Close, pw.Close} {
		if err := closeFn(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func uploadError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
}

// uploadCause keeps both facts. uploadError answers "was this a cancellation?"
// by discarding what actually failed, which is right where the cause is a
// symptom of the cancellation and wrong where it is not: a file the walk cannot
// read would report itself as context.Canceled the moment the operator presses
// Ctrl-C, losing the reason for the failed deploy. Joining keeps
// errors.Is(err, context.Canceled) true and still names the cause.
func uploadCause(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return errors.Join(err, ctxErr)
	}
	return err
}

func (s *SSH) Host() string        { return s.host }
func (s *SSH) Destination() string { return s.target }
func (s *SSH) SSHUser() string     { return s.user }
func (s *SSH) SSHJump() string     { return s.jump }
func (s *SSH) SSHPort() string     { return s.port }

// Close releases the whole connection in reverse order. The jump's TCP conn is
// closed last and unconditionally: it is the only close that cannot be
// swallowed by a wedged bastion.
func (s *SSH) Close() error {
	if s.jumpTCP == nil {
		return s.client.Close()
	}
	return closeStack(s.jumpTCP, closeGrace, s.client, s.jumpClient)
}

// closeGrace bounds how long a graceful shutdown may take before the jump's
// connection is dropped underneath it. A clean close is one round trip; only a
// bastion that has stopped answering takes longer.
const closeGrace = 2 * time.Second

// closeStack closes each graceful closer in order, then the jump host's raw
// connection. Every graceful close writes through that connection, so if the
// bastion has stopped moving bytes they block; dropping the connection is what
// unblocks them, which makes it a backstop rather than a formality. An
// already-closed connection is the normal case — closing the jump client
// closes it — so that is not reported as a failure.
func closeStack(raw io.Closer, grace time.Duration, graceful ...io.Closer) error {
	done := make(chan error, 1)
	go func() {
		errs := make([]error, 0, len(graceful))
		for _, closer := range graceful {
			errs = append(errs, closer.Close())
		}
		done <- errors.Join(errs...)
	}()
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case err := <-done:
		return errors.Join(err, ignoreClosed(raw.Close()))
	case <-timer.C:
		// The goroutine is released by this close and is not waited for: it
		// cannot be, since waiting is the thing that was blocked.
		return ignoreClosed(raw.Close())
	}
}

func ignoreClosed(err error) error {
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}
