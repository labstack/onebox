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

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// SSH is the production transport: agentless, key-auth only, host keys
// verified against ~/.ssh/known_hosts — never skipped (design §11).
type SSH struct {
	client *ssh.Client
	host   string
	target string // user@host — the full ssh/rsync destination
	Logger func(host, cmd string)
}

// ParseAddr splits [user@]host[:port]; port defaults to 22.
func ParseAddr(addr string) (user, host, port string) {
	port = "22"
	if i := strings.Index(addr, "@"); i >= 0 {
		user, addr = addr[:i], addr[i+1:]
	}
	if h, p, err := net.SplitHostPort(addr); err == nil {
		return user, h, p
	}
	return user, addr, port
}

func NewSSH(addr string) (*SSH, error) {
	user, host, port := ParseAddr(addr)
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
	var auths []ssh.AuthMethod
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			auths = append(auths, ssh.PublicKeysCallback(agent.NewClient(conn).Signers))
		}
	}
	for _, name := range []string{"id_ed25519", "id_rsa"} {
		if b, err := os.ReadFile(filepath.Join(home, ".ssh", name)); err == nil {
			if signer, err := ssh.ParsePrivateKey(b); err == nil {
				auths = append(auths, ssh.PublicKeys(signer))
			}
		}
	}
	if len(auths) == 0 {
		return nil, fmt.Errorf("no SSH auth available (agent or ~/.ssh/id_ed25519|id_rsa)")
	}
	client, err := ssh.Dial("tcp", net.JoinHostPort(host, port), &ssh.ClientConfig{
		User:            user,
		Auth:            auths,
		HostKeyCallback: hk,
		// Ask the server for the host-key TYPE we actually have pinned. Without
		// this the client negotiates its own default (often ecdsa/rsa) which may
		// differ from what known_hosts holds (OpenSSH's TOFU writes a single
		// ed25519 line), and knownhosts then reports a spurious "key mismatch".
		HostKeyAlgorithms: knownHostKeyAlgos(hk, net.JoinHostPort(host, port)),
	})
	if err != nil {
		return nil, fmt.Errorf("ssh %s@%s:%s: %w", user, host, port, err)
	}
	return &SSH{client: client, host: host, target: user + "@" + host}, nil
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

func (s *SSH) RunStream(ctx context.Context, cmd string, out io.Writer) error {
	if s.Logger != nil {
		s.Logger(s.host, cmd)
	}
	sess, err := s.client.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	sess.Stdout, sess.Stderr = out, out
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
	sess, err := s.client.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	pw, err := sess.StdinPipe()
	if err != nil {
		return err
	}
	var errb strings.Builder
	sess.Stderr = &errb
	if err := sess.Start("mkdir -p " + shq(remoteDir) + " && tar -xzf - -C " + shq(remoteDir)); err != nil {
		return err
	}
	gz := gzip.NewWriter(pw)
	tw := tar.NewWriter(gz)
	walkErr := filepath.Walk(localDir, func(path string, info os.FileInfo, err error) error {
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
			defer f.Close()
			_, err = io.Copy(tw, f)
			return err
		}
		return nil
	})
	if walkErr != nil {
		return walkErr
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	if err := pw.Close(); err != nil {
		return err
	}
	if err := sess.Wait(); err != nil {
		return fmt.Errorf("remote untar: %w (%s)", err, strings.TrimSpace(errb.String()))
	}
	_ = ctx
	return nil
}

func (s *SSH) Host() string   { return s.host }
func (s *SSH) Target() string { return s.target }
func (s *SSH) Close() error   { return s.client.Close() }
