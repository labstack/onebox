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

# Strictly validate canonical specs and every active OpenSpec change.
docs-check:
    npx --yes @fission-ai/openspec@{{ openspec_version }} validate --all --strict --no-interactive

# Create and publish the next vYEAR.MONTH.SEQUENCE tag from releasable main.
release:
    #!/bin/bash
    set -euo pipefail
    validate_release_checkout() {
      local release_branch
      release_branch=$(git rev-parse --abbrev-ref HEAD)
      if [ "$release_branch" != "main" ]; then
        echo "release must run from main." >&2; return 1
      fi
      if ! git diff --quiet || ! git diff --cached --quiet; then
        echo "release requires a clean tracked worktree." >&2; return 1
      fi
    }
    validate_release_checkout
    release_head=$(git rev-parse HEAD)
    git fetch origin --tags main
    if [ "$release_head" != "$(git rev-parse origin/main)" ]; then
      echo "release must run from an up-to-date main (HEAD must equal origin/main)." >&2; exit 1
    fi
    just check
    just docs-check
    validate_release_checkout
    if [ "$(git rev-parse HEAD)" != "$release_head" ]; then
      echo "release HEAD changed while checks were running." >&2; exit 1
    fi
    # Refresh both branch and tag state after checks. The push below also leases
    # main to this exact commit, closing the race between this fetch and publish.
    git fetch origin --tags main
    if [ "$release_head" != "$(git rev-parse origin/main)" ]; then
      echo "origin/main advanced while release checks were running; retry from updated main." >&2; exit 1
    fi
    release_month=$(date +%Y.%m)
    release_tags=$(git tag --list "v${release_month}.*" --sort=-v:refname) || exit 1
    release_month_pattern=${release_month//./\\.}
    release_last=$(printf '%s\n' "$release_tags" | { grep -E "^v${release_month_pattern}\.[1-9][0-9]*$" || true; } | head -1)
    if [ -z "$release_last" ]; then release_number=1; else release_number=$(( ${release_last##*.} + 1 )); fi
    release_tag="v${release_month}.${release_number}"
    echo "tagging $release_tag"
    git tag "$release_tag" "$release_head"
    git push --atomic \
      --force-with-lease="refs/heads/main:${release_head}" \
      origin \
      "${release_head}:refs/heads/main" \
      "refs/tags/${release_tag}:refs/tags/${release_tag}" \
      || { git tag -d "$release_tag" >/dev/null; exit 1; }
    echo "published $release_tag"

# Remove the installed binary.
clean:
    #!/bin/bash
    set -euo pipefail
    ob_build_dir="${OB_BIN_DIR:-${HOME}/.local/bin}"
    rm -f "${ob_build_dir}/ob"
