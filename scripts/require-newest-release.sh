#!/usr/bin/env bash
set -euo pipefail

# Homebrew and Scoop hold one version each, so publishing a tag behind the
# current release silently downgrades both package managers.
#
# The comparison is against what is PUBLISHED, never against local tags: a tag
# that merely exists says nothing about whether its release ran, and a queued
# release is not stale merely because a newer tag was created while it waited.
# It is also the only source a re-run of an old tag cannot argue with.
release_tag=${1:-${GITHUB_REF_NAME:-}}
repository=${2:-${GITHUB_REPOSITORY:-}}

if [ -z "$release_tag" ] || [ -z "$repository" ]; then
  echo "usage: require-newest-release.sh <tag> <owner/repo>" >&2
  exit 1
fi

# A list endpoint, so "no releases" is an empty array rather than a 404 that is
# indistinguishable from a 401, a rate limit, or a network failure. Any nonzero
# exit is a refusal: a guard that reads an outage as "nothing is published yet"
# turns itself off exactly when it cannot see what it is protecting.
if ! published=$(gh api "repos/${repository}/releases?per_page=1" --jq '.[0].tag_name // ""'); then
  echo "cannot read the published releases of ${repository}; refusing to publish ${release_tag} without knowing what it would replace." >&2
  exit 1
fi
if [ -z "$published" ]; then
  echo "no published release yet; ${release_tag} is the first"
  exit 0
fi
if [ "$published" = "$release_tag" ]; then
  # GoReleaser publishes the GitHub Release before the Cask and the Scoop
  # manifest, so a failure in either leaves this tag published and its package
  # metadata stale. That state is not repaired by re-running the same tag —
  # the release is immutable — but by cutting the next revision.
  echo "release ${release_tag} is already published; if its Homebrew or Scoop update failed, publish the repair as the next revision rather than re-running an immutable release." >&2
  exit 1
fi

# sort -V orders the release grammar correctly: fields are numeric and unpadded,
# which is exactly the case a lexicographic sort gets wrong.
newest=$(printf '%s\n%s\n' "$published" "$release_tag" | LC_ALL=C sort -V | tail -n 1)
if [ "$newest" != "$release_tag" ]; then
  echo "release tag ${release_tag} is behind the published ${published}; publishing it would downgrade Homebrew and Scoop." >&2
  exit 1
fi

echo "validated ${release_tag} as newer than the published ${published}"
