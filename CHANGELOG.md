# Changelog

All notable changes to this project are documented in this file.

The format is loosely based on [Keep a Changelog](https://keepachangelog.com/),
adapted for this project's protocol-version-based identification (see
`docs/architecture.md` — there is no semantic-version string in the code
itself; releases are tagged in git).

## Unreleased

## v1.4.0 - 2026-10-06

### Added

- **GUI-enabled Windows client artifact.** A new, distinct windowed client,
  `pixsee_client_gui_windows_amd64.exe`, is built with the `fyne` build tag
  and `CGO_ENABLED=1` via `make build-gui-windows` /
  `scripts/build-gui-windows.sh`. It is linked as a Windows GUI-subsystem
  executable (`-H=windowsgui`, so double-clicking it opens the Fyne window with
  no extra console) with the MinGW runtime statically linked
  (`-extldflags=-static`), and the build runs a post-build import-table gate
  that fails if the artifact still imports `libwinpthread-1.dll`,
  `libgcc_s_seh-1.dll`, or `libstdc++-6.dll`. The standard cross-platform matrix (`scripts/build.sh`) is
  unchanged and remains headless (`CGO_ENABLED=0`, no window, console
  diagnostics); the GUI artifact is produced only for Windows/amd64, needs a C
  compiler (MinGW-w64 `gcc`) and `objdump` on `PATH`, and is uploaded as a
  downloadable GitHub Actions artifact from a native `windows-latest` runner
  rather than published or released. That native `windows-latest` job
  (`gui-windows`) built the artifact and uploaded it successfully on CI run
  [37566223183](https://github.com/deagy/pixsee/actions/runs/37566223183), so
  the Fyne+CGO build path is verified end to end; the workflow itself does not
  create or publish a GitHub release, and a manual tagged release can attach
  the uploaded artifact. See `docs/architecture.md` §2.3.

### Changed

- **Input rate overrun degrades instead of disconnecting** (F7 / AC-10, owner
  decision Q5). A client event burst above `-max-input-per-sec` now sheds the
  excess events — counted via `host.Service.InputDropped()` and logged as one
  line per drop burst — and the session stays Active streaming frames.
  Previously any overrun returned `ErrInputRate` and terminated the whole
  session, so a fast polling mouse could kill it; the exported
  `host.ErrInputRate` is gone with that behavior. Termination remains
  reserved for protocol violations.
- **The host now fails closed without authentication material** (F4 / AC-6,
  owner decision Q2; supersedes the warning-only tokenless mode of v1.2.2).
  `vdhost` refuses to start with a zero token: a non-loopback bind always
  requires `--token`, and a loopback bind (`127.0.0.0/8`, `::1`, `localhost`)
  requires the new explicit `--no-auth` opt-in and still prints a loud stderr
  warning naming the flag. `--no-auth` does not unlock non-loopback binds.
  Startup refusal exits non-zero with an error naming the bound address and
  the fix.
- **Token files accessible beyond their owner are refused.** A `--token`
  path with any group/other permission (`mode & 0o077 != 0`) aborts startup
  with a `chmod 0600` instruction (Windows relies on ACLs and is exempt).
- **Ephemeral certificate fingerprints are published at startup** (F6 / AC-7,
  owner decision Q3). Started without `--ca`/`--key`, `vdhost` prints the
  self-signed leaf certificate's SHA-256 fingerprint and the matching
  client-side `--fingerprint` usage. Validity stays 1 h; TOFU pinning is an
  explicit follow-on.
- **The heartbeat watchdog gives up only past an unanswered probe** (D7).
  Give-up timing is measured from the last PING whose PONG never arrived
  rather than elapsed-since-last-activity on a phase-aligned tick, so
  scheduling jitter cannot reap a session whose keepalive is being answered.
- **`host.Service.Run` requires peers to implement
  `SetSteadyReadTimeout`** (verifier follow-on N1): a peer without the method
  is now a startup error instead of silently skipping the steady-deadline
  arming, so a future wrapper cannot regress F2 unnoticed.
- **Client per-connection state and symmetric keepalive** (F1 / F2 / F3,
  AC-1..AC-3, R1). A reconnecting `client.Session` now resets its framebuffer
  and re-arms its input state per connection, and both sides derive their
  steady-state read deadlines from the heartbeat config and send unsolicited
  PINGs after half the heartbeat interval of idleness. `client.Config` gained
  `HeartbeatInterval`/`HeartbeatTimeout` (default 30 s, symmetric with the
  host's flags).

### Fixed

- **`fyne`-tagged client build was broken.** The headless renderer host
  (`cmd/vdclient/renderer_host.go`) lacked a `//go:build !fyne` constraint, so
  enabling the `fyne` tag compiled both renderer hosts and failed with
  duplicate `rendererHost`/`newRendererHost` declarations; the shared
  `connectionStateText` helper was also defined only in the headless file,
  leaving the tagged build with an undefined symbol. The headless host is now
  excluded under `fyne`, and the helper is defined once in the untagged
  `observer.go`. The headless build compiles and is covered by the existing
  build/test matrix; the `fyne`-tagged GUI build compiles and is built
  successfully by the native `windows-latest` CI job (`gui-windows`) on run
  37566223183. No runtime behavior changed.

## v1.3.2 - 2026-09-18

### Fixed

- **Windows ARM64 config parsing crash, second round.** The v1.3.1 fix
  only handled UTF-16/BOM files, but `pixsee_host_windows_arm64.exe`
  from v1.3.1 still failed on Windows 11 ARM64 with
  `config: While parsing config: yaml: control characters are not
  allowed`. The YAML decoder also refuses DEL..U+0084, the C1 controls
  U+0086..U+009F, UTF-16 surrogates and U+FFFE/U+FFFF; those are valid
  UTF-8, so the old byte-level sanitizer left such files untouched and
  never retried. The concrete trigger was the em-dash in the shipped
  example configs after a Latin-1 mis-decode (e.g. PowerShell 5.1
  `Invoke-WebRequest`, or an editor treating the file as "ANSI").
  `internal/cliconfig` now sanitizes rune by rune against the decoder's
  exact allowed set and drops invalid UTF-8 bytes, the example configs
  are ASCII-only, and a config error now names the file that was
  actually loaded (including when found via the default search) plus
  the first disallowed character, its line/column and byte offset.
  Covered by `internal/cliconfig/cliconfig_windows_c1_encoding_test.go`.
- **Second client leaked a goroutine and a listener slot.** `vdhost`
  accepted every authenticated connection, so a second client
  authenticated, blocked forever reading `CLIENT_HELLO`, and never
  released its resources. `vdhost` now admits exactly one active client
  session: a second authenticated connection is rejected with
  `ERROR_BUSY` and closed before entering the service loop.
- **No keyframe recovery or heartbeat.** The host ignored the client's
  `KEYFRAME_REQUEST`, so a client that could not apply a delta ended its
  session instead of resynchronizing, and neither side exchanged
  `PING`/`PONG` to detect a dead peer. `internal/host` now handles
  `KEYFRAME_REQUEST` by forcing the next visual update to be a
  keyframe, sends `PING` after idle, and answers incoming `PING` with
  `PONG`; an unhandled control message no longer silently ends the
  session.
- **Client could hang forever against a dead host.** `vdclient` gained a
  `--connect-timeout` (default `0` = no limit) that bounds the whole
  initial connect sequence — dial, TLS handshake, authentication, and
  `CLIENT_HELLO`/`SERVER_HELLO` negotiation — across reconnect attempts.
  When it is exhausted the client returns an error instead of
  reconnecting forever. The initial dial now runs against the
  timeout-bound context (`internal/client/session.go`), so the deadline
  actually aborts a black-hole/firewall-dropped-port dial instead of
  waiting indefinitely; a timeout during the first attempt returns the
  distinct `client: connect timeout` error immediately rather than
  falling through to the reconnect delay.
- **Heartbeat feature shipped inert.** `internal/host` handled
  `PING`/`PONG` and sent idle probes, but `vdhost` never exposed a way to
  set the cadence — the 30s defaults were applied silently inside
  `NewService`. `vdhost` now accepts `-heartbeat-interval` and
  `-heartbeat-timeout` (default `30s` each) and passes them to the
  service so the dead-peer detection is actually configurable.
- **No startup warning for `-allow-insecure`.** `vdclient` now prints a
  loud stderr warning when host certificate verification is disabled,
  mirroring the tokenless-mode warning.
- **Wheel-delta bound mismatch between protocol validation and host
  injection.** `internal/protocol` and `internal/host/input` now agree on
  the maximum wheel delta; the bound is asserted by
  `TestWheelDeltaBound` so validation and injection cannot drift apart.

### Docs

- README documents the single-session guard, the `-allow-insecure`
  startup warning, `--connect-timeout`, the new `vdhost` heartbeat flags,
  and a troubleshooting entry for the busy error; CHANGELOG records the
  second-round Windows ARM64 config fix and the host/client hardening
  fixes above.

## v1.3.1 - 2026-09-11

### Changed

- **Build artifact naming.** Cross-platform release binaries produced by
  `scripts/build.sh` and `make build`/`make build-all` are now named
  `pixsee_<client|host|e2e|captest|rt>_<os>_<arch>[.exe]` (`.exe` on
  Windows, no extension on Linux/macOS; e.g. `pixsee_host_linux_amd64`,
  `pixsee_client_windows_arm64.exe`), replacing the plain
  `<binary>_<os>_<arch>[.exe]` scheme shipped in v1.3.0.

### Fixed

- **Windows ARM64 config parsing crash.** `vdhost`/`vdclient` failed to
  start on Windows (including ARM64) with
  `config: While parsing config: yaml: control characters are not
  allowed` when the YAML config file was written by a tool that emits
  UTF-16 or a UTF-8 byte-order mark (PowerShell redirection/`Set-Content`
  without `-Encoding utf8`, Notepad's legacy "Unicode" save option, or
  similar). `internal/cliconfig` now detects and transcodes UTF-16
  (with or without BOM) and UTF-8-BOM config files, and strips stray
  disallowed control bytes, before handing the bytes to the YAML parser.
  A clean UTF-8 file is left byte-for-byte unchanged. Covered by
  `internal/cliconfig/cliconfig_windows_encoding_test.go`.
- **Missing GUI and continuous error spam connecting to a host.**
  `vdhost` accepted a client's TLS handshake and token but then invoked
  the session service directly without exchanging `CLIENT_HELLO`/
  `SERVER_HELLO` first. The client's session state machine (`Negotiating`)
  rejects any `DISPLAY_CONFIG` sent before that exchange, so every
  connection was torn down immediately after authentication; the client
  never saw a display and looped forever on `context deadline exceeded`
  reconnect attempts with no window and continuous stderr error spam.
  This affected every platform, but was only observed after the Windows
  ARM64 config-parsing crash above was fixed and a host/client on that
  platform could actually reach the connect step. `vdhost` now completes
  the `CLIENT_HELLO`/`SERVER_HELLO` negotiation before sending
  `DISPLAY_CONFIG`, matching what the client already expects. Covered by
  a new regression test in `cmd/vdhost/handle_connection_test.go`.

### Docs

- README updated with the full per-platform binary naming example table
  and a note on the Windows ARM64 / GUI fixes above.

## v1.3.0 - 2026-09-10

### Added

- **Cobra-based CLI.** `vdhost` and `vdclient` are now built on
  [spf13/cobra](https://github.com/spf13/cobra). Both commands gained
  Cobra-generated `--help` output describing every flag, default, and
  description. Single-dash long flags (e.g. `-addr`) continue to work
  alongside the standard double-dash form (`--addr`) for backward
  compatibility. The pre-Cobra exit-code convention (2 for flag/config
  errors, 1 for runtime errors) is unchanged.
- **Viper-based configuration.** A new shared `internal/cliconfig`
  package wires up [spf13/viper](https://github.com/spf13/viper) for both
  commands, giving every value flag > environment variable
  (`VDHOST_<FLAG_NAME>` / `VDCLIENT_<FLAG_NAME>`) > YAML config file
  (`--config`, or auto-discovered `vdhost.yaml`/`vdclient.yaml` in `.`,
  `$HOME/.config/virtualdesktop`, or `/etc/virtualdesktop`) > built-in
  default precedence. Example, fully-commented config files are provided
  at `configs/vdhost.example.yaml` and `configs/vdclient.example.yaml`.
- **testify + mockery test infrastructure.** Unit tests now use
  [testify](https://github.com/stretchr/testify) `assert`/`require`, and
  [mockery](https://github.com/vektra/mockery)-generated mocks for the
  core `host.Capture`, `host.Input`, `host.Peer`, `client.Renderer`,
  `client.StateObserver`, and `client.InputSender` interfaces. mockery is
  registered as a `go.mod` tool dependency; regenerate mocks with
  `go generate ./...` after changing a mocked interface's signature.
- **Cross-platform release build.** `scripts/build.sh` and a new
  `Makefile` (`make build-all`) cross-compile `vdhost`, `vdclient`,
  `e2e`, `captest`, and `rt` for all six supported OS/arch pairs
  (linux/darwin/windows × amd64/arm64), naming each artifact with the
  correct platform extension (`.exe` on Windows, none on Linux/macOS) and
  writing a `dist/SHA256SUMS` checksum file. `make build` builds
  host-platform-only binaries for local development.

### Changed

- README rewritten to document the new CLI, configuration, testing, and
  build workflows described above.

### Unchanged

- Wire protocol, message types, and validation (`internal/protocol`).
- TLS 1.3 transport, token authentication (including tokenless mode), and
  session state machine (`internal/transport`, `internal/session`).
- All existing `vdhost`/`vdclient` flag names, defaults, and runtime
  behavior — the CLI/config migration is additive and preserves the
  pre-Cobra interface.

## v1.2.2 - Tokenless mode

- `vdclient` can now run without a `-token` flag: it mints a random
  non-zero bearer token itself when one isn't supplied, instead of
  requiring an operator-provided token.

## v1.2.1 - Optional token & CA

- Host creation no longer requires a token or CA to be supplied up front;
  both are now optional.

## v1.2.0 - Allow-insecure flag

- Added an `-allow-insecure` flag to `vdclient` for connecting to hosts
  without full TLS certificate verification.

## v1.1.0 - Cross-platform host

- Added Windows and macOS support to the host, alongside the existing
  Linux support.

## v1.0.0-protocol-v1 - Protocol v1 / MVP

- Initial virtual desktop protocol (v1) and MVP host/client
  implementation.
