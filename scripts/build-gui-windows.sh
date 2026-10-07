#!/usr/bin/env bash
# Builds the GUI-enabled Windows/amd64 client artifact.
#
# The standard release matrix (scripts/build.sh) is headless: every artifact is
# built with CGO_ENABLED=0 and the "fyne" renderer host is excluded, so the
# client presents no window. This script produces the one windowed client,
# pixsee_client_gui_windows_amd64.exe, by enabling the "fyne" build tag with
# CGO. Fyne's Windows driver is cgo-backed, so it must be built with a C
# compiler (MinGW-w64 gcc) on PATH — the CGO_ENABLED=0 matrix cannot produce it.
#
# The artifact is linked as a Windows GUI-subsystem executable (-H=windowsgui)
# so double-clicking it opens the Fyne window with no extra console window, and
# with the MinGW runtime statically linked (-extldflags=-static) so it carries
# no libwinpthread/libgcc/libstdc++ DLL dependencies. A post-build import-table
# gate fails the build if any of those DLLs is still imported, before the
# artifact is checksummed or uploaded.
#
# The headless artifacts and their names are untouched; this only adds the GUI
# artifact to dist/ and refreshes dist/SHA256SUMS. See docs/architecture.md
# §2.3 for the full rationale.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST_DIR="${DIST_DIR:-$REPO_ROOT/dist}"

# The GUI client is Windows/amd64 only (docs/architecture.md §2.3).
GOOS_TARGET="windows"
GOARCH_TARGET="amd64"
ARTIFACT="pixsee_client_gui_${GOOS_TARGET}_${GOARCH_TARGET}.exe"

if ! command -v go >/dev/null 2>&1; then
  echo "error: the Go toolchain (go) is not on PATH" >&2
  exit 1
fi

# Fyne's Windows driver uses cgo, so a C compiler is mandatory here. Honor CC
# when set; otherwise Go's default compiler on windows/amd64 is gcc.
cc="${CC:-gcc}"
if ! command -v "$cc" >/dev/null 2>&1; then
  echo "error: no C compiler ($cc) on PATH; Fyne's Windows driver needs cgo." >&2
  echo "       Install MinGW-w64 (e.g. the mingw-w64-x86_64-gcc package from" >&2
  echo "       MSYS2) and retry, or set CC to your compiler." >&2
  exit 1
fi

# objdump inspects the PE import table for the gate below. Without it we cannot
# prove the artifact is free of MinGW runtime DLL dependencies, so its absence
# is a hard error rather than a silent skip. Honor OBJDUMP when set; MinGW-w64
# ships objdump alongside gcc.
objdump="${OBJDUMP:-objdump}"
if ! command -v "$objdump" >/dev/null 2>&1; then
  echo "error: no PE inspector ($objdump) on PATH; needed to verify the GUI" >&2
  echo "       artifact does not import MinGW runtime DLLs." >&2
  echo "       Install binutils (MinGW-w64 bundles objdump) and retry, or set" >&2
  echo "       OBJDUMP to your inspector." >&2
  exit 1
fi

mkdir -p "$DIST_DIR"
cd "$REPO_ROOT"

out="$DIST_DIR/$ARTIFACT"
echo "building GUI client ${GOOS_TARGET}/${GOARCH_TARGET} -> $ARTIFACT"
# -H=windowsgui: Windows GUI subsystem, so the windowed client gets no extra
#   console window when double-clicked (the headless client is unchanged and
#   keeps its console for diagnostics).
# -extldflags=-static: statically link the MinGW runtime (libgcc, libstdc++,
#   libwinpthread) so the artifact has no non-system DLL dependencies.
GOOS="$GOOS_TARGET" GOARCH="$GOARCH_TARGET" CGO_ENABLED=1 \
  go build -tags fyne \
  -ldflags "-H=windowsgui -extldflags=-static" \
  -o "$out" ./cmd/vdclient/

# Import-table gate: these three MinGW runtime DLLs are what a non-static
# MinGW build would leave as external dependencies. Fail before checksumming or
# uploading if any is still imported, so a broken artifact never ships.
#
# objdump's output format varies between binutils builds — some print resolved
# "DLL Name: <dll>" lines, some print only the raw import descriptor table — so
# we match the DLL name anywhere in the listing and, as a backstop, scan the raw
# file bytes, where a dynamically imported DLL's name is stored as an ASCII
# string. Either hit means the runtime was not statically linked.
echo "verifying $ARTIFACT imports no MinGW runtime DLLs"
listing="$("$objdump" -p "$out")"
for dll in libwinpthread-1.dll libgcc_s_seh-1.dll libstdc++-6.dll; do
  if grep -qiF "$dll" <<<"$listing" || grep -aqF "$dll" "$out"; then
    echo "error: $ARTIFACT depends on $dll; the MinGW runtime was not statically linked." >&2
    exit 1
  fi
done

# Refresh SHA256SUMS over everything present in dist/, matching
# scripts/build.sh (which does the same after the headless matrix). Any
# pre-existing headless artifacts are re-hashed too, so the file stays one
# consistent index whether this runs alone or after `make build-all`.
(cd "$DIST_DIR" && rm -f SHA256SUMS && sha256sum -- * > SHA256SUMS)

echo "GUI client built; artifact and checksum in $DIST_DIR"
