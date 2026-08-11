#!/usr/bin/env bash
set -euo pipefail

dist_dir=${1:-dist}
metadata=${dist_dir}/metadata.json
if [ ! -f "$metadata" ]; then
  echo "release metadata is missing: ${metadata}" >&2
  exit 1
fi

release_version=$(jq -r '.version // empty' "$metadata")
release_tag=$(jq -r '.tag // empty' "$metadata")
if [ -z "$release_version" ] || [ -z "$release_tag" ]; then
  echo "release metadata has no version or tag." >&2
  exit 1
fi

expected_artifacts=(
  "onebox_${release_version}_checksums.txt"
  "onebox_${release_version}_darwin_amd64.tar.gz"
  "onebox_${release_version}_darwin_arm64.tar.gz"
  "onebox_${release_version}_linux_amd64.deb"
  "onebox_${release_version}_linux_amd64.rpm"
  "onebox_${release_version}_linux_amd64.tar.gz"
  "onebox_${release_version}_linux_arm64.deb"
  "onebox_${release_version}_linux_arm64.rpm"
  "onebox_${release_version}_linux_arm64.tar.gz"
  "onebox_${release_version}_windows_amd64.zip"
  "onebox_${release_version}_windows_arm64.zip"
)

actual_artifacts=()
while IFS= read -r artifact; do
  actual_artifacts+=("$artifact")
done < <(find "$dist_dir" -maxdepth 1 -type f -name 'onebox_*' -exec basename {} \; | LC_ALL=C sort)
if [ "${actual_artifacts[*]}" != "${expected_artifacts[*]}" ]; then
  echo "release artifact inventory differs from the contract." >&2
  diff -u <(printf '%s\n' "${expected_artifacts[@]}") <(printf '%s\n' "${actual_artifacts[@]}") >&2 || true
  exit 1
fi

