// The server end-to-end harness.
//
// These tests run here and reach a real machine over SSH. That is the shape
// the product actually has — ob runs on a workstation and the box is somewhere
// else — and internal/transport/transport.go records that the Docker suite
// deliberately substitutes local docker for that box. This is the one harness
// that does not make the substitution, so it is the only place the SSH
// transport, systemd timers, a bare machine, and the host trust store are
// exercised at all.
//
// The machine is supplied by `just lima-up`; anything reachable as root over
// SSH will do, including a rented one.
package e2e

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"text/template"
	"time"
)

type server struct {
	target string // user@host[:port], the same shape ob.yml's `server:` takes
	user   string
	host   string
	port   string
	key    string // private key that reaches it
	guest  string // the guest's own address, as a container on it sees the host

	// First contact happens once per server, like it does for an operator.
	bootstrapped map[string]bool
}

func requireServer(t *testing.T) *server {
	t.Helper()
	if os.Getenv("OB_SERVER_E2E") != "1" {
		t.Skip("set OB_SERVER_E2E=1 (see `just server-e2e`)")
	}
	// Opting in is a promise the machine is there. Skipping past an
	// unreachable one turns a gate into a green tick for work nobody did.
	target := os.Getenv("OB_E2E_SERVER")
	key := os.Getenv("OB_E2E_SERVER_KEY")
	if target == "" || key == "" {
		t.Fatal("OB_SERVER_E2E=1 without OB_E2E_SERVER and OB_E2E_SERVER_KEY")
	}
	user, rest, ok := strings.Cut(target, "@")
	if !ok {
		t.Fatalf("OB_E2E_SERVER %q is not user@host[:port]", target)
	}
	host, port, ok := strings.Cut(rest, ":")
	if !ok {
		port = "22"
	}
	// ob never elevates — there is no sudo anywhere in internal/engine,
	// internal/transport or internal/onebox, and it writes unit files directly
	// under /etc/systemd/system. A server it cannot reach as root fails later,
	// in a place that looks like a deploy bug.
	if user != "root" {
		t.Fatalf("OB_E2E_SERVER is %q; ob writes to /etc/systemd/system and does not elevate, so it must be root", target)
	}
	s := &server{target: target, user: user, host: host, port: port, key: key,
		bootstrapped: map[string]bool{}}
	if err := s.try("true"); err != nil {
		t.Fatalf("OB_SERVER_E2E=1 but %s is not reachable: %v", target, err)
	}
	s.guest = strings.Fields(s.run(t, "hostname -I"))[0]
	return s
}

// run executes a command on the server and fails the test if it does not
// succeed. This is fixture plumbing, not the transport under test, so it uses
// the system ssh client directly.
func (s *server) run(t *testing.T, command string) string {
	t.Helper()
	out, err := s.output(command)
	if err != nil {
		t.Fatalf("server command failed: %s\n%v\n%s", command, err, out)
	}
	return out
}

func (s *server) try(command string) error {
	_, err := s.output(command)
	return err
}

func (s *server) output(command string) (string, error) {
	cmd := exec.Command("ssh",
		"-i", s.key, "-p", s.port,
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=10",
		s.user+"@"+s.host, command)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// write places a file on the server without an intermediate shell quoting
// problem: the body travels on stdin.
func (s *server) write(t *testing.T, path string, mode string, body []byte) {
	t.Helper()
	cmd := exec.Command("ssh",
		"-i", s.key, "-p", s.port,
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		s.user+"@"+s.host,
		"install -D -m "+mode+" /dev/stdin "+path)
	cmd.Stdin = strings.NewReader(string(body))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("writing %s: %v\n%s", path, err, out)
	}
}

var (
	obOnce sync.Once
	obPath string
	obErr  error
)

