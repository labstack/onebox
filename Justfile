openspec_version := "1.7.0"

# Build the CLI into a user-local directory on PATH.
default: build

# Build the ob binary.
build:
    #!/bin/bash
    set -euo pipefail
    ob_build_dir="${OB_BIN_DIR:-${HOME}/.local/bin}"
    ob_build_version="${OB_VERSION:-}"
    if [ -n "$ob_build_version" ] && [[ ! "$ob_build_version" =~ ^v[0-9]{4}\.(0[1-9]|1[0-2])\.[1-9][0-9]*$ ]]; then
      echo "OB_VERSION must match vYEAR.MONTH.SEQUENCE" >&2; exit 1
    fi
    if [ -z "$ob_build_version" ]; then
      ob_build_version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
    fi
    ob_build_time=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    mkdir -p "$ob_build_dir"
    go build -ldflags "-X github.com/labstack/onebox/internal/buildinfo.release=${ob_build_version} -X github.com/labstack/onebox/internal/buildinfo.buildTime=${ob_build_time}" -o "${ob_build_dir}/ob" ./cmd/ob
    echo "built ${ob_build_dir}/ob"

# Install the ob binary (alias for build).
install: build

# Run the test suite.
test:
    go test ./...

# Run Go's static analyzer.
vet:
    go vet ./...

# Format all Go packages.
fmt:
    go fmt ./...

# Run all non-mutating checks.
check: test vet

# Regenerate the parts of the documentation site that are derived from Go.
#
# The project-file field reference, the error-code catalogue and the CLI
# reference are all enumerated in the binary already. Writing them again by hand
# would create a second source that can disagree with the first, so this command
# is their only writer. The CLI pages need a built `ob` on PATH; without one
# they are skipped rather than written stale.
docs-generate: build
    go run ./cmd/ob-docgen

# Fail when a generated documentation page is behind the binary.
docs-generate-check: build
    go run ./cmd/ob-docgen --check

# Install the documentation site's dependencies.
site-install:
    cd site && npm install --no-audit --no-fund

# Serve the documentation site locally with live reload.
site: docs-generate
    cd site && npm run dev

# Build the documentation site into site/dist.
site-build: docs-generate
    cd site && npm run build

# Strictly validate canonical specs and every active OpenSpec change.
docs-check: diagrams-check docs-generate-check
    npx --yes @fission-ai/openspec@{{ openspec_version }} validate --all --strict --no-interactive

# Render every openspec .d2 source to the .svg committed beside it, and record
# each source's hash so drift is detectable without d2 installed.
diagrams:
    #!/usr/bin/env bash
    set -euo pipefail
    command -v d2 >/dev/null || { echo "d2 not installed: brew install d2" >&2; exit 1; }
    shopt -s nullglob
    for dir in openspec/*/*/diagrams openspec/*/*/*/diagrams; do
        manifest="$dir/.manifest"
        : >"$manifest"
        for src in "$dir"/*.d2; do
            out="${src%.d2}.svg"
            # elk routes orthogonally and reads better than dagre's curves for
            # most of these, but neither engine honours `direction` inside a
            # container, and elk handles that case far worse. A source can opt
            # out with a `# d2-layout: dagre` line.
            #
            # --dark-theme embeds a prefers-color-scheme block so the diagrams
            # follow GitHub's dark mode. That only works while the sources set
            # no explicit fills: an explicit style wins over the theme in BOTH
            # modes, so a light pastel fill would survive onto a dark ground.
            # Semantic colour therefore lives on strokes and labels only.
            layout="$(sed -n 's/^# d2-layout: *//p' "$src" | head -1)"
            d2 --layout "${layout:-elk}" --theme 1 --dark-theme 200 --pad 24 "$src" "$out" >/dev/null
            printf '%s  %s\n' "$(shasum -a 256 "$src" | cut -d' ' -f1)" "$(basename "$src")" >>"$manifest"
        done
        echo "rendered $(ls "$dir"/*.d2 2>/dev/null | wc -l | tr -d ' ') diagram(s) in $dir"
    done

# Fail when a .d2 source changed without `just diagrams` being re-run.
#
# Compares each source against the hash recorded when its .svg was rendered,
# rather than re-rendering and diffing. That keeps this runnable in CI with no
# d2 dependency, and avoids false failures from d2 version differences changing
# SVG output. It catches the real mistake — editing a diagram and forgetting to
# regenerate — but does not verify hand-edited SVG content.
diagrams-check:
    #!/usr/bin/env bash
    set -euo pipefail
    shopt -s nullglob
    status=0
    for dir in openspec/*/*/diagrams openspec/*/*/*/diagrams; do
        manifest="$dir/.manifest"
        if [[ ! -f "$manifest" ]]; then
            echo "missing $manifest — run: just diagrams" >&2
            status=1
            continue
        fi
        for src in "$dir"/*.d2; do
            name="$(basename "$src")"
            [[ -f "${src%.d2}.svg" ]] || { echo "missing SVG for $src — run: just diagrams" >&2; status=1; continue; }
            recorded="$(awk -v n="$name" '$2 == n {print $1}' "$manifest")"
            [[ -n "$recorded" ]] || { echo "$src is not in $manifest — run: just diagrams" >&2; status=1; continue; }
            actual="$(shasum -a 256 "$src" | cut -d' ' -f1)"
            [[ "$recorded" == "$actual" ]] || { echo "$src changed since its SVG was rendered — run: just diagrams" >&2; status=1; }
        done
        for recorded_name in $(awk '{print $2}' "$manifest"); do
            [[ -f "$dir/$recorded_name" ]] || { echo "$dir/$recorded_name is in the manifest but gone — run: just diagrams" >&2; status=1; }
        done
    done
    exit $status

# Create and publish the next vYEAR.MONTH.SEQUENCE tag from releasable main.
release:
    bash scripts/release.sh

# Remove the installed binary.
clean:
    #!/bin/bash
    set -euo pipefail
    ob_build_dir="${OB_BIN_DIR:-${HOME}/.local/bin}"
    rm -f "${ob_build_dir}/ob"
