package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/labstack/onebox/internal/app"
)

// Executable protection for the postgres driver.
//
// Everything above this file describes protection: the project declares intent,
// the catalogue declares what each driver could support, the lifecycle state
// records whether it was ever established, and the artifact set records what a
// protected service should look like. None of it runs anything. This file and
// its _ops sibling are the part that does, and they are deliberately narrow —
// one driver, one recovery kind, physical base plus WAL.
//
// Whether a service *is* protected is not decided here. It is durable state on
// the target, observed at project load and bound into the rendered project
// before anything renders, so a policy that was declared but never enabled
// produces an ordinary server rather than one archiving to a repository nobody
// initialised.

// StageProtectionRuntime places the verified wal-g binary and its generated
// wrapper on the target, then makes them readable by the service.
//
// The binary is fetched here — on the machine running `ob` — and uploaded,
// rather than downloaded by the target. That keeps the agentless model intact
// and, more importantly, keeps verification on this side of the trust boundary:
// the checksum is pinned in the Onebox binary, so a host with no outbound
// internet still gets protection, and a compromised release page cannot
// substitute a binary that a target-side `curl | sha256sum` would happily
// accept against a checksum from the same source.
func (e *Engine) StageProtectionRuntime(ctx context.Context, service string, wrapper []byte) error {
	machine, err := e.targetMachine(ctx)
	if err != nil {
		return err
	}
	asset, expected, err := app.WalgAssetFor(machine)
	if err != nil {
		return err
	}
	n := e.names()
	destination := n.ProtectionBinaryFile(service)

	present, err := e.fileHasChecksum(ctx, destination, expected)
	if err != nil {
		return err
	}
	if !present {
		st := e.ui.Step("protection runtime wal-g "+app.WalgVersion+" ("+machine+")", false)
		staged, cleanup, err := fetchVerifiedBinary(ctx, app.WalgDownloadURL(asset), expected)
		if err != nil {
			st(err)
			return err
		}
		defer cleanup()
		if err := e.uploadProtectionBinary(ctx, n.ProtectionRuntimeDir(service), staged, destination); err != nil {
			st(err)
			return err
		}
		st(nil)
	}

	// The wrapper is passed in rather than rendered here, because enablement
	// has to stage the runtime *before* it records that the service is
	// protected — and until that record exists there is no bound state to
	// render from. It is rewritten every time: it is derived from the declared
	// credential entry names, which can change without the binary changing.
	wrapperPath := n.ProtectionWrapperFile(service)
	if err := e.writeServiceFile(ctx, wrapperPath, wrapper); err != nil {
		return fmt.Errorf("cannot place protection wrapper %s: %w", wrapperPath, err)
	}
	// Readable and executable by the unprivileged server user inside the
	// container, which is the whole point of it being there. Safe because it
	// holds no credential — only the names of entries it reads from the
	// environment.
	if err := e.chmodPath(ctx, wrapperPath, "0755"); err != nil {
		return err
	}
	return e.chmodPath(ctx, n.ProtectionRuntimeDir(service), "0755")
}

func (e *Engine) targetMachine(ctx context.Context) (string, error) {
	res, err := e.T.Run(ctx, "uname -m")
	if err != nil {
		return "", err
	}
	machine := strings.TrimSpace(res.Stdout)
	if res.ExitCode != 0 || machine == "" {
		return "", fmt.Errorf("cannot determine the target's machine architecture")
	}
	return machine, nil
}

// fileHasChecksum reports whether the target already holds exactly the expected
// bytes. Re-uploading 60MB on every enable would be the kind of cost that makes
// people avoid running the command.
func (e *Engine) fileHasChecksum(ctx context.Context, remotePath, expected string) (bool, error) {
	res, err := e.T.Run(ctx, "sha256sum "+q(remotePath)+" 2>/dev/null | cut -d' ' -f1")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(res.Stdout) == expected, nil
}

// fetchVerifiedBinary downloads an asset and refuses it unless it hashes to the
// pinned value. The file is never made executable and never leaves the
// temporary directory until it has matched.
func fetchVerifiedBinary(ctx context.Context, url, expected string) (string, func(), error) {
	dir, err := os.MkdirTemp("", "ob-protection-runtime-")
	if err != nil {
		return "", nil, fmt.Errorf("create staging directory: %w", err)
	}
	cleanup := func() { os.RemoveAll(dir) }

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		cleanup()
		return "", nil, err
	}
	client := &http.Client{Timeout: 10 * time.Minute}
	response, err := client.Do(request)
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		cleanup()
		return "", nil, fmt.Errorf("fetch %s: %s", url, response.Status)
	}

	staged := filepath.Join(dir, "wal-g")
	file, err := os.OpenFile(staged, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		cleanup()
		return "", nil, err
	}
	digest := sha256.New()
	if _, err := io.Copy(io.MultiWriter(file, digest), response.Body); err != nil {
		file.Close()
		cleanup()
		return "", nil, fmt.Errorf("download %s: %w", url, err)
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	if observed := hex.EncodeToString(digest.Sum(nil)); observed != expected {
		cleanup()
		return "", nil, fmt.Errorf(
			"the wal-g download does not match its pinned checksum (expected %s, got %s); refusing to place it on the host",
			expected, observed)
	}
	return staged, cleanup, nil
}