checksum_file="${dist_dir}/onebox_${release_version}_checksums.txt"
distributable_count=$(( ${#expected_artifacts[@]} - 1 ))
if [ "$(wc -l < "$checksum_file" | tr -d ' ')" -ne "$distributable_count" ]; then
  echo "checksum manifest must cover exactly ${distributable_count} distributable artifacts." >&2
  exit 1
fi
if command -v sha256sum >/dev/null 2>&1; then
  (cd "$dist_dir" && sha256sum --check "$(basename "$checksum_file")")
else
  (cd "$dist_dir" && shasum -a 256 -c "$(basename "$checksum_file")")
fi

# Every later hash comparison reads this manifest rather than recomputing: the
# bytes behind it were just checked, and one source cannot disagree with itself.
artifact_hash() {
  awk -v name="$1" '$2 == name || $2 == "*" name { print $1 }' "$checksum_file"
}

for platform in darwin_amd64 darwin_arm64 linux_amd64 linux_arm64; do
  archive="${dist_dir}/onebox_${release_version}_${platform}.tar.gz"
  entries=$(tar -tzf "$archive" | LC_ALL=C sort)
  if [ "$entries" != $'README.md\nob' ]; then
    echo "unexpected contents in $(basename "$archive"):" >&2
    printf '%s\n' "$entries" >&2
    exit 1
  fi
done
for platform in windows_amd64 windows_arm64; do
  archive="${dist_dir}/onebox_${release_version}_${platform}.zip"
  entries=$(unzip -Z1 "$archive" | LC_ALL=C sort)
  if [ "$entries" != $'README.md\nob.exe' ]; then
    echo "unexpected contents in $(basename "$archive"):" >&2
    printf '%s\n' "$entries" >&2
    exit 1
  fi
done

# What the contract promises about a package is where it puts the binary, which
# is the one thing nfpm's own output format cannot guarantee on its behalf.
if command -v dpkg-deb >/dev/null 2>&1; then
  for arch in amd64 arm64; do
    deb="${dist_dir}/onebox_${release_version}_linux_${arch}.deb"
    if ! dpkg-deb -c "$deb" | grep -q ' \./usr/bin/ob$'; then
      echo "Debian package does not install /usr/bin/ob: $(basename "$deb")" >&2
      exit 1
    fi
  done
fi

scoop_manifest="${dist_dir}/scoop/onebox.json"
if [ ! -f "$scoop_manifest" ]; then
  echo "Scoop manifest is missing: ${scoop_manifest}" >&2
  exit 1
fi
if [ "$(jq -r '.version' "$scoop_manifest")" != "$release_version" ]; then
  echo "Scoop manifest version does not match ${release_version}." >&2
  exit 1
fi
for architecture in 64bit:amd64 arm64:arm64; do
  scoop_arch=${architecture%%:*}
  artifact_arch=${architecture##*:}
  expected_url="https://github.com/labstack/onebox/releases/download/${release_tag}/onebox_${release_version}_windows_${artifact_arch}.zip"
  actual_url=$(jq -r --arg arch "$scoop_arch" '.architecture[$arch].url' "$scoop_manifest")
  if [ "$actual_url" != "$expected_url" ]; then
    echo "Scoop ${scoop_arch} URL = ${actual_url}, want ${expected_url}." >&2
    exit 1
  fi
  if ! jq -e --arg arch "$scoop_arch" '.architecture[$arch].bin == ["ob.exe"]' "$scoop_manifest" >/dev/null; then
    echo "Scoop ${scoop_arch} entry does not install ob.exe." >&2
    exit 1
  fi
  expected_hash=$(artifact_hash "onebox_${release_version}_windows_${artifact_arch}.zip")
  actual_hash=$(jq -r --arg arch "$scoop_arch" '.architecture[$arch].hash' "$scoop_manifest")
  if [ "$actual_hash" != "$expected_hash" ]; then
    echo "Scoop ${scoop_arch} hash does not match its Windows archive." >&2
    exit 1
  fi
done

cask="${dist_dir}/homebrew/Casks/onebox.rb"
if [ ! -f "$cask" ]; then
  echo "Homebrew Cask is missing: ${cask}" >&2
  exit 1
fi
if ! grep -Fq 'cask "onebox" do' "$cask" || ! grep -Fq 'binary "ob"' "$cask"; then
  echo "Homebrew Cask does not install ob." >&2
  exit 1
fi
# The cask holds a url and sha256 for macOS AND Linux, on Intel and ARM. A
# file-wide grep therefore still passes when two of them are swapped, which is a
# cask that hands every user the wrong digest — so read each block on its own.
for pair in intel:amd64 arm:arm64; do
  cask_arch=${pair%%:*}
  artifact_arch=${pair##*:}
  block=$(awk '/on_macos do/,/^  end$/' "$cask" | awk "/on_${cask_arch} do/,/end/")
  # The cask interpolates the file name from its own version, so the fixed part
  # to assert is the release the URL points at and the architecture it serves.
  url=$(grep -m 1 '^ *url "' <<< "$block")
  if [[ "$url" != *"/download/${release_tag}/"* || "$url" != *"_darwin_${artifact_arch}.tar.gz"* ]]; then
    echo "Homebrew Cask ${artifact_arch} URL does not point at ${release_tag} for that architecture: ${url}" >&2
    exit 1
  fi
  if ! grep -Fq "$(artifact_hash "onebox_${release_version}_darwin_${artifact_arch}.tar.gz")" <<< "$block"; then
    echo "Homebrew Cask ${artifact_arch} hash does not match its macOS archive." >&2
    exit 1
  fi
done

case "$(uname -s)-$(uname -m)" in
  Darwin-arm64) native_binary="${dist_dir}/ob_darwin_arm64_v8.0/ob" ;;
  Darwin-x86_64) native_binary="${dist_dir}/ob_darwin_amd64_v1/ob" ;;
  Linux-aarch64 | Linux-arm64) native_binary="${dist_dir}/ob_linux_arm64_v8.0/ob" ;;
  Linux-x86_64 | Linux-amd64) native_binary="${dist_dir}/ob_linux_amd64_v1/ob" ;;
  *)
    echo "unsupported verification host: $(uname -s)-$(uname -m)" >&2
    exit 1
    ;;
esac
if [ -n "$native_binary" ]; then
  version_output=$("$native_binary" version --output json)
  if ! grep -Fq "\"version\": \"${release_tag}\"" <<< "$version_output"; then
    echo "native binary does not report release tag ${release_tag}." >&2
    exit 1
  fi
  if grep -Fq '"build_time": ""' <<< "$version_output"; then
    echo "native binary has no embedded build time." >&2
    exit 1
  fi
fi

echo "verified 6 archives, 4 Linux packages, ${distributable_count} checksums, Homebrew, Scoop, and embedded build metadata for ${release_tag}"
