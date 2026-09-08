#!/bin/bash
export PATH=$PATH:/opt/data/work/.worktrees/t_6ecc54e1/.tools/go/bin
export GOTOOLCHAIN=local
export GOFLAGS=-mod=mod
cd /opt/data/work/.worktrees/t_17ff703a

echo "=== vdhost cross-compile (imports x11 adapter) ==="
for pair in windows/amd64 windows/arm64 darwin/amd64 darwin/arm64 linux/amd64; do
  os=${pair%/*}; arch=${pair#*/}
  echo -n "vdhost $pair: "
  if GOOS=$os GOARCH=$arch go build -o /tmp/vdhost_bin ./cmd/vdhost/ 2>/tmp/err.txt; then
    echo "BUILD OK"
  else
    echo "FAIL:"; head -3 /tmp/err.txt
  fi
done

echo "=== e2e cross-compile (imports x11 adapter) ==="
for pair in windows/amd64 darwin/arm64 linux/amd64; do
  os=${pair%/*}; arch=${pair#*/}
  echo -n "e2e $pair: "
  if GOOS=$os GOARCH=$arch go build -o /tmp/e2e_bin ./cmd/e2e/ 2>/tmp/err.txt; then
    echo "BUILD OK"
  else
    echo "FAIL:"; head -3 /tmp/err.txt
  fi
done

echo "=== vdclient cross-compile (renderer_fyne is fyne-tagged, so core builds) ==="
for pair in windows/amd64 darwin/arm64 linux/amd64; do
  os=${pair%/*}; arch=${pair#*/}
  echo -n "vdclient $pair: "
  if GOOS=$os GOARCH=$arch go build -o /tmp/vdclient_bin ./cmd/vdclient/ 2>/tmp/err.txt; then
    echo "BUILD OK"
  else
    echo "FAIL:"; head -3 /tmp/err.txt
  fi
done

echo "=== go vet linux/amd64 (non-fyne packages) ==="
go vet ./internal/... ./cmd/... 2>&1 | head -10
echo "vet exit=$?"

echo "=== go test linux/amd64 ==="
go test ./internal/... ./cmd/... 2>&1 | tail -20
echo "test exit=$?"
