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
supported OS/arch pair and names each artifact
`pixsee_<client|host|e2e|captest|rt>_<os>_<arch>[.exe]`, matching the
platform-appropriate extension:

| OS      | Arch  | Extension | Example (`vdhost`)              | Example (`vdclient`)              |
| ------- | ----- | --------- | -------------------------------- | ----------------------------------- |
| linux   | amd64 | none      | `pixsee_host_linux_amd64`        | `pixsee_client_linux_amd64`        |
| linux   | arm64 | none      | `pixsee_host_linux_arm64`        | `pixsee_client_linux_arm64`        |
| darwin  | amd64 | none      | `pixsee_host_darwin_amd64`       | `pixsee_client_darwin_amd64`       |
| darwin  | arm64 | none      | `pixsee_host_darwin_arm64`       | `pixsee_client_darwin_arm64`       |
| windows | amd64 | `.exe`    | `pixsee_host_windows_amd64.exe`  | `pixsee_client_windows_amd64.exe`  |
| windows | arm64 | `.exe`    | `pixsee_host_windows_arm64.exe`  | `pixsee_client_windows_arm64.exe`  |

The `e2e`, `captest`, and `rt` dev/test tools follow the same
`pixsee_<name>_<os>_<arch>[.exe]` scheme (e.g. `pixsee_e2e_linux_amd64`,
`pixsee_captest_windows_arm64.exe`, `pixsee_rt_darwin_arm64`).

```sh
make build-all
# or
./scripts/build.sh
```

Artifacts land in `dist/`, built with `CGO_ENABLED=0`. `make checksums`
(or the build script itself) writes a `dist/SHA256SUMS` file alongside
them. `make clean` removes `dist/`.

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
implications. The same kind of loud stderr warning is printed when
`--allow-insecure` is set, since it disables host certificate verification
and must never be used against an untrusted host on a real network.

`vdclient` accepts a `--connect-timeout` (default `0` = no limit) that
bounds the whole initial connect sequence — dial, TLS handshake,
authentication, and `CLIENT_HELLO`/`SERVER_HELLO` negotiation — across
reconnect attempts. When it is exhausted the client returns an error
instead of reconnecting forever against a dead host.

`vdhost` admits exactly one active client session at a time. A second
client that authenticates is rejected with a busy protocol error and
closed, rather than being served a stale or interleaved view of the
host's display.

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

## Troubleshooting

- **`config: While parsing config: yaml: control characters are not
  allowed`** — the YAML config file contains characters the YAML parser
  refuses: NUL/C0 control bytes, DEL, the C1 range U+0080–U+009F,
  surrogates or U+FFFE/U+FFFF. On Windows (including ARM64) this comes
  from tools that re-encode text: PowerShell redirection (`>`) or
  `Set-Content` without `-Encoding utf8` produce UTF-16, Notepad's legacy
  "Unicode"/"ANSI" options, and Latin-1 mis-decodes (PowerShell 5.1
  `Invoke-WebRequest`, some editors) that turn a multi-byte character
  such as an em-dash into C1 controls. `internal/cliconfig` transcodes
  UTF-16/UTF-8-BOM files and strips every character the parser refuses
  before parsing, so current builds load such files. If a file still
  fails, the error names the file that was loaded and the first
  offending character with its line, column and byte offset; inspect
  that spot with `Format-Hex <file>` (PowerShell) or `xxd` and re-save
  the file as plain UTF-8. This never affected flags or environment
  variables, only YAML config files.

- **No client window appears, and the client repeatedly logs
  `context deadline exceeded` while reconnecting** — this was caused by
  `vdhost` never sending `SERVER_HELLO` after accepting a connection,
  so the client sat in `Negotiating` until its wait timed out and it
  retried forever with no display ever shown. It reproduced on every
  platform, but was previously masked on Windows ARM64 by the YAML
  parsing crash above (the client never got far enough to hit it). Fixed
  as of this release — `vdhost` now completes the `CLIENT_HELLO`/
  `SERVER_HELLO` negotiation before sending `DISPLAY_CONFIG`. If you see
  this with a current build, check that the host and client binaries are
  both from the same release (an old `vdhost` paired with a new
  `vdclient`, or vice versa, can still exhibit protocol mismatches).

- **A second `vdclient` connects but sees no display (or the client
  exits with a busy error)** — `vdhost` admits exactly one active client
  session at a time. A second client that authenticates is rejected with
  a `busy` protocol error and closed before it can receive any frames.
  This is normal: only one client can view the host's display. Connect a
  different client, or close the first one before connecting another.

## Testing

Run the full test suite:

```sh
go test ./...
# or
make test
```

`go vet ./...` (or `make vet`) is also part of the standard pre-commit
check.

Continuous integration runs the same gates on every push and pull request in
[`.github/workflows/ci.yml`](.github/workflows/ci.yml): a `gofmt` formatting
check, `go vet`, unit tests split so the RSA-2048 client handshake tests do not
hold up the rest of the suite, a `-race` gate on the fast packages, the
integration suite, a fixed-budget protocol fuzz smoke run, and a
cross-compile of every release binary.

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
