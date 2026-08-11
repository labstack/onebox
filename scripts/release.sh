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

# increment_decimal adds one to an arbitrarily wide non-negative decimal string,
# carrying by hand so no value is ever narrowed to a machine integer.
increment_decimal() {
  local digits=$1 carry=1 index result=""
  for ((index = ${#digits} - 1; index >= 0; index--)); do
    local sum=$(( ${digits:index:1} + carry ))
    result="$(( sum % 10 ))${result}"
    carry=$(( sum / 10 ))
  done
  if [ "$carry" -gt 0 ]; then
    result="${carry}${result}"
  fi
  printf '%s\n' "$result"
}

# Only one release may be in flight. GitHub orders queued runs by when they
# start waiting, not by when their tags were created, so two tags created close
# together can publish out of order: the newer one lands first and the older is
# then correctly refused at publish time — a release nobody gets. Refusing to
# create the next tag until the previous one is actually published removes the
# race rather than adjudicating it.
#
# The check is skipped only when origin is not a GitHub remote, which no real
# release is. Set OB_RELEASE_REPOSITORY to name the repository explicitly.
require_previous_release_published() {
  local repository=$1 previous="" candidate
  while IFS= read -r candidate; do
    if [[ "$candidate" =~ ^v[1-9][0-9]{3}\.([1-9]|1[0-2])\.(0|[1-9][0-9]{0,18})$ ]]; then
      previous=$candidate
      break
    fi
  done <<< "$(git tag --list 'v*' --sort=-v:refname)"
  if [ -z "$previous" ]; then
    return 0
  fi
  if ! command -v gh >/dev/null 2>&1; then
    echo "gh is required to confirm that ${previous} has published; install it or set OB_RELEASE_REPOSITORY=" >&2
    return 1
  fi
  if ! gh api "repos/${repository}/releases/tags/${previous}" >/dev/null 2>&1; then
    echo "previous release ${previous} has not published yet; wait for its release run to finish before creating the next tag." >&2
    return 1
  fi
}

github_repository_slug() {
  local remote=$1
  case "$remote" in
    https://github.com/*) printf '%s\n' "${remote#https://github.com/}" | sed 's/\.git$//' ;;
    git@github.com:*) printf '%s\n' "${remote#git@github.com:}" | sed 's/\.git$//' ;;
    *) printf '\n' ;;
  esac
}

release_repository=${OB_RELEASE_REPOSITORY:-$(github_repository_slug "$(git remote get-url origin)")}
if [ -n "$release_repository" ]; then
  require_previous_release_published "$release_repository"
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
  # Incremented as decimal text, not with Bash arithmetic: arithmetic is signed
  # 64-bit and the grammar admits revisions wider than that, which would wrap to
  # a negative number and be tagged. Exhaustion is a twenty-digit result, which
  # is the only value the contract cannot express.
  release_number=$(increment_decimal "${release_last##*.}")
  if [ "${#release_number}" -gt 19 ]; then
    echo "revision space for ${release_period} is exhausted at ${release_last}." >&2
    exit 1
  fi
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
