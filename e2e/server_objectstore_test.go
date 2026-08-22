package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The object store the backup target points at.
//
// It runs from its own binary under its own systemd unit, deliberately not as
// a container. ob owns the container runtime on a server it manages — it
// creates and prunes networks by ownership, and `ob destroy` is meant to leave
// the machine clean — so a foreign container in that daemon is either flaky or
// a test that passes for the wrong reason. Outside it, the object store
// survives everything the suite does to ob's side of the machine.
const (
	minioRelease   = "RELEASE.2025-09-07T16-13-09Z"
	minioAMD64SHA  = "7c5bd8512c6e966455b1d198209358b2d191c77a83ab377c4073281065fb855f"
	minioARM64SHA  = "5c83cd2cf151717ba0243f73e1c7802ff36e272b67144bdd7f1f7d684fd6f03d"
	minioPort      = "9443"
	minioBucket    = "observer-backups"
	minioAccessKey = "obe2eaccesskey"
	minioSecretKey = "obe2esecretkey0123456789"
)

// objectStore starts MinIO over HTTPS with a certificate signed by an
// authority that exists only on this server, and installs that authority into
// the server's trust store.
//
// That arrangement is the whole point. Nothing publicly trusted signs this
// endpoint, so wal-g can only reach it if ob carries the host's trust store
// into the container — which is what issue #88 found it does not do.
func (s *server) objectStore(t *testing.T) string {
	t.Helper()
	ca := newAuthority(t, s.guest)

	s.write(t, "/etc/minio/public.crt", "0644", ca.certPEM)
	s.write(t, "/etc/minio/private.key", "0600", ca.keyPEM)
	// The trust store the container must inherit. update-ca-certificates
	// rebuilds /etc/ssl/certs/ca-certificates.crt, which is the bundle ob
	// stages beside the wal-g binary.
	s.write(t, "/usr/local/share/ca-certificates/onebox-e2e.crt", "0644", ca.caPEM)
	s.run(t, "update-ca-certificates >/dev/null 2>&1")

	arch, sha := "amd64", minioAMD64SHA
	if machine := strings.TrimSpace(s.run(t, "uname -m")); machine == "aarch64" || machine == "arm64" {
		arch, sha = "arm64", minioARM64SHA
	}
	// Pinned and checksummed rather than resolved at run time, for the same
	// reason the wal-g binary is: what runs is the artifact this line names, or
	// the setup stops.
	url := fmt.Sprintf("https://github.com/minio/minio/releases/download/%s/minio.linux-%s.%s",
		minioRelease, arch, minioRelease)
	s.run(t, strings.Join([]string{
		"set -e",
		"if ! sha256sum /usr/local/bin/minio 2>/dev/null | grep -q " + sha + "; then",
		"  curl -fsSL --retry 3 -o /tmp/minio " + url,
		"  echo '" + sha + "  /tmp/minio' | sha256sum --check --strict -",
		"  install -m 0755 /tmp/minio /usr/local/bin/minio",
		"  rm -f /tmp/minio",
		"fi",
		// A bucket the target declares as already existing. With the
		// filesystem backend a bucket is a directory.
		"mkdir -p /var/lib/minio/" + minioBucket,
	}, "\n"))

	s.write(t, "/etc/systemd/system/minio.service", "0644", []byte(`[Unit]
Description=Object store for the onebox server end-to-end suite
After=network-online.target

[Service]
Environment=MINIO_ROOT_USER=`+minioAccessKey+`
Environment=MINIO_ROOT_PASSWORD=`+minioSecretKey+`
ExecStart=/usr/local/bin/minio server /var/lib/minio \
  --address :`+minioPort+` \
  --certs-dir /etc/minio-certs
Restart=on-failure

[Install]
WantedBy=multi-user.target
`))
	// MinIO expects public.crt and private.key inside its certs directory.
	s.run(t, "mkdir -p /etc/minio-certs && cp /etc/minio/public.crt /etc/minio/private.key /etc/minio-certs/")
	s.run(t, "systemctl daemon-reload && systemctl enable --now minio")

	endpoint := "https://" + s.guest + ":" + minioPort
	deadline := time.Now().Add(60 * time.Second)
	for {
		// Verified against the freshly installed authority, so this also
		// proves the trust store was actually rebuilt.
		if err := s.try(t, "curl -fsS --max-time 5 "+endpoint+"/minio/health/live"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("object store never became ready at %s:\n%s", endpoint,
				s.run(t, "journalctl -u minio --no-pager --lines=40 || true"))
		}
		time.Sleep(time.Second)
	}
	// Deliberately left running when the test ends. The guest is disposable,
	// and stopping the repository as a cleanup destroys the evidence for
	// whatever failed — a wal-push run afterwards reports "connection refused"
	// and looks like the bug, when it is only the teardown.
	return endpoint
}
