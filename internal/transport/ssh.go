package transport

import (
	"archive/tar"
	"compress/gzip"
	"context"
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
		return nil, fmt.Errorf("known_hosts (required — yeet never skips host verification): %w", err)
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
	})
	if err != nil {
		return nil, fmt.Errorf("ssh %s@%s:%s: %w", user, host, port, err)
	}
	return &SSH{client: client, host: host}, nil
}

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

func (s *SSH) Host() string { return s.host }
func (s *SSH) Close() error { return s.client.Close() }
