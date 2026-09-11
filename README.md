# Virtual Desktop

A Go virtual desktop that streams one host display to a client over TLS 1.3
and forwards keyboard and pointer input back. The data plane is
deliberately narrow — host to client: display metadata and pixel updates
only; client to host: keyboard and pointer input only. Audio, clipboard,
file transfer, and shell/command access are out of scope. See
[`docs/architecture.md`](docs/architecture.md) for the full protocol and
design rationale.

The project ships two commands:

- `vdhost` — captures a local desktop, authenticates one client, and
  streams pixel updates.
- `vdclient` — connects to a `vdhost`, renders the remote display, and
  forwards local keyboard/pointer input.

Both are Cobra-based CLIs configured through [Viper](#configuration),
supporting flags, environment variables, and YAML config files.

## Requirements

- Go (see the `go` directive in `go.mod` for the minimum version).
- Linux: X11. Windows and macOS builds use native capture/input APIs
  (see `docs/architecture.md`).

## Building

Build the two CLIs (plus the `e2e`, `captest`, and `rt` support binaries)
for your current OS/arch with `go build`, or use the provided `Makefile`:

```sh
make build          # host-platform binaries only, written to dist/
```

### Cross-platform release build

`make build-all` (or `scripts/build.sh` directly) cross-compiles every
binary — `vdhost`, `vdclient`, `e2e`, `captest`, `rt` — for every
supported OS/arch pair and names each artifact with the
platform-appropriate extension:

| OS      | Arch          | Extension |
| ------- | ------------- | --------- |
| linux   | amd64, arm64  | none      |
| darwin  | amd64, arm64  | none      |
| windows | amd64, arm64  | `.exe`    |

```sh
make build-all
# or
./scripts/build.sh
```

Artifacts land in `dist/pixsee_<client|host|e2e|captest|rt>_<os>_<arch>[.exe]`
(e.g. `dist/pixsee_host_linux_amd64`,
`dist/pixsee_client_windows_amd64.exe`), built with `CGO_ENABLED=0`.
`make checksums` (or the build script itself) writes a `dist/SHA256SUMS`
file alongside them. `make clean` removes `dist/`.

## Running

```sh
# Host: listen on the default address, require a 32-byte token file.
./dist/pixsee_host_linux_amd64 --token /path/to/host.token --ca cert.pem --key key.pem

# Client: connect, pinning the host's CA.
./dist/pixsee_client_linux_amd64 --addr host:6511 --token /path/to/client.token --ca ca.pem
```

Both commands print Cobra-generated `--help` output listing every flag,
its default, and a short description:

```sh
./dist/pixsee_host_linux_amd64 --help
./dist/pixsee_client_linux_amd64 --help
```

Single-dash long flags (e.g. `-addr`) are still accepted for compatibility
with earlier invocations and scripts; `--addr` is equivalent. Short,
single-character flags (e.g. `-h`) are unaffected.

`vdhost` and `vdclient` can each run without a token (tokenless mode): a
host started without `--token` accepts any non-zero client token, and a
client started without `--token` mints a random one. A warning is printed
to stderr in this mode — see `docs/architecture.md` for the security
implications.

## Configuration

Both commands resolve every configuration value with the same four-tier
precedence, highest first:

1. **command-line flag** (e.g. `--addr`)
2. **environment variable** — `VDHOST_<FLAG_NAME>` for `vdhost`,
   `VDCLIENT_<FLAG_NAME>` for `vdclient` (dashes become underscores, e.g.
   `--capture-interval` → `VDHOST_CAPTURE_INTERVAL`)
3. **YAML config file** — pointed to by `--config`, or auto-discovered as
   `vdhost.yaml`/`vdclient.yaml` (`.yml`/`.json`/etc. also work) searched
   for in, in order: the current directory, `$HOME/.config/virtualdesktop`,
   then `/etc/virtualdesktop`
4. **the flag's built-in default**

This precedence is implemented once, in the shared `internal/cliconfig`
package, and used identically by both commands. A missing config file is
not an error — flags and environment variables alone are sufficient.

Example config files with every key documented are in
[`configs/vdhost.example.yaml`](configs/vdhost.example.yaml) and
[`configs/vdclient.example.yaml`](configs/vdclient.example.yaml). Copy one
to `vdhost.yaml`/`vdclient.yaml` (or pass it via `--config`) and uncomment
what you need, e.g.:

```yaml
# vdhost.yaml
addr: 127.0.0.1:6511
token: /etc/virtualdesktop/host.token
ca: /etc/virtualdesktop/host-cert.pem
key: /etc/virtualdesktop/host-key.pem
capture-interval: 33ms
keyframe-interval: 10s
max-input-per-sec: 500
enable-input: true
timeout: 10s
```

Run `vdhost --help` or `vdclient --help` for the authoritative, current
list of flags, defaults, and descriptions — every config file key and
environment variable name matches a flag name exactly.

## Testing

Run the full test suite:

```sh
go test ./...
# or
make test
```

`go vet ./...` (or `make vet`) is also part of the standard pre-commit
check.

Unit tests use [testify](https://github.com/stretchr/testify) (`assert`
and `require`) for assertions, and [mockery](https://github.com/vektra/mockery)-generated
mocks for the core interfaces (`host.Capture`, `host.Input`, `host.Peer`,
`client.Renderer`, `client.StateObserver`, `client.InputSender`). Mocks
live under `internal/host/mocks` and `internal/client/mocks` and are
checked into the repository — tests do not regenerate them automatically.

### Regenerating mocks

mockery is registered as a Go tool dependency (`tool
github.com/vektra/mockery/v2` in `go.mod`), so no separate install step is
needed beyond `go mod download`. After changing the signature of any
mocked interface, regenerate every mock from the module root:

```sh
go generate ./...
```

This invokes `go tool mockery`, which reads `.mockery.yaml` (interface
list, output package, and naming conventions) and rewrites the
`mock_*.go` files under `internal/*/mocks`. Commit the regenerated files
alongside your interface change.

## Project layout

See [`docs/architecture.md`](docs/architecture.md) for the full package
boundary rationale, protocol details, and threat model. At a glance:

- `cmd/vdhost`, `cmd/vdclient` — CLI entrypoints (Cobra commands, Viper
  config wiring, flag/env/config-file loading).
- `internal/cliconfig` — shared Viper precedence wiring used by both CLIs.
- `internal/protocol`, `internal/transport`, `internal/session` (via
  `internal/host`, `internal/client`) — wire protocol, TLS/auth, and
  session lifecycle.
- `internal/frame`, `internal/damage` — framebuffer/rectangle validation
  and changed-region detection.
- `internal/host/*`, `internal/client/*` — platform capture/input
  adapters and the client renderer/input layers.
- `internal/host/mocks`, `internal/client/mocks` — mockery-generated test
  doubles for the packages above (see [Testing](#testing)).
