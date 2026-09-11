# Changelog

All notable changes to this project are documented in this file.

The format is loosely based on [Keep a Changelog](https://keepachangelog.com/),
adapted for this project's protocol-version-based identification (see
`docs/architecture.md` — there is no semantic-version string in the code
itself; releases are tagged in git).

## Unreleased

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
  (linux/darwin/windows × amd64/arm64), naming each artifact
  `pixsee_<client|host|e2e|captest|rt>_<os>_<arch>[.exe]` (`.exe` on
  Windows, no extension on Linux/macOS; e.g. `pixsee_host_linux_amd64`,
  `pixsee_client_windows_arm64.exe`) and writing a `dist/SHA256SUMS`
  checksum file. `make build` builds host-platform-only binaries for
  local development, using the same naming scheme.

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
