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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	parsed, err := obtarget.Parse(addr)
	if err != nil {
		return nil, fmt.Errorf("target %q: %w", addr, err)
	}
	user, host, port := parsed.User, parsed.Host, parsed.Port
	if user == "" {
		user = os.Getenv("USER")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	hk, err := knownhosts.New(filepath.Join(home, ".ssh", "known_hosts"))
	if err != nil {
		return nil, fmt.Errorf("known_hosts (required — ob never skips host verification): %w", err)
	}
	auths, diag := sshAuths(home, os.Getenv("SSH_AUTH_SOCK"))
	if len(auths) == 0 {
		msg := "no usable SSH auth found (need an ssh-agent identity or ~/.ssh/id_ed25519|id_rsa)"
		if len(diag) > 0 {
			msg += ": " + strings.Join(diag, "; ")
		}
		return nil, errors.New(msg)
	}
	config := &ssh.ClientConfig{
		User:            user,
		Auth:            auths,
		HostKeyCallback: hk,
		// Ask the server for the host-key TYPE we actually have pinned. Without
		// this the client negotiates its own default (often ecdsa/rsa) which may
		// differ from what known_hosts holds (OpenSSH's TOFU writes a single
		// ed25519 line), and knownhosts then reports a spurious "key mismatch".
		HostKeyAlgorithms: knownHostKeyAlgos(hk, net.JoinHostPort(host, port)),
	}
	address := net.JoinHostPort(host, port)
	conn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("ssh %s@%s:%s: %w", user, host, port, err)
	}
	handshakeDeadline := time.Now().Add(15 * time.Second)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(handshakeDeadline) {
		handshakeDeadline = deadline
	}
	_ = conn.SetDeadline(handshakeDeadline)
	cancelWatch := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-cancelWatch:
		}
	}()
	sshConn, channels, requests, err := ssh.NewClientConn(conn, address, config)
	close(cancelWatch)
	if err != nil {
		_ = conn.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("ssh %s@%s:%s: %w", user, host, port, err)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		_ = sshConn.Close()
		return nil, ctxErr
	}
	_ = conn.SetDeadline(time.Time{})
	client := ssh.NewClient(sshConn, channels, requests)
	return &SSH{
		client: client, user: user, host: host, port: port,
		target: parsed.Destination(user),
	}, nil
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
func sshAuths(home, authSock string) (auths []ssh.AuthMethod, diag []string) {
	if authSock != "" {
		conn, err := net.Dial("unix", authSock)
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
		if ee, ok := err.(*ssh.ExitError); ok {
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
	return uploadWithClient(ctx, &sshUploadClient{client: s.client}, localDir, remoteDir)
}

type uploadClient interface {
	NewSession() (uploadSession, error)
	Close() error
}

type sshUploadClient struct {
	client *ssh.Client
}

func (c *sshUploadClient) NewSession() (uploadSession, error) {
	sess, err := c.client.NewSession()
	if err != nil {
		return nil, err
	}
	return &sshUploadSession{Session: sess}, nil
}

func (c *sshUploadClient) Close() error { return c.client.Close() }

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
func (s *SSH) SSHPort() string     { return s.port }
func (s *SSH) Close() error        { return s.client.Close() }
