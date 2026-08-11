#!/usr/bin/env bash
set -euo pipefail

release_tag=${1:-${GITHUB_REF_NAME:-}}
main_ref=${2:-origin/main}

# The revision is capped at nineteen digits because that is the widest value the
# runner's own parser can hold: every 19-digit number fits in a uint64, and a tag
# it cannot parse is a release with no usable provenance.
if [[ ! "$release_tag" =~ ^v[1-9][0-9]{3}\.([1-9]|1[0-2])\.(0|[1-9][0-9]{0,18})$ ]]; then
  echo "release tag must match vYYYY.M.REVISION." >&2
  exit 1
fi

if ! release_commit=$(git rev-parse --verify "refs/tags/${release_tag}^{commit}" 2>/dev/null); then
  echo "release tag ${release_tag} does not resolve to a commit." >&2
  exit 1
fi
if ! main_commit=$(git rev-parse --verify "${main_ref}^{commit}" 2>/dev/null); then
  echo "main lineage ref ${main_ref} does not resolve to a commit." >&2
  exit 1
fi

head_commit=$(git rev-parse HEAD)
if [ "$head_commit" != "$release_commit" ]; then
  echo "checked-out commit ${head_commit} does not match ${release_tag} (${release_commit})." >&2
  exit 1
fi
if ! git merge-base --is-ancestor "$release_commit" "$main_commit"; then
  echo "release tag ${release_tag} is not reachable from ${main_ref}." >&2
  exit 1
fi

echo "validated ${release_tag} at ${release_commit} on ${main_ref}"
