# Build the CLI into the checkout.
default: build

# Build the ob binary into ./bin, which is not on PATH.
#
# It used to build straight into ~/.local/bin, which put a checkout build in
# front of whatever `ob` the machine had installed — so `ob` meant the working
# tree rather than the release, and `ob doctor` reported the mismatch as a stale
# PATH candidate on every run. Shipping a binary onto PATH is a decision, not a
# side effect of compiling, so it now belongs to `just install`.
build:
    #!/bin/bash
    set -euo pipefail
    ob_build_dir="${OB_BIN_DIR:-bin}"
    ob_build_version="${OB_VERSION:-}"
    if [ -n "$ob_build_version" ] && [[ ! "$ob_build_version" =~ ^v[1-9][0-9]{3}\.([1-9]|1[0-2])\.(0|[1-9][0-9]{0,18})$ ]]; then
      echo "OB_VERSION must match vYYYY.M.REVISION" >&2; exit 1
    fi
    if [ -z "$ob_build_version" ]; then
      # --long keeps the commit suffix even on a tagged commit, so a checkout build
      # can never stamp a bare release identity and pass as a released runner.
      ob_build_version=$(git describe --tags --always --dirty --long 2>/dev/null || echo dev)
    fi
    ob_build_time=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    mkdir -p "$ob_build_dir"
    go build -ldflags "-X github.com/labstack/onebox/internal/buildinfo.release=${ob_build_version} -X github.com/labstack/onebox/internal/buildinfo.buildTime=${ob_build_time}" -o "${ob_build_dir}/ob" ./cmd/ob
    echo "built ${ob_build_dir}/ob"

# Put the built binary on PATH, deliberately.
#
# This is what shadows an installed release, so it says where it landed and
# what it will answer to.
install: build
    #!/bin/bash
    set -euo pipefail
    ob_install_dir="${OB_INSTALL_DIR:-${HOME}/.local/bin}"
    mkdir -p "$ob_install_dir"
    install -m 0755 "${OB_BIN_DIR:-bin}/ob" "${ob_install_dir}/ob"
    echo "installed ${ob_install_dir}/ob ($("${ob_install_dir}/ob" --version))"

# Run the test suite.
test:
    go test ./...

# Run Go's static analyzer.
vet:
    go vet ./...

# Format all Go packages.
fmt:
    go fmt ./...

# ---------- Verification ----------

# Local pre-commit gate. It contacts no target and writes nothing to the tree.
#
# Not hermetic in the stronger sense: `_mod-tidy` needs the module cache warm
# (or the network), and `site-build` needs `site/node_modules` — run
# `just site-install` once on a fresh clone.
check: _mod-tidy _fmt-check vet test docs-generate-check site-build
    @echo "All checks passed"

# CI adds the pinned lint, vulnerability, workflow and spec passes to the local
# gate.
#
# They are separate from `check` because each needs a tool the repository does
# not vendor; a contributor without them should still be able to run `just check`
# and get a truthful answer about their change.
ci: check lint vuln workflow-check
    @echo "CI checks passed"

[private]
_mod-tidy:
    go mod tidy -diff

# gofmt reports "nothing to do" and "I could not parse that file" the same way
# on stdout — the parse error goes to stderr and the listing comes back empty.
# Collapsing the two meant a file that does not compile passed this check, so the
# tool's exit status is read separately from its output.
[private]
_fmt-check:
    #!/usr/bin/env bash
    set -euo pipefail
    if ! unformatted="$(gofmt -l .)"; then
      echo "gofmt could not read the tree (see the errors above)" >&2
      exit 1
    fi
    if [[ -n "$unformatted" ]]; then
      echo "Unformatted files:" >&2
      echo "$unformatted" >&2
      exit 1
    fi

# Static analysis beyond `go vet`, configured in .golangci.yml. The config
# records which linters are held back and why, so the gate is green from the
# first run and a failure means the change in front of you.
lint:
    # `run` ignores an unknown config key in silence, so a typo would delete the
    # block it governs and leave the gate green. Only `config verify` is strict.
    golangci-lint config verify
    golangci-lint run ./...

