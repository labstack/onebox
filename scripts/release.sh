#!/usr/bin/env bash
set -euo pipefail

validate_release_checkout() {
  local release_branch
  release_branch=$(git rev-parse --abbrev-ref HEAD)
  if [ "$release_branch" != "main" ]; then
    echo "release must run from main." >&2
    return 1
  fi
  if ! git diff --quiet || ! git diff --cached --quiet; then
    echo "release requires a clean tracked worktree." >&2
    return 1
  fi
}

release_tag=""
release_tag_created=0
cleanup_failed_release() {
  local release_status=$?
  trap - EXIT
  if [ "$release_status" -ne 0 ] && [ "$release_tag_created" -eq 1 ]; then
    git tag -d "$release_tag" >/dev/null 2>&1 || true
  fi
  exit "$release_status"
}
trap cleanup_failed_release EXIT

validate_release_checkout
release_head=$(git rev-parse HEAD)
git fetch origin --tags main
if [ "$release_head" != "$(git rev-parse origin/main)" ]; then
  echo "release must run from an up-to-date main (HEAD must equal origin/main)." >&2
  exit 1
fi

just check
just docs-check

validate_release_checkout
if [ "$(git rev-parse HEAD)" != "$release_head" ]; then
  echo "release HEAD changed while checks were running." >&2
  exit 1
fi

# Refresh both branch and tag state after checks. The push below also leases
# main to this exact commit, closing the race between this fetch and publish.
git fetch origin --tags main
if [ "$release_head" != "$(git rev-parse origin/main)" ]; then
  echo "origin/main advanced while release checks were running; retry from updated main." >&2
  exit 1
fi

release_calendar=$(date -u +%Y:%m)
if [[ ! "$release_calendar" =~ ^[0-9]{4}:(0[1-9]|1[0-2])$ ]]; then
  echo "could not determine the UTC release month." >&2
  exit 1
fi
release_year=${release_calendar%%:*}
release_month_padded=${release_calendar##*:}
release_month=$((10#$release_month_padded))
release_period="${release_year}.${release_month}"
release_tags=$(git tag --list "v${release_period}.*" --sort=-v:refname)
release_period_pattern=${release_period//./\\.}
release_last=""
while IFS= read -r release_candidate; do
  if [[ "$release_candidate" =~ ^v${release_period_pattern}\.(0|[1-9][0-9]{0,18})$ ]]; then
    release_last=$release_candidate
    break
  fi
done <<< "$release_tags"
if [ -z "$release_last" ]; then
  release_number=0
else
  # Bash arithmetic is signed 64-bit, so a revision the grammar still allows can
  # overflow it and wrap to a negative number that this script would then tag.
  # Refuse above the last value that increments safely instead.
  release_previous=${release_last##*.}
  if [ "${#release_previous}" -ge 19 ]; then
    echo "revision space for ${release_period} is exhausted at ${release_last}." >&2
    exit 1
  fi
  release_number=$((10#$release_previous + 1))
fi
release_tag="v${release_period}.${release_number}"
release_commit=$(printf 'chore(release): %s\n' "$release_tag" | git commit-tree "${release_head}^{tree}" -p "$release_head")

echo "tagging $release_tag"
git tag --no-sign "$release_tag" "$release_commit"
release_tag_created=1
git push --atomic \
  --force-with-lease="refs/heads/main:${release_head}" \
  origin \
  "${release_commit}:refs/heads/main" \
  "refs/tags/${release_tag}:refs/tags/${release_tag}"
release_tag_created=0
if ! git update-ref -m "release $release_tag" refs/heads/main "$release_commit" "$release_head"; then
  echo "published $release_tag, but local main did not advance; fetch origin/main before continuing." >&2
fi
echo "published $release_tag"
