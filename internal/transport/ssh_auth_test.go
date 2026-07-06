package transport

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// writeKey writes an ed25519 private key to home/.ssh/<name>, encrypted with
// passphrase when non-empty, and returns the .ssh dir.
func writeKey(t *testing.T, home, name, passphrase string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var blk *pem.Block
	if passphrase == "" {
		blk, err = ssh.MarshalPrivateKey(priv, "")
	} else {
		blk, err = ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte(passphrase))
	}
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), pem.EncodeToMemory(blk), 0o600); err != nil {
		t.Fatal(err)
	}
}

// serveAgent starts an ssh-agent on a temp unix socket and returns its path.
// keys are added to the agent before serving connections.
func serveAgent(t *testing.T, keys int) string {
	t.Helper()
	ring := agent.NewKeyring()
	for i := 0; i < keys; i++ {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		if err := ring.Add(agent.AddedKey{PrivateKey: priv}); err != nil {
			t.Fatal(err)
		}
	}
	// A short base dir avoids the 104-char sun_path limit — t.TempDir() under
	// macOS's /var/folders already blows past it on its own.
	base, err := os.MkdirTemp("/tmp", "ob")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	sock := filepath.Join(base, "a.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go agent.ServeAgent(ring, conn)
		}
	}()
	return sock
}

// An encrypted on-disk key must not be silently dropped: sshAuths yields no
// method and an actionable "load it into your agent" diagnostic — the bug where
// ob offered nothing and let the server return a bare "handshake failed".
func TestSSHAuthsEncryptedKeyIsDiagnosed(t *testing.T) {
	home := t.TempDir()
	writeKey(t, home, "id_rsa", "hunter2")

	auths, diag := sshAuths(home, "")
	if len(auths) != 0 {
		t.Fatalf("encrypted key must not yield an auth method, got %d", len(auths))
	}
	joined := strings.Join(diag, "; ")
	if !strings.Contains(joined, "passphrase-encrypted") || !strings.Contains(joined, "ssh-add") {
		t.Fatalf("diagnostic must name the cause and the fix, got %q", joined)
	}
}

// An unencrypted on-disk key is usable.
func TestSSHAuthsUnencryptedKeyIsUsed(t *testing.T) {
	home := t.TempDir()
	writeKey(t, home, "id_ed25519", "")

	auths, diag := sshAuths(home, "")
	if len(auths) != 1 {
		t.Fatalf("want 1 auth method, got %d (diag: %v)", len(auths), diag)
	}
}

// A reachable-but-empty agent must contribute no auth method (so it can't mask
// the no-usable-key error) and must explain itself.
func TestSSHAuthsEmptyAgentContributesNothing(t *testing.T) {
	home := t.TempDir() // no ~/.ssh keys
	sock := serveAgent(t, 0)

	auths, diag := sshAuths(home, sock)
	if len(auths) != 0 {
		t.Fatalf("empty agent must yield no auth method, got %d", len(auths))
	}
	if !strings.Contains(strings.Join(diag, "; "), "no identities") {
		t.Fatalf("empty agent must be diagnosed, got %v", diag)
	}
}

// An agent holding an identity contributes exactly one auth method.
func TestSSHAuthsAgentWithIdentityIsUsed(t *testing.T) {
	home := t.TempDir()
	sock := serveAgent(t, 1)

	auths, diag := sshAuths(home, sock)
	if len(auths) != 1 {
		t.Fatalf("agent with one identity → 1 auth method, got %d (diag: %v)", len(auths), diag)
	}
}

// An unreachable SSH_AUTH_SOCK is reported, not silently ignored.
func TestSSHAuthsUnreachableAgentIsDiagnosed(t *testing.T) {
	home := t.TempDir()
	auths, diag := sshAuths(home, filepath.Join(t.TempDir(), "nope.sock"))
	if len(auths) != 0 {
		t.Fatalf("want no auth, got %d", len(auths))
	}
	if !strings.Contains(strings.Join(diag, "; "), "unreachable") {
		t.Fatalf("unreachable agent must be diagnosed, got %v", diag)
	}
}
