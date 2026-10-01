#!/usr/bin/env bash
# docs-check — advisory doc/source consistency gate.
#
# Warns (never fails) when documentation drifts from the canonical build
# sources:
#
#   (a) the OS/arch build matrix in scripts/build.sh must match the platform
#       list in docs/architecture.md (§2.1) and README.md.
#   (b) the binary list in the Makefile must match the command list in
#       docs/architecture.md (§9).
#
# Advisory by design (spec D5): it prints warnings and always exits 0, so it
# can be wired into CI or a pre-commit hook without ever blocking a change.
# A drifted doc is a warning, not a build failure.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

DOC="docs/architecture.md"
README="README.md"
BUILD="scripts/build.sh"
MAKEFILE="Makefile"

# normalize: fold to a canonical "goos arch" token (macOS -> darwin, the GOOS
# name build.sh uses), squeeze whitespace, one per line, sorted unique.
norm() {
  sed -E 's/Linux/linux/;s/Windows/windows/;s/macOS/darwin/' \
    | tr -s ' \t' ' ' \
    | grep -oE '[a-z]+ (amd64|arm64)' \
    | tr 'A-Z' 'a-z' \
    | sort -u
}

warn=0

# --- (a) OS/arch: build.sh vs doc §2.1 vs README matrix ---
sh_targets="$(grep -oE '"(linux|windows|darwin) (amd64|arm64)"' "$BUILD" | tr -d '"' | norm)"
doc_targets="$(awk '/^### 2.1/{f=1} /### 2.2/{f=0} f' "$DOC" | grep -oE '(Linux|Windows|macOS) (amd64|arm64)' | norm)"
readme_targets="$(awk 'NR>=47 && NR<=52' "$README" | tr '|' ' ' | norm)"

if [ -n "$sh_targets" ] && [ "$sh_targets" != "$doc_targets" ]; then
  echo "WARN docs-check: OS/arch in $BUILD != $DOC §2.1" >&2
  warn=1
fi
if [ -n "$sh_targets" ] && [ "$sh_targets" != "$readme_targets" ]; then
  echo "WARN docs-check: OS/arch in $BUILD != README.md matrix" >&2
  warn=1
fi

# --- (b) binaries: Makefile vs doc §9 ---
make_bins="$(grep -oE 'for bin in [a-z0-9 ]+' "$MAKEFILE" | sed 's/for bin in //' | tr ' ' '\n' | grep -E '^[a-z0-9]+$' | sort | tr '\n' ' ' | sed 's/ *$//')"
doc_bins="$(awk '/^## 9\./{f=1} /^## 10\./{f=0} f' "$DOC" | grep -oE 'cmd/[a-z0-9]+' | sed 's|cmd/||' | sort | tr '\n' ' ' | sed 's/ *$//')"

if [ -n "$make_bins" ] && [ -n "$doc_bins" ] && [ "$make_bins" != "$doc_bins" ]; then
  echo "WARN docs-check: binaries in $MAKEFILE ($make_bins) != $DOC §9 ($doc_bins)" >&2
  warn=1
fi

if [ "$warn" -eq 0 ]; then
  echo "docs-check: OK — build matrix and binaries in sync"
else
  echo "docs-check: drift detected (see warnings above)" >&2
fi

# Advisory: never fail, even on drift.
exit 0
