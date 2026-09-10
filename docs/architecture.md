# Virtual Desktop MVP Architecture

Status: Approved implementation baseline for MVP

## 1. Objective

Build a Go virtual desktop with a deliberately narrow data plane:

- host to client: display metadata and visual pixel updates only;
- client to host: keyboard and pointer input only; and
- either direction: protocol control needed to establish, monitor, and close the session.

Audio, clipboard synchronization, file transfer, USB/device forwarding, arbitrary commands, shell access, application data, and bidirectional pixel streaming are out of scope. Control messages MUST NOT carry opaque application payloads that could bypass these exclusions.

## 2. Repository evidence and platform decision

The repository contains only an empty `README.md`; it establishes no platform, dependency, protocol, compatibility, or deployment convention.

MVP support is therefore explicitly limited to:

- host: Linux amd64, one local X11 desktop and one selected display;
- client: Linux amd64 desktop; and
- Go: the current stable Go release at implementation start, recorded in `go.mod`.

Linux/X11 is selected because one implementation can capture a selected display and inject keyboard/pointer events without first designing separate Windows, macOS, Wayland-portal, or browser adapters. The core protocol and session packages remain OS-independent so later platform adapters do not change the wire protocol.

Wayland, Windows, macOS, mobile, browser clients, headless virtual displays, multiple simultaneous displays, and architectures other than amd64 are not supported in the MVP. Linux arm64 may be enabled later after CI and the native capture/input dependencies are verified there; it is not implied by pure-Go protocol portability.

Assumption requiring confirmation: an X11-only host is acceptable for the first usable release. If Wayland is required, capture (PipeWire/XDG Desktop Portal) and input injection need a separate design and should not be hidden behind an implementation detail.

## 3. System shape and trust boundary

A host process owns capture, changed-region detection, encoding, session admission, and OS input injection. A client process owns rendering, viewport-to-host coordinate mapping, and local input capture. The host listens on a configured address and permits exactly one active client session in the MVP.

The network is untrusted. Capture and input adapters are also treated as validation boundaries: decoded network values are validated in the session layer before reaching the host injector, and captured buffers are validated before encoding.

The host MUST default to loopback-only listening. Non-loopback binding requires an explicit flag and configured authentication material. There is no discovery, relay, NAT traversal, account service, or unattended remote-management service in the MVP.

## 4. Transport, authentication, and encryption

### 4.1 Transport

Use one long-lived TCP connection protected by TLS 1.3. TCP gives ordered, reliable delivery for the first implementation and keeps authentication, framing, and shutdown simple. QUIC, WebRTC, UDP, and a browser-compatible transport are deferred until measurement demonstrates a need.

TLS requirements:

- minimum and maximum version are TLS 1.3 for the MVP;
- the host presents an operator-provisioned certificate;
|- the client validates the configured CA or an exact SHA-256 certificate fingerprint;
|- certificate verification is on by default and forbidden in production; an opt-in `-allow-insecure` client flag exists only for lab/isolated use against self-signed hosts and disables verification via `InsecureSkipVerify`;
|- private keys and tokens MUST NOT be logged; and
- handshake, authentication, reads, and writes use explicit deadlines and honor context cancellation.

After TLS is established, the client sends an `AUTH` message containing a version identifier and a 32-byte cryptographically random bearer token. The host compares it in constant time to its configured token. Authentication failure returns only a generic error and closes the connection. A token is transmitted only inside validated TLS, is stored with owner-only filesystem permissions when file-backed, and can be rotated by restarting the MVP host. Rate limiting is not a substitute for the loopback default or TLS.

Assumptions requiring confirmation: operators can provision or pin a certificate and distribute a 256-bit token out of band. Public-key client identity, multiple users, authorization roles, token rotation without restart, and revocation are deferred.

Tokenless mode: either side may run without an operator-supplied token. A host started without `-token` accepts any non-zero client token (no-authentication mode); a client started without `-token` mints a random non-zero bearer token and connects to such a host. The wire invariant forbidding a zero token is still enforced, so tokenless mode never weakens the wire — it simply omits a shared secret when neither side requires one. Host certificate trust is orthogonal to the token: a tokenless client still verifies the host certificate via `-ca`, `-fingerprint`, or the opt-in `-allow-insecure` flag.

