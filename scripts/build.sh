#!/usr/bin/env bash
# Cross-compiles the project's release binaries for every OS/arch pair in the
# build matrix, naming each output pixsee_<artifact>_<os>_<arch>[.exe] so the
# artifact is directly runnable on its target OS (.exe on Windows, no
# extension on Linux/macOS) and identifiable as a Pixsee release artifact.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST_DIR="${DIST_DIR:-$REPO_ROOT/dist}"

# Binaries built and published as release artifacts, mapped from their
# ./cmd/<dir> source directory to the artifact name used in
# pixsee_<artifact>_<os>_<arch>. vdhost/vdclient are the user-facing
# host/client binaries; e2e/captest/rt are internal dev/test tools shipped
# with the same naming scheme for consistency.
declare -A ARTIFACT_NAMES=(
  [vdhost]="host"
  [vdclient]="client"
  [e2e]="e2e"
  [captest]="captest"
  [rt]="rt"
)
CMDS=(vdhost vdclient e2e captest rt)

# Supported OS/arch pairs. Keep in sync with docs/architecture.md and the
# release process; add a line here to add a platform to the build matrix.
TARGETS=(
  "linux amd64"
  "linux arm64"
  "windows amd64"
  "windows arm64"
  "darwin amd64"
  "darwin arm64"
)

mkdir -p "$DIST_DIR"
rm -f "${DIST_DIR:?}"/*

cd "$REPO_ROOT"

fail=0
for cmd in "${CMDS[@]}"; do
  name="${ARTIFACT_NAMES[$cmd]}"
  for target in "${TARGETS[@]}"; do
    os="${target%% *}"
    arch="${target##* }"

    # Windows binaries must end in .exe to be runnable (double-clickable,
    # found by PATHEXT, etc). Linux and macOS binaries take no extension.
    ext=""
    if [ "$os" = "windows" ]; then
      ext=".exe"
    fi

    out="$DIST_DIR/pixsee_${name}_${os}_${arch}${ext}"
    echo "building ${cmd} ${os}/${arch} -> $(basename "$out")"
    if ! GOOS="$os" GOARCH="$arch" CGO_ENABLED=0 go build -o "$out" "./cmd/${cmd}/"; then
      echo "FAILED: ${cmd} ${os}/${arch}" >&2
      fail=1
    fi
  done
done

if [ "$fail" -ne 0 ]; then
  echo "one or more builds failed" >&2
  exit 1
fi

(cd "$DIST_DIR" && sha256sum -- * > SHA256SUMS)

echo "all builds succeeded; artifacts in $DIST_DIR"
