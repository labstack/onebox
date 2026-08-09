// Package release manages the versioned remote layout:
// /var/lib/ob/<app>/releases/<id>/ + a `current` symlink. Nothing live is
// ever overwritten; rollback re-activates a previous directory.
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

	"github.com/labstack/onebox/internal/app"
	"github.com/labstack/onebox/internal/shellquote"
	"github.com/labstack/onebox/internal/transport"
)

type Paths struct{ Base, Releases, Current string }

// PathsFor resolves the remote layout from the project's own names.
//
// Whatever `NamesFor(environment)` resolved is the single authority. Any second
// path source — an app name plus an environment variable, say — lets locks,
// fences, journals, releases and secret pushes operate in a different tree from
// the one preflight validated and generation wrote, for any project or
// environment declaring `base_path`.
func PathsFor(n app.Names) Paths {
	base := n.AppDir()
	return Paths{Base: base, Releases: base + "/releases", Current: base + "/current"}
}

var safeSHA = regexp.MustCompile(`^[0-9a-f]{4,40}$`)

// releaseID matches what NewID produces: a UTC timestamp, then a git SHA or the
// literal "nogit", optionally followed by a caller's own suffix. No dot — that
// is what keeps a `<id>.partial` staging directory from reading as a release.
var releaseID = regexp.MustCompile(`^\d{8}-\d{6}-[0-9a-zA-Z_-]+$`)

// IsID reports whether a directory name is a release id. Anything under the
// releases directory that is not one is something else's, and must not be
// rolled back to or counted against retention.
func IsID(name string) bool { return releaseID.MatchString(name) }

// NewID builds a lexically time-ordered release id. An unsafe SHA component
// is replaced, never interpolated (command-injection rule).
func NewID(now time.Time, gitSHA string) string {
	if !safeSHA.MatchString(gitSHA) {
		gitSHA = "nogit"
	}
	return now.UTC().Format("20060102-150405") + "-" + gitSHA
}

// Stage writes the release payload into a local staging dir: the rendered
// compose plus the ob.yml snapshot so rollback replays the prior
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

func Push(ctx context.Context, t transport.Transport, stagingDir string, n app.Names, id string) (string, error) {
	remote := PathsFor(n).Releases + "/" + id
	if err := t.Upload(ctx, stagingDir, remote); err != nil {
		return "", err
	}
	return remote, nil
}

func Current(ctx context.Context, t transport.Transport, n app.Names) (string, error) {
	p := PathsFor(n)
	res, err := t.Run(ctx, "if [ -L "+q(p.Current)+" ]; then readlink "+q(p.Current)+"; elif [ -e "+q(p.Current)+" ]; then exit 2; fi")
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("read current release failed (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	link := strings.TrimSpace(res.Stdout)
	if link == "" {
		return "", nil // first deploy
	}
	return filepath.Base(link), nil
}

// list returns the release ids under the releases directory, and separately the
// entries it refused to treat as ids.
//
// Skipping is reported rather than swallowed because the two failure directions
// are not equally visible. Admitting a non-release is loud — rollback lands on
// junk. Rejecting a real release is silent: the directory is on the host, the
// operator can see it with `ls`, and ob simply behaves as though it were not
// there. Callers must be able to say which happened.
func list(ctx context.Context, t transport.Transport, n app.Names) (ids, skipped []string, err error) {
	res, err := t.Run(ctx, "ls -1 "+q(PathsFor(n).Releases)+" 2>/dev/null || true")
	if err != nil {
		return nil, nil, err
	}
	for _, l := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		// Every entry here is treated as a release id — it is handed to
		// `ob rollback` by Previous and counted against retention by
		// PruneCandidates — so anything that is not one has to be excluded
		// rather than assumed absent. Upload staging is a dot-directory in here,
		// which `ls -1` already hides; this is the guard that does not depend on
		// that.
		if !IsID(l) {
			skipped = append(skipped, l)
			continue
		}
		ids = append(ids, l)
	}
	sort.Strings(ids) // ids are lexically time-ordered by construction
	return ids, skipped, nil
}

func Previous(ctx context.Context, t transport.Transport, n app.Names) (string, error) {
	cur, err := Current(ctx, t, n)
	if err != nil {
		return "", err
	}
	ids, skipped, err := list(ctx, t, n)
	if err != nil {
		return "", err
	}
	for i, id := range ids {
		if id == cur && i > 0 {
			return ids[i-1], nil
		}
	}
	// Report the entries that were filtered out. Without them this message
	// states a directory listing that contradicts what `ls` shows on the host —
	// confident, specific and wrong — and gives the operator no way to tell a
	// genuinely absent release from one ob declined to recognise.
	if len(skipped) > 0 {
		return "", fmt.Errorf("no previous release (current=%q, releases=%v); %d entr(ies) under %s were not "+
			"recognised as release ids and were ignored: %v", cur, ids, len(skipped), PathsFor(n).Releases, skipped)
	}
	return "", fmt.Errorf("no previous release (current=%q, releases=%v)", cur, ids)
}

func Activate(ctx context.Context, t transport.Transport, n app.Names, id string) error {
	p := PathsFor(n)
	res, err := t.Run(ctx, "ln -sfn "+q("releases/"+id)+" "+q(p.Current))
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("activate: %s", res.Stderr)
	}
	return nil
}

// PruneCandidates returns releases beyond retain, never the current target,
// along with the entries it did not recognise as release ids. Removal is the
// caller's job (the engine fences it). Images are deliberately NOT pruned,
// because rollback never pulls.
//
// Unrecognised entries are returned rather than dropped because retention is
// enforced over a directory they still occupy: they are neither counted against
// retain nor removed, so a directory ob claims to be keeping at N entries can
// grow without bound and report nothing.
func PruneCandidates(ctx context.Context, t transport.Transport, n app.Names, retain int) (victims, unrecognized []string, err error) {
	ids, skipped, err := list(ctx, t, n)
	if err != nil || len(ids) <= retain {
		return nil, skipped, err
	}
	cur, err := Current(ctx, t, n)
	if err != nil {
		return nil, skipped, err
	}
	for _, id := range ids[:len(ids)-retain] {
		if id != cur {
			victims = append(victims, id)
		}
	}
	return victims, skipped, nil
}

// Prune removes releases beyond retain, never the current target.
func Prune(ctx context.Context, t transport.Transport, n app.Names, retain int) ([]string, error) {
	victims, _, err := PruneCandidates(ctx, t, n, retain)
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, id := range victims {
		res, err := t.Run(ctx, "rm -rf "+q(PathsFor(n).Releases+"/"+id))
		if err != nil {
			return removed, err
		}
		if res.ExitCode != 0 {
			return removed, fmt.Errorf("prune release %s failed (exit %d): %s", id, res.ExitCode, strings.TrimSpace(res.Stderr))
		}
		removed = append(removed, id)
	}
	return removed, nil
}

func q(s string) string { return shellquote.Quote(s) }
