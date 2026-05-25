#!/usr/bin/env bash
# Build a stripped static workhorse-agent binary for one or many targets.
#
# Usage:
#   scripts/build.sh                          # host platform
#   scripts/build.sh linux/amd64              # one target
#   scripts/build.sh linux/amd64 darwin/arm64 # several targets
#   scripts/build.sh all                      # full Linux/macOS/Windows × amd64/arm64 matrix
#
# Output:  dist/workhorse-agent[-<goos>-<goarch>][.exe]

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST_DIR="${REPO_ROOT}/dist"
PKG="./cmd/workhorse-agent"

# Reproducibility: -trimpath strips local paths; -s -w drops debug + symbol
# tables to keep the binary small. CGO disabled because modernc.org/sqlite is
# pure Go and we have no other C dependencies.
LDFLAGS="-s -w"
BUILDFLAGS=(-trimpath -ldflags="${LDFLAGS}")

ALL_MATRIX=(
  "linux/amd64"
  "linux/arm64"
  "darwin/amd64"
  "darwin/arm64"
  "windows/amd64"
  "windows/arm64"
)

declare -a TARGETS=()
if [ $# -eq 0 ]; then
  TARGETS=("host")
elif [ "$1" = "all" ]; then
  TARGETS=("${ALL_MATRIX[@]}")
else
  TARGETS=("$@")
fi

mkdir -p "${DIST_DIR}"
cd "${REPO_ROOT}"

build_one() {
  local target="$1"
  local goos goarch out ext=""
  if [ "${target}" = "host" ]; then
    goos="$(go env GOOS)"
    goarch="$(go env GOARCH)"
    out="${DIST_DIR}/workhorse-agent"
  else
    goos="${target%/*}"
    goarch="${target##*/}"
    out="${DIST_DIR}/workhorse-agent-${goos}-${goarch}"
  fi
  if [ "${goos}" = "windows" ]; then
    ext=".exe"
  fi
  out="${out}${ext}"

  echo ">> building ${goos}/${goarch} → ${out}"
  GOOS="${goos}" GOARCH="${goarch}" CGO_ENABLED=0 \
    go build "${BUILDFLAGS[@]}" -o "${out}" "${PKG}"
}

for t in "${TARGETS[@]}"; do
  build_one "${t}"
done

echo
echo "built artifacts:"
ls -lh "${DIST_DIR}"
