#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
version="${VERSION:-dev}"
commit="${COMMIT:-$(git rev-parse HEAD)}"
build_date="${BUILD_DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"
output="${OUTPUT_DIR:-dist/release}"

# Values become filenames and Go linker flags. Accept only token characters.
if [[ ! "$version" =~ ^[a-zA-Z0-9][a-zA-Z0-9._-]*$ ]] ||
   [[ ! "$commit" =~ ^[a-fA-F0-9]+$ ]] ||
   [[ ! "$build_date" =~ ^[0-9TZ:+-]+$ ]]; then
  echo 'Invalid release version, commit, or build date' >&2
  exit 1
fi

mkdir -p "$output"
stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT
archives=()

for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  goos="${target%/*}"
  goarch="${target#*/}"
  directory="$stage/$goos-$goarch"
  mkdir -p "$directory"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -trimpath \
    -ldflags="-s -w -X teo-ai/internal/buildinfo.Version=$version -X teo-ai/internal/buildinfo.Commit=$commit -X teo-ai/internal/buildinfo.Date=$build_date" \
    -o "$directory/teo-ai" ./cmd/teo-ai
  cp README.md "$directory/README.md"
  archive="teo-ai_${version}_${goos}_${goarch}.tar.gz"
  tar -czf "$output/$archive" -C "$directory" teo-ai README.md
  archives+=("$archive")
done

(
  cd "$output"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "${archives[@]}" > checksums.txt
  else
    shasum -a 256 "${archives[@]}" > checksums.txt
  fi
)
printf 'Release archives and checksums: %s\n' "$output"