### 4.2 Framing and limits

All protocol integers are unsigned big-endian unless explicitly signed. Each record has a fixed header:

| Field | Size | Meaning |
| --- | ---: | --- |
| magic | 4 | ASCII `VDP1` |
| version | 2 | protocol major, initially `1` |
| type | 2 | closed message-type enum |
| flags | 2 | type-specific, unknown bits rejected |
| reserved | 2 | zero |
| length | 4 | payload bytes |
| sequence | 8 | monotonically increasing per direction |

Readers consume the fixed header first, reject invalid magic/version/type/flags/sequence/length, and only then allocate a bounded payload. Maximum control or input payload is 64 KiB. Maximum pixel-update payload is configurable and hard-capped at 16 MiB. Display dimensions are independently capped at 8192 by 8192 and checked with overflow-safe arithmetic. Unknown message types, wrong-direction messages, malformed payloads, replayed/out-of-order sequence numbers, or limit violations terminate the connection with a generic protocol error when safe to send.

The protocol major is negotiated by `CLIENT_HELLO` and `SERVER_HELLO`; the only MVP-supported value is 1. No common version causes clean rejection. Minor-compatible features are represented by a known capability bitset; peers reject required unknown capabilities and ignore no fields implicitly.

## 5. Session lifecycle

The state machine is:

1. `Connected`: TCP accepted; TLS handshake must finish within 10 seconds.
2. `Authenticating`: client sends `AUTH` within 5 seconds; host admits it only if no session is active.
3. `Negotiating`: client sends `CLIENT_HELLO`; host responds with `SERVER_HELLO` and `DISPLAY_CONFIG`.
4. `Active`: host may send `FRAME`; client may send keyboard, pointer, wheel, `PING`, or `CLOSE`; host may send `PING`, `ERROR`, or `CLOSE`.
5. `Closing`: a peer sends `CLOSE`, stops new application messages, flushes at most one bounded write, and closes TLS/TCP.
6. `Closed`: capture, encoder, reader, writer, and input workers are canceled and joined.

Messages invalid for the current state are protocol errors. One read loop and one write loop own the connection; other goroutines communicate through bounded typed queues. Any fatal read, write, authentication, capture, injection, or context error cancels the whole session. Idle sessions exchange `PING`/`PONG`; no valid message for 30 seconds causes a ping, and no response within 10 seconds closes the session. Exact timeout values are configuration with these defaults and bounded minimums.

Disconnect MUST release all remotely pressed keys and pointer buttons to avoid stuck input. A new connection starts with a keyframe and no inherited input state. The host rejects a second authenticated connection as busy; it does not evict the active client.

## 6. Display model and pixel updates

### 6.1 Geometry and scaling

`DISPLAY_CONFIG` includes a monotonically increasing display generation, width, height, stride-independent pixel format, and optional physical width/height in millimetres. MVP capture has one display with origin `(0,0)`. Width and height are physical framebuffer pixels.

The client maintains a framebuffer at exactly the advertised host dimensions. It scales that framebuffer uniformly for presentation, preserves aspect ratio, and letterboxes unused viewport space. Scaling is a client rendering concern; wire rectangles and pointer coordinates always use host framebuffer pixels. Coordinates in letterbox margins are ignored. Client coordinates are mapped with clamping to `[0,width-1]` and `[0,height-1]` after accounting for the rendered offset and scale.

A host resolution change increments the display generation, sends a new `DISPLAY_CONFIG`, discards queued deltas, and follows with a full keyframe. Frames for a stale generation are rejected.

### 6.2 Pixel representation

Version 1 uses packed `BGRA8888`: four bytes per pixel in B, G, R, A order, row-major, top-to-bottom, with alpha fixed to `0xff`. Rectangle payload rows have no padding. This explicit format avoids host-native endianness and stride assumptions.

A `FRAME` contains:

- display generation (`uint64`);
- frame sequence (`uint64`);
- base frame sequence (`uint64`, zero for a keyframe);
- keyframe flag;
- rectangle count (`uint16`, maximum 256); and
- for each rectangle: `x`, `y`, `width`, `height`, encoding, encoded length, and bytes.

