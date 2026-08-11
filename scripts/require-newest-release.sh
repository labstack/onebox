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

published=$(gh api "repos/${repository}/releases/latest" --jq '.tag_name' 2>/dev/null || true)
if [ -z "$published" ] || [ "$published" = "null" ]; then
  echo "no published release yet; ${release_tag} is the first"
  exit 0
fi
if [ "$published" = "$release_tag" ]; then
  echo "release ${release_tag} is already published" >&2
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
