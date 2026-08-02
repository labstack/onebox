package compose

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	return StagePayloadContext(context.Background(), p, stagingDir)
}

// StagePayloadContext is StagePayload with cancellation checks while walking
// and copying project payloads.
func StagePayloadContext(ctx context.Context, p *types.Project, stagingDir string) (map[string]string, error) {
	rewrites := PayloadRewrites(p)
	for abs, rel := range rewrites {
		if err := ctx.Err(); err != nil {
			return nil, err
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
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			return // outside the project: a host path, not payload
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

// RewriteSources replaces absolute runner paths with release-relative ones in
// the rendered YAML. Absolute paths are unique strings — plain replacement is
// exact.
func RewriteSources(rendered []byte, rewrites map[string]string) []byte {
	for abs, rel := range rewrites {
		rendered = bytes.ReplaceAll(rendered, []byte(abs), []byte(rel))
	}
	return rendered
}

func copyTree(src, dst string) error {
	return copyTreeContext(context.Background(), src, dst)
}

func copyTreeContext(ctx context.Context, src, dst string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
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
		return copyFileContext(ctx, path, target, fi.Mode())
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	return copyFileContext(context.Background(), src, dst, mode)
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