Rectangles MUST be non-empty, within the advertised display, non-overlapping within a frame, and collectively bounded by the message and decoded-size limits. Encodings are `RAW_BGRA` and `ZLIB_BGRA`; zlib output must expand to exactly `width * height * 4` bytes and is decompressed through a hard byte limit. The encoder chooses zlib only when smaller than raw. A client applies a frame atomically only after all rectangles validate and decode. A delta is accepted only when its `base frame sequence` equals the client's last committed frame; otherwise the client requests a keyframe with `KEYFRAME_REQUEST` and does not partially render the delta.

### 6.3 Changed-region strategy

The host partitions the capture into 64 by 64 pixel tiles (edge tiles may be smaller), computes a fast content hash, and byte-compares tiles whose hash changed before declaring them dirty. Adjacent dirty tiles on the same tile rows are coalesced, then vertically merged only when their horizontal spans match. If coalescing would exceed 256 rectangles, or dirty pixels exceed 60% of the display, send one full-screen keyframe. Send a periodic keyframe at least every 10 seconds to bound recovery and hash-collision impact.

Correctness does not depend solely on the non-cryptographic hash: candidate changes are confirmed by byte comparison, and periodic keyframes repair any lost logical synchronization.

### 6.4 Backpressure and frame pacing

Capture is sampled at a configurable target capped at 30 frames per second for the MVP. There is one pending-frame slot between capture/encoding and the network writer, with a total encoded update cap of 16 MiB. The writer never blocks capture while holding capture resources.

If a newer update arrives while an unsent delta is pending, replace the pending delta with a newly generated keyframe representing the latest complete framebuffer. Never send a delta whose base was dropped. After any write timeout, queue overflow that cannot be represented by the one-slot latest state, or resolution change, discard queued deltas and force the next transmitted visual update to be a keyframe. Control shutdown/error messages use a separate small bounded queue but cannot grow without limit or indefinitely starve visual updates.

The client rendering path similarly keeps at most the current committed framebuffer and one fully decoded candidate. It may skip presentation callbacks but MUST process protocol frame ancestry correctly.

## 7. Input semantics

Only the active, authenticated session may inject input. Input is disabled unless the host starts with an explicit enable-input flag. All input messages carry the current display generation and an input sequence number.

### 7.1 Keyboard

`KEY` carries USB HID Usage Tables keyboard usage page `0x07`, a 16-bit usage ID, action `DOWN` or `UP`, and an 8-bit modifier bitmap. It represents physical keys, not text, Unicode, commands, or arbitrary HID reports. Unknown/reserved usages are rejected. Auto-repeat is represented as repeated `DOWN` events and may be coalesced only if ordering relative to other key events is preserved.

The host tracks pressed usages per session, suppresses duplicate `UP`, bounds the pressed-key set, and releases all tracked keys on focus loss reported by the client, disconnect, or session failure. Client-side text composition and IME strings are not sent in version 1.

### 7.2 Pointer

`POINTER_MOVE` carries absolute host-pixel `x` and `y`; moves outside the current generation are rejected rather than passed to the injector. The client may coalesce unsent move events to the most recent position, but it MUST NOT reorder a move across a button or wheel event.

`POINTER_BUTTON` carries one of left, middle, right, back, or forward plus `DOWN`/`UP`. The host tracks and releases pressed buttons on disconnect. No arbitrary button numbers or opaque HID reports are accepted.

`POINTER_WHEEL` carries signed 16-bit horizontal and vertical wheel deltas in multiples of 120 units per detent. Trackpads may accumulate fractional local movement until a non-zero wire delta is available. Values are range-checked and rate-limited before injection.

Input uses a bounded client send queue. Pointer moves may be replaced by a newer move. Key, button, and wheel transitions are never silently dropped; if their queue is full or a write deadline expires, the client closes the session so host cleanup releases state.

## 8. Protocol direction matrix

