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
    bash scripts/release.sh

# Remove the installed binary.
clean:
    #!/bin/bash
    set -euo pipefail
    ob_build_dir="${OB_BIN_DIR:-${HOME}/.local/bin}"
    rm -f "${ob_build_dir}/ob"
