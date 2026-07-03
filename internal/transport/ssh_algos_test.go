package transport

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// knownHostKeyAlgos must return the pinned key's type so the client requests a
// host-key algorithm that's actually in known_hosts (the bug: a host offering
// ecdsa/rsa/ed25519 with only ed25519 pinned reported a spurious mismatch).
func TestKnownHostKeyAlgosMatchesPinnedType(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	kh := filepath.Join(t.TempDir(), "known_hosts")
	line := knownhosts.Line([]string{"h.example.net:22"}, signer.PublicKey())
	if err := os.WriteFile(kh, []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cb, err := knownhosts.New(kh)
	if err != nil {
		t.Fatal(err)
	}
	got := knownHostKeyAlgos(cb, "h.example.net:22")
	if len(got) != 1 || got[0] != ssh.KeyAlgoED25519 {
		t.Fatalf("want [%s], got %v", ssh.KeyAlgoED25519, got)
	}
	// unknown host → no pins → nil (client falls back, callback still rejects)
	if got := knownHostKeyAlgos(cb, "other.example.net:22"); got != nil {
		t.Fatalf("unknown host must yield nil, got %v", got)
	}
}