| Message | Client to host | Host to client |
| --- | :---: | :---: |
| `AUTH`, `CLIENT_HELLO` | yes | no |
| `SERVER_HELLO`, `DISPLAY_CONFIG`, `FRAME` | no | yes |
| `KEYFRAME_REQUEST` | yes | no |
| `KEY`, `POINTER_MOVE`, `POINTER_BUTTON`, `POINTER_WHEEL`, `FOCUS_LOST` | yes | no |
| `PING`, `PONG`, `CLOSE`, bounded `ERROR` | yes | yes |

The decoder/session state machine enforces this matrix before dispatch. `ERROR` contains only an enum and bounded diagnostic text; it is not a general-purpose data channel.

## 9. Go package and command boundaries

Suggested module layout:

- `cmd/vdhost`: Cobra command, flags/config, certificate and token loading, host assembly and signal handling.
- `cmd/vdclient`: Cobra command, flags/config, trust pin loading, client UI assembly and signal handling.
- `internal/cliconfig`: shared Viper wiring used by both commands, giving every configuration value flag > environment variable (`VDHOST_*`/`VDCLIENT_*`) > YAML config file > default precedence. See the top-level README for the full precedence rules and example config files.
- `internal/protocol`: constants, typed messages, framing, version negotiation, limits, direction/state validation; no OS or UI imports.
- `internal/transport`: TLS configuration, dialing/listening, deadlines, connection read/write ownership, authentication.
- `internal/session`: lifecycle state machine, cancellation, heartbeats, queues, host/client orchestration.
- `internal/frame`: framebuffer model, rectangles, BGRA validation, zlib codec, delta application.
- `internal/damage`: tile comparison, dirty-region coalescing, keyframe policy; pure and deterministic.
- `internal/host/capture`: platform-neutral capture interface and immutable frame result.
- `internal/host/capture/x11`: Linux X11 capture adapter.
- `internal/host/input`: validated injector interface and session pressed-state cleanup.
- `internal/host/input/x11`: Linux X11 keyboard/pointer adapter.
- `internal/client/render`: renderer interface and viewport/host geometry mapping.
- `internal/client/input`: UI event normalization to the restricted protocol types.

Native libraries and UI toolkit selection are implementation decisions only if they satisfy Linux amd64, cancellation, threading, licensing, and test-fake requirements. Protocol packages MUST NOT import native capture, injection, or UI packages. Interfaces belong with their consumers; adapters implement them. Commands contain wiring, not protocol logic.

Configuration is explicit flags, environment variables, or a local YAML config file, resolved via `internal/cliconfig` (Viper) with flag > environment variable > config file > default precedence. Environment variables may point to secret files but raw secret values should not be exposed in process listings. Logs use session-local random IDs and metadata, never pixel contents, key events, tokens, or certificate private data.

## 10. Implementation phases and dependencies

1. Protocol foundation: create the module; implement typed framing, limits, direction/state validation, TLS/authentication, lifecycle cancellation, and fake-connection tests. This is the critical dependency for all other work.
2. Frame pipeline: implement framebuffer/rectangle validation, raw/zlib codecs, delta application, tile damage detection, coalescing, and backpressure tests using generated images.
3. Host vertical slice: implement X11 capture behind the interface, single-session server, frame pacing, resolution-change handling, and a recording input-injector fake. Initially keep real input disabled.
4. Client vertical slice: implement renderer/input interfaces, viewport mapping, frame ancestry/keyframe request behavior, and a loopback client using fake UI adapters.
5. Input enablement: implement the X11 injector, pressed-state cleanup, explicit enable-input control, and negative/rate-limit tests.
6. End-to-end hardening: run host and client over loopback TLS; test keyframe plus delta rendering and recorded input; fuzz decoders; run race detection, static analysis, and disconnect/slow-peer tests.
7. Linux packaging: document certificate/token bootstrap, X11 permissions, loopback default, diagnostics, supported limits, and exclusions; build Linux amd64 artifacts.

Phases 2 and TLS/server scaffolding within phase 3 may proceed in parallel after protocol message shapes are fixed. Host and client adapter work may proceed in parallel after the protocol, framebuffer contracts, and fake interfaces land. Real input remains gated until lifecycle cleanup and authentication tests pass.

