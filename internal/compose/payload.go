package compose

import (
	"bytes"
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
// This is what makes the release dir self-contained (design §04 transfer).
func StagePayload(p *types.Project, stagingDir string) (map[string]string, error) {
	projectDir := p.WorkingDir
	rewrites := map[string]string{}
	stage := func(abs string) error {
		if abs == "" || !filepath.IsAbs(abs) {
			return nil
		}
		rel, err := filepath.Rel(projectDir, abs)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			return nil // outside the project: a host path, not payload
		}
		if _, seen := rewrites[abs]; seen {
			return nil
		}
		if err := copyTree(abs, filepath.Join(stagingDir, rel)); err != nil {
			return fmt.Errorf("stage %s: %w", rel, err)
		}
		rewrites[abs] = "./" + filepath.ToSlash(rel)
		return nil
	}
	for _, svc := range p.Services {
		for _, v := range svc.Volumes {
			if v.Type == types.VolumeTypeBind {
				if err := stage(v.Source); err != nil {
					return nil, err
				}
			}
		}
		for _, ef := range svc.EnvFiles {
			if err := stage(ef.Path); err != nil {
				return nil, err
			}
		}
	}
	return rewrites, nil
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
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return copyFile(src, dst, info.Mode())
	}
	return filepath.Walk(src, func(path string, fi os.FileInfo, err error) error {
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
		return copyFile(path, target, fi.Mode())
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
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
	_, err = io.Copy(out, in)
	return err
}