// obBinary builds the real CLI once per run.
//
// The acceptance path is the binary, not the engine: `ob backup enable` is
// orchestrated in internal/onebox above the engine, and driving the engine
// directly would skip the part where a failed enablement has to put the
// service back.
func obBinary(t *testing.T) string {
	t.Helper()
	obOnce.Do(func() {
		dir, err := os.MkdirTemp("", "ob-e2e-bin")
		if err != nil {
			obErr = err
			return
		}
		obPath = filepath.Join(dir, "ob")
		build := exec.Command("go", "build", "-o", obPath, "./cmd/ob")
		build.Dir = repoRoot(t)
		if out, err := build.CombinedOutput(); err != nil {
			obErr = fmt.Errorf("building ob: %v\n%s", err, out)
		}
	})
	if obErr != nil {
		t.Fatal(obErr)
	}
	return obPath
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// project renders the fixture against this server and returns its directory.
//
// The version is what the workload serves, so a second render into the same
// directory is a second release of the same project — which is what rollback
// and resume need in order to have something to move between.
func (s *server) project(t *testing.T, endpoint, version string) string {
	t.Helper()
	dir := t.TempDir()
	s.render(t, dir, endpoint, version)
	return dir
}

// render writes ob.yml into an existing project directory, replacing what is
// there. Used to produce the next version without disturbing the credentials
// or the artifacts already beside it.
func (s *server) render(t *testing.T, dir, endpoint, version string) {
	t.Helper()
	tmpl, err := template.ParseFiles(filepath.Join("testdata", "postgres", "ob.yml.tmpl"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.Create(filepath.Join(dir, "ob.yml"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if err := tmpl.Execute(out, struct{ Server, Endpoint, Version string }{s.target, endpoint, version}); err != nil {
		t.Fatal(err)
	}
	out.Close()
	// The credential file is resolved relative to the project file, so it
	// travels with it.
	secrets := filepath.Join(dir, "secrets")
	if err := os.MkdirAll(secrets, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"backup.env", "age.key"} {
		body, err := os.ReadFile(filepath.Join("testdata", "postgres", "secrets", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(secrets, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// obHome is the HOME ob runs with: the identity that reaches the server, and a
// known_hosts holding its key.
//
// ob refuses an unknown host by design, so pinning it here is what lets the
// happy path run at all — and it means the absent and mismatched cases are
// their own tests rather than an accident of the environment.
func (s *server) obHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	ssh := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(ssh, 0o700); err != nil {
		t.Fatal(err)
	}
	key, err := os.ReadFile(s.key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ssh, "id_ed25519"), key, 0o600); err != nil {
		t.Fatal(err)
	}
	scan := exec.Command("ssh-keyscan", "-p", s.port, s.host)
	scanned, err := scan.Output()
	if err != nil {
		t.Fatalf("ssh-keyscan %s:%s: %v", s.host, s.port, err)
	}
	if err := os.WriteFile(filepath.Join(ssh, "known_hosts"), scanned, 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

// ob runs the CLI against this server and returns its combined output.
func (s *server) ob(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	return s.obWithHome(t, dir, s.obHome(t), args...)
}

// obWithHome is the same, with the HOME chosen by the caller — which is how
// the tests about SSH itself deny ob the identity or the host key.
func (s *server) obWithHome(t *testing.T, dir, home string, args ...string) (string, error) {
	t.Helper()
	return s.obInput(t, dir, home, "", args...)
}

// obInput is the same again, with something on standard input — which is the
// only way to answer the confirmations ob asks for deliberately and offers no
// flag to skip.
func (s *server) obInput(t *testing.T, dir, home, stdin string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(obBinary(t), append([]string{"-c", filepath.Join(dir, "ob.yml")}, args...)...)
	cmd.Dir = dir
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		// Nothing may fall back to the developer's agent: these tests are
		// about which credentials ob finds, so an agent in the environment
		// would make them pass for a reason the CI runner does not share.
		"SSH_AUTH_SOCK=",
		"SOPS_AGE_KEY_FILE="+filepath.Join(dir, "secrets", "age.key"),
	)
	out, err := cmd.CombinedOutput()
	t.Logf("ob %s\n%s", strings.Join(args, " "), out)
	return string(out), err
}

func (s *server) mustOb(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := s.ob(t, dir, args...)
	if err != nil {
		t.Fatalf("ob %s failed: %v", strings.Join(args, " "), err)
	}
	return out
}

// authority is a certificate authority that exists only on this guest.
type authority struct {
	certPEM []byte
	keyPEM  []byte
	caPEM   []byte
}

// newAuthority issues a CA and a server certificate for an IP address.
//
// A private authority is the point of the exercise: it can only be trusted by
// installing it on the host, which is exactly the arrangement that fails when
// nothing carries the host's trust store into the container wal-g runs in.
func newAuthority(t *testing.T, ip string) authority {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "onebox e2e authority"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: ip},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP(ip)},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leafKeyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		t.Fatal(err)
	}
	return authority{
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		keyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: leafKeyDER}),
		caPEM:   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
	}
}

// deploy runs the release the way the product documents it for a plan that has
// never been approved: plan, approve, apply.
//
// ob refuses a bare `deploy` against a host that has not seen this exact plan
// — "one_time approval is required for this exact deployment plan" — so a test
// that only called deploy would be testing the gate, not the release. Going
// through the artifacts also puts `plan` and `approve` under acceptance
// coverage, which they have never had.
func (s *server) deploy(t *testing.T, dir string) {
	t.Helper()
	// First contact. ob refuses to deploy to a machine that has no owner
	// record for this application — "host has no Onebox application owner" —
	// so bootstrap is part of the path, not a precondition a test may assume.
	if app := filepath.Base(dir); !s.bootstrapped[app] {
		s.mustOb(t, dir, "bootstrap")
		s.bootstrapped[app] = true
	}

	plan := filepath.Join(dir, "ob-plan.json")
	approval := filepath.Join(dir, "ob-approval.json")
	s.mustOb(t, dir, "plan", "-o", plan)

	// `approve` records a *human* confirmation and deliberately has no flag to
	// skip it, so the answer arrives on standard input. A strong approval wants
	// the release ID typed back; anything weaker is a yes-or-no.
	body, err := os.ReadFile(plan)
	if err != nil {
		t.Fatal(err)
	}
	answer := "y\n"
	if strings.Contains(string(body), `"approval":"strong"`) ||
		strings.Contains(string(body), `"approval": "strong"`) {
		match := releaseIDRe.FindSubmatch(body)
		if match == nil {
			t.Fatalf("plan wants a strong approval but names no release id:\n%s", body)
		}
		answer = string(match[1]) + "\n"
	}
	out, err := s.obInput(t, dir, s.obHome(t), answer, "approve", "--plan", plan, "-o", approval)
	if err != nil {
		t.Fatalf("ob approve failed: %v\n%s", err, out)
	}
	s.mustOb(t, dir, "deploy", "--plan", plan, "--approval", approval, "-y")
}

// releaseIDRe pulls the release the plan is bound to out of the artifact. The
// plan is JSON with a stable field name; a full decode would need the internal
// types, which the acceptance path deliberately does not import.
var releaseIDRe = regexp.MustCompile(`"release_id"\s*:\s*"([^"]+)"`)