// uploadProtectionBinary moves the verified binary into place. Upload writes a
// directory, so the staged file is placed alone in one and moved across.
func (e *Engine) uploadProtectionBinary(ctx context.Context, runtimeDir, staged, destination string) error {
	res, err := e.T.Run(ctx, "mkdir -p "+q(runtimeDir))
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("cannot create the protection runtime directory: %s", strings.TrimSpace(res.Stderr))
	}
	remoteStaging := runtimeDir + "/.staging"
	if err := e.T.Upload(ctx, filepath.Dir(staged), remoteStaging); err != nil {
		return fmt.Errorf("upload the wal-g binary: %w", err)
	}
	install := "mv -f " + q(remoteStaging+"/"+filepath.Base(staged)) + " " + q(destination) +
		" && chmod 0755 " + q(destination) +
		" && rm -rf " + q(remoteStaging)
	res, err = e.T.Run(ctx, install)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("cannot install the wal-g binary: %s", strings.TrimSpace(res.Stderr))
	}
	return nil
}

func (e *Engine) chmodPath(ctx context.Context, target, mode string) error {
	res, err := e.T.Run(ctx, "chmod "+mode+" "+q(target))
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("cannot set mode %s on %s", mode, target)
	}
	return nil
}

// WriteProtectionLifecycleState places the already-sealed lifecycle record that
// makes a service protected. The record's schema, transitions, and digest all
// belong to the layer above; the engine only puts the bytes on the target,
// under the same fence as every other generated file.
func (e *Engine) WriteProtectionLifecycleState(ctx context.Context, service string, body []byte) error {
	n := e.names()
	res, err := e.T.Run(ctx, "mkdir -p "+q(path.Join(n.AppDir(), "protection", "state")))
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("cannot create the protection state directory: %s", strings.TrimSpace(res.Stderr))
	}
	return e.writeServiceFile(ctx, n.ProtectionLifecycleStateFile(service), body)
}

// ReadProtectionLifecycleState returns the raw lifecycle record for a service,
// or nil when none exists. Decoding belongs to the layer that owns the schema;
// the engine only fetches the bytes.
func (e *Engine) ReadProtectionLifecycleState(ctx context.Context, service string) ([]byte, error) {
	res, err := e.T.Run(ctx, "cat "+q(e.names().ProtectionLifecycleStateFile(service))+" 2>/dev/null || true")
	if err != nil {
		return nil, err
	}
	if len(res.Stdout) == 0 {
		return nil, nil
	}
	return []byte(res.Stdout), nil
}

// RebindServiceRuntimeStates re-derives the rendered project from lifecycle
// state the caller has just written. Enablement is the one flow that needs it:
// the project was loaded before the service was protected, so without this the
// same run would render the unprotected server it started from.
func (e *Engine) RebindServiceRuntimeStates(states map[string]app.ServiceRuntimeState) error {
	bound, err := e.Spec.WithServiceRuntimeStates(states)
	if err != nil {
		return err
	}
	e.Spec = bound
	return nil
}

// ResolveProtectedImage pins the service image by the digest the host actually
// has, after pulling it.
//
// It is the stock PostgreSQL image — wal-g is mounted in beside it rather than
// baked into a derived one — but it is still pinned, because the reason
// protected image selection is durable state has nothing to do with which image
// it is: the bytes running over a live data directory must not change because a
// tag moved.
func (e *Engine) ResolveProtectedImage(ctx context.Context, service string) (string, error) {
	reference, err := e.Spec.ServiceImageForRuntime(service)
	if err != nil {
		return "", err
	}
	st := e.ui.Step("protected image "+reference.Image, false)
	res, err := e.T.Run(ctx, "docker pull "+q(reference.Image))
	if err != nil {
		st(err)
		return "", err
	}
	if res.ExitCode != 0 {
		err := fmt.Errorf("cannot pull %s: %s", reference.Image, lastLines(res.Stderr, 3))
		st(err)
		return "", err
	}
	res, err = e.T.Run(ctx, "docker image inspect --format '{{index .RepoDigests 0}}' "+q(reference.Image))
	if err != nil {
		st(err)
		return "", err
	}
	pinned := strings.TrimSpace(res.Stdout)
	if res.ExitCode != 0 || !strings.Contains(pinned, "@sha256:") {
		err := fmt.Errorf(
			"%s has no registry digest on this host; a protected service runs an image pinned by digest, so it must come from a registry rather than a local build",
			reference.Image)
		st(err)
		return "", err
	}
	st(nil)
	return pinned, nil
}

func sortedPaths(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
