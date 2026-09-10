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