## 11. Testable acceptance criteria

The MVP is acceptable when all of the following are demonstrated by automated tests unless marked manual:

1. A Linux amd64 host and client build from a clean checkout using documented commands.
2. A TLS 1.3 loopback session succeeds only with a trusted/pinned host certificate and the correct 32-byte token; wrong token, wrong pin, plaintext, timeout, and second-client attempts fail closed.
3. Version negotiation accepts version 1 and rejects no-overlap, malformed, oversized, unknown-type, invalid-state, invalid-sequence, and wrong-direction records without unbounded allocation.
4. A synthetic framebuffer keyframe and at least two changed rectangles arrive byte-for-byte in BGRA order and commit atomically; corrupt zlib, overlap, out-of-bounds geometry, stale display generation, and wrong delta base are rejected.
5. Damage tests prove unchanged frames produce no update, changed edge tiles are detected, valid runs coalesce, rectangle/60% thresholds force a keyframe, and periodic keyframes occur.
6. A slow-peer test proves memory remains within configured queue/message bounds, stale deltas are not transmitted after drops, and the recovered image equals the latest host frame.
7. Geometry tests cover letterboxing, scale-up/down, edge coordinates, margin rejection, and a resolution change followed by a keyframe.
8. A recording injector receives ordered keyboard usage, pointer position/button, and wheel events only from an authenticated client; server-origin input and client-origin frames are rejected.
9. Disconnect, focus loss, malformed input, and write timeout release every tracked key/button and terminate all session goroutines; `go test -race ./...` reports no race.
10. Decoder fuzz targets run for framing, frame rectangles, zlib expansion, and every input payload without panic or excessive allocation.
11. Manual X11 smoke test shows the selected display updating at usable interactive latency at 1080p/30 fps on a LAN and confirms keyboard and five supported pointer buttons after explicit input enablement.
12. Documentation states the supported platform, provisioning steps, loopback default, input security warning, limits, and every excluded channel. A packet/message audit finds no audio, clipboard, file-transfer, generic HID-report, or command-execution payload.

Repository-native verification commands should become:

- `gofmt -w` on changed Go files and a clean formatting check in CI;
- `go vet ./...`;
- `go test ./...` (unit tests use testify `assert`/`require` and mockery-generated mocks under `internal/*/mocks`; regenerate mocks with `go generate ./...` after an interface change);
- `go test -race ./...`;
- `./scripts/build.sh` (or `make build-all`) to cross-compile every release binary for all supported OS/arch pairs with the correct platform extension; and
- protocol fuzz smoke runs with a fixed CI time budget.

## 12. Risks and mitigations

- X11 capture or injection permissions vary by desktop: fail with actionable diagnostics, never fall back to shell commands, and keep adapters replaceable.
- Raw desktop bandwidth can exceed LAN capacity: changed tiles, zlib-if-smaller, 30 fps cap, latest-state backpressure, and keyframes bound behavior; video codecs are post-MVP.
- TCP head-of-line blocking can raise latency: strict write deadlines and latest-state replacement prevent unbounded lag; QUIC/WebRTC requires a measured follow-up.
- Remote input is security-sensitive: loopback default, TLS pinning, random token, one session, explicit input opt-in, type/range/rate validation, and release-on-disconnect reduce exposure.
- Pixel data and key events are sensitive even without excluded channels: do not log payloads and document the trust boundary.
- Compression bombs and geometry overflow: validate dimensions and encoded/decoded lengths before allocation and use limited readers.
- Layout differences can make physical HID usages produce unexpected symbols: document that keyboard layout is controlled by the host OS; text/IME transfer is excluded.
- Native/toolkit dependencies may constrain static builds or licensing: record chosen dependencies and licenses before phase 3/4 implementation.

## 13. Explicit non-goals

The MVP will not provide audio, microphone, clipboard, file transfer, printing, camera, smart-card, generic USB/HID passthrough, shell/command execution, remote process management, drive mapping, browser access, session recording, multi-user access, multi-monitor composition, relay/NAT traversal, discovery, account management, or privilege escalation. Adding any such channel requires a new threat model, protocol version/capability, architecture review, and explicit scope approval.