# Exported identifiers under internal/ that nothing references. `unused` cannot
# see these — it treats every exported name as a package boundary — so they
# accumulate silently, which is how a superseded function survives its own
# replacement.
#
# Deliberately not part of `check` or `ci`, for two reasons. An exported helper
# may legitimately land before the caller that needs it, and a gate cannot tell
# that apart from something left behind. And this matches on the identifier
# alone, so two types with a same-named method are counted as one — a report
# that under-counts is fine as a prompt to go and look, and wrong as a rule that
# fails a build. Read the output, then confirm each hit before deleting.
dead-exports:
    #!/usr/bin/env bash
    set -euo pipefail
    # Definitions come from non-test files only: a Test/Benchmark/Fuzz function is
    # called by the testing package, not by name, and would report as unreferenced
    # every time. References are counted across tests too, because a helper a test
    # exercises is used.
    names=$(grep -rhoE '^func (\([^)]*\) )?[A-Z][A-Za-z0-9_]*\(' internal/ \
      --include='*.go' --exclude='*_test.go' \
      | sed -E 's/^func (\([^)]*\) )?//; s/\($//' | sort -u)
    if [ -z "${names}" ]; then
      echo "no exported identifiers found under internal/ — check this recipe's pattern" >&2
      exit 1
    fi
    dead=0
    while read -r name; do
      # One occurrence is the definition itself; anything more is a reference.
      if [ "$(grep -rhoE "\\b${name}\\(" internal/ cmd/ e2e/ --include='*.go' | wc -l)" -le 1 ]; then
        echo "no references: ${name}"
        dead=$((dead + 1))
      fi
    done <<< "${names}"
    echo "checked $(echo "${names}" | wc -l | tr -d ' ') exported identifiers, ${dead} unreferenced"

# Scan reachable code against the official vulnerability database.
vuln:
    govulncheck ./...

# Validate GitHub Actions YAML and the shell embedded in it.
workflow-check:
    #!/usr/bin/env bash
    set -euo pipefail
    # actionlint does not warn when it cannot find shellcheck — it silently stops
    # checking the shell inside `run:` blocks, which is half of what this recipe
    # claims to do.
    command -v shellcheck >/dev/null || {
      echo "shellcheck is not installed; actionlint would skip every run: block" >&2
      exit 1
    }
    actionlint

# The Docker end-to-end suite. Opt-in locally because it needs a working daemon;
# CI runs it as its own job so a slow suite never hides a fast failure.
e2e:
    OB_E2E=1 go test ./e2e/ -count=1 -timeout 20m

# Boot the throwaway server the `server-e2e` suite deploys to.
#
# Separate from the suite because the guest outlives a test run: booting takes
# about a minute, and iterating on a failing case should not pay for it again.
lima-up:
    bash scripts/lima.sh up

lima-down:
    bash scripts/lima.sh down

# The server end-to-end suite: the tests run here and reach a real machine over
# SSH, which is the transport every operator uses and the one the Docker suite
# substitutes local docker for.
#
# It boots the guest first rather than failing on a missing one, because the
# common case is not having booted it, and `lima-up` reuses a running instance.
server-e2e: lima-up
    bash scripts/lima.sh test

# Print the connection the suite uses, for running a single case by hand:
#   eval "$(just server-env | sed 's/^/export /')"
server-env:
    @bash scripts/lima.sh env

# Regenerate the parts of the documentation site that are derived from Go.
#
# The project-file field reference, the error-code catalogue and the CLI
# reference are all enumerated in the binary already. Writing them again by hand
# would create a second source that can disagree with the first, so this command
# is their only writer. The CLI reference is read out of the binary this recipe
# just built, named explicitly rather than resolved from PATH — otherwise an
# older `ob` installed elsewhere documents a tree it did not come from.
docs-generate: build
    go run ./cmd/ob-docgen --ob "${OB_BIN_DIR:-bin}/ob"

# Fail when a generated documentation page is behind the binary.
docs-generate-check: build
    go run ./cmd/ob-docgen --check --ob "${OB_BIN_DIR:-bin}/ob"

# Install the documentation site's dependencies.
site-install:
    cd site && npm install --no-audit --no-fund

# Serve the documentation site locally with live reload.
site: docs-generate
    cd site && npm run dev

# Build the documentation site into site/dist.
#
# Depends on the *checking* recipe, not the writing one: `check` is a gate, and a
# gate that rewrites the tree it is inspecting cannot tell you whether the tree
# was already right. Use `just site` for the writing path during development.
site-build: docs-generate-check
    cd site && npm run build

# Validate that every generated reference page matches the binary.
docs-check: docs-generate-check

# Create and publish the next vYYYY.M.REVISION tag from releasable main.
release:
    bash scripts/release.sh

# Remove the built binary.
#
# The checkout only, never the copy on PATH. While `build` wrote straight to
# ~/.local/bin, removing it there was removing what this recipe had put there;
# now that the build lands in ./bin, doing the same would delete a binary this
# checkout may never have created — including a release installed by hand
# through the steps the installation guide gives. `just uninstall` is how you
# ask for that, and it says so.
clean:
    #!/bin/bash
    set -euo pipefail
    rm -f "${OB_BIN_DIR:-bin}/ob"

# Remove the copy `just install` placed on PATH.
uninstall:
    #!/bin/bash
    set -euo pipefail
    ob_install_dir="${OB_INSTALL_DIR:-${HOME}/.local/bin}"
    rm -f "${ob_install_dir}/ob"
    echo "removed ${ob_install_dir}/ob"
