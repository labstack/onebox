// Package release manages the versioned remote layout:
// /var/lib/ob/<app>/releases/<id>/ + a `current` symlink. Nothing live is
// ever overwritten (design §04); rollback re-activates a previous dir.
package release

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/labstack/onebox/internal/transport"
)

type Paths struct{ Base, Releases, Current string }

// Root is the directory that holds every app's per-app tree (plus _host). The
// OB_BASE_DIR override is the e2e/macOS test hook; the default is the real
// layout. Never hardcode "/var/lib/ob" — host-level commands (ob ls) enumerate
// this dir and would look in the wrong place under the test hook.
func Root() string {
	if root := os.Getenv("OB_BASE_DIR"); root != "" {
		return root
	}
	return "/var/lib/ob"
}

func PathsFor(app string) Paths {
	base := Root() + "/" + app
	return Paths{Base: base, Releases: base + "/releases", Current: base + "/current"}
}

var safeSHA = regexp.MustCompile(`^[0-9a-f]{4,40}$`)

// NewID builds a lexically time-ordered release id. An unsafe SHA component
// is replaced, never interpolated (command-injection rule).
func NewID(now time.Time, gitSHA string) string {
	if !safeSHA.MatchString(gitSHA) {
		gitSHA = "nogit"
	}
	return now.UTC().Format("20060102-150405") + "-" + gitSHA
}

// Stage writes the release payload into a local staging dir: the rendered
// compose plus the ob.yml snapshot (design §04 — rollback replays the OLD
// choreography from the release's own snapshot).
func Stage(dir string, composeYAML, snapshotYAML []byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// compose.yaml can still carry inline `environment: KEY: ${VAR}` secrets
	// that interpolation resolved (env_file entries stay external) — mode 600,
	// same trust boundary as the host's .env.
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), composeYAML, 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "ob.snapshot.yml"), snapshotYAML, 0o644)
}

func Push(ctx context.Context, t transport.Transport, stagingDir, app, id string) (string, error) {
	remote := PathsFor(app).Releases + "/" + id
	if err := t.Upload(ctx, stagingDir, remote); err != nil {
		return "", err
	}
	return remote, nil
}

func Current(ctx context.Context, t transport.Transport, app string) (string, error) {
	p := PathsFor(app)
	res, err := t.Run(ctx, "readlink "+q(p.Current)+" || true")
	if err != nil {
		return "", err
	}
	link := strings.TrimSpace(res.Stdout)
	if link == "" {
		return "", nil // first deploy
	}
	return filepath.Base(link), nil
}

func list(ctx context.Context, t transport.Transport, app string) ([]string, error) {
	res, err := t.Run(ctx, "ls -1 "+q(PathsFor(app).Releases)+" 2>/dev/null || true")
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, l := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			ids = append(ids, l)
		}
	}
	sort.Strings(ids) // ids are lexically time-ordered by construction
	return ids, nil
}

func Previous(ctx context.Context, t transport.Transport, app string) (string, error) {
	cur, err := Current(ctx, t, app)
	if err != nil {
		return "", err
	}
	ids, err := list(ctx, t, app)
	if err != nil {
		return "", err
	}
	for i, id := range ids {
		if id == cur && i > 0 {
			return ids[i-1], nil
		}
	}
	return "", fmt.Errorf("no previous release (current=%q, releases=%v)", cur, ids)
}

func Activate(ctx context.Context, t transport.Transport, app, id string) error {
	p := PathsFor(app)
	res, err := t.Run(ctx, "ln -sfn "+q("releases/"+id)+" "+q(p.Current))
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("activate: %s", res.Stderr)
	}
	return nil
}

// PruneCandidates returns releases beyond retain, never the current target.
// Removal is the caller's job (the engine fences it). Images are deliberately
// NOT pruned (design §06: rollback never pulls).
func PruneCandidates(ctx context.Context, t transport.Transport, app string, retain int) ([]string, error) {
	ids, err := list(ctx, t, app)
	if err != nil || len(ids) <= retain {
		return nil, err
	}
	cur, err := Current(ctx, t, app)
	if err != nil {
		return nil, err
	}
	var victims []string
	for _, id := range ids[:len(ids)-retain] {
		if id != cur {
			victims = append(victims, id)
		}
	}
	return victims, nil
}

// Prune removes releases beyond retain, never the current target.
func Prune(ctx context.Context, t transport.Transport, app string, retain int) ([]string, error) {
	victims, err := PruneCandidates(ctx, t, app, retain)
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, id := range victims {
		res, err := t.Run(ctx, "rm -rf "+q(PathsFor(app).Releases+"/"+id))
		if err != nil {
			return removed, err
		}
		if res.ExitCode == 0 {
			removed = append(removed, id)
		}
	}
	return removed, nil
}

func q(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
