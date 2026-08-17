package compose

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
)

// StagePayload copies every file the compose file references from INSIDE the
// project dir (bind-mount sources, env_files — compose-go absolutized them at
// load) into the staging dir under its project-relative path, and returns the
// abs→"./rel" rewrites to apply to the rendered YAML. Sources outside the
// project dir (/var/run/docker.sock, /data/...) are host paths — untouched.
// This makes the transferred release directory self-contained.
func StagePayload(p *types.Project, stagingDir string) (map[string]string, error) {
	return StagePayloadContext(context.Background(), p, stagingDir, nil)
}

// StagePayloadContext is StagePayload with cancellation checks while walking
// and copying project payloads.
//
// projected names files the caller supplies directly in the staging dir, keyed
// by project-relative slash path. They are referenced by the document like any
// other payload, but do not need copying from the project. They stay in the
// rewrite map, because the rendered bytes must not depend on how a file arrived.
func StagePayloadContext(ctx context.Context, p *types.Project, stagingDir string, projected map[string]bool) (map[string]string, error) {
	rewrites := PayloadRewrites(p)
	for abs, rel := range rewrites {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if projected[strings.TrimPrefix(rel, "./")] {
			continue
		}
		dst := filepath.Join(stagingDir, filepath.FromSlash(strings.TrimPrefix(rel, "./")))
		if err := copyTreeContext(ctx, abs, dst); err != nil {
			return nil, fmt.Errorf("stage %s: %w", rel, err)
		}
	}
	return rewrites, nil
}

// PayloadRewrites is the pure half of StagePayload: the abs→"./rel" map,
// with no copying. Plan and apply both use it, so the rendered bytes a plan
// stores are byte-identical to what apply would render.
func PayloadRewrites(p *types.Project) map[string]string {
	projectDir := p.WorkingDir
	rewrites := map[string]string{}
	note := func(abs string) {
		if abs == "" || !filepath.IsAbs(abs) {
			return
		}
		rel, err := filepath.Rel(projectDir, abs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return // outside the project: a host path, not payload
		}
		if rel == "." {
			rewrites[abs] = "."
			return
		}
		rewrites[abs] = "./" + filepath.ToSlash(rel)
	}
	for _, svc := range p.Services {
		for _, v := range svc.Volumes {
			if v.Type == types.VolumeTypeBind {
				note(v.Source)
			}
		}
		for _, ef := range svc.EnvFiles {
			note(ef.Path)
		}
	}
	return rewrites
}

// RewriteSources replaces complete absolute runner path scalars with their
// release-relative forms. Longer paths go first, and scalar boundaries prevent
// a project root such as /srv/app from corrupting an unrelated /srv/app-data
// host mount.
func RewriteSources(rendered []byte, rewrites map[string]string) []byte {
	paths := make([]string, 0, len(rewrites))
	for abs := range rewrites {
		paths = append(paths, abs)
	}
	sort.Slice(paths, func(i, j int) bool {
		if len(paths[i]) == len(paths[j]) {
			return paths[i] < paths[j]
		}
		return len(paths[i]) > len(paths[j])
	})
	for _, abs := range paths {
		rendered = replacePathScalar(rendered, []byte(abs), []byte(rewrites[abs]))
	}
	return rendered
}

func replacePathScalar(in, old, replacement []byte) []byte {
	var out []byte
	for len(in) > 0 {
		i := bytes.Index(in, old)
		if i < 0 {
			return append(out, in...)
		}
		end := i + len(old)
		startsScalar := i == 0 || isScalarStart(in[i-1])
		if startsScalar && (end == len(in) || isScalarEnd(in[end])) {
			out = append(out, in[:i]...)
			out = append(out, replacement...)
			in = in[end:]
			continue
		}
		out = append(out, in[:end]...)
		in = in[end:]
	}
	return out
}

func isScalarStart(b byte) bool {
	switch b {
	case ' ', '\t', '\r', '\n', ',', '[', '{', '\'', '"':
		return true
	default:
		return false
	}
}

func isScalarEnd(b byte) bool {
	switch b {
	case ' ', '\t', '\r', '\n', ':', ',', ']', '}', '\'', '"':
		return true
	default:
		return false
	}
}

func copyTreeContext(ctx context.Context, src, dst string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing symlink %s", src)
	}
	if !info.IsDir() {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("refusing non-regular file %s", src)
		}
		return copyFileContext(ctx, src, dst, info.Mode())
	}
	return filepath.Walk(src, func(path string, fi os.FileInfo, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if fi.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink %s", path)
		}
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("refusing non-regular file %s", path)
		}
		return copyFileContext(ctx, path, target, fi.Mode())
	})
}

func copyFileContext(ctx context.Context, src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode.Perm())
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, contextReader{ctx: ctx, reader: in})
	return err
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
