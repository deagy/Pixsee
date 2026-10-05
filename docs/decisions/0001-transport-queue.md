# ADR-001 — Transport queue backpressure model

- **Status:** Accepted (2026-10-05, R4)
- **Deciders:** self (draft for review)
- **Related:** R3 rate-overrun degrade (`internal/session`), AC-10

## Context

The transport layer moves protocol messages from the reader goroutine to the
session writer through a bounded queue. Under sustained overload — the capture
loop produces frames faster than the stream can send them — the queue has to
decide what happens to the excess: **block the reader, drop silently, or drop
with a signal.** This is the decision that shapes backpressure for the whole
session, so it is worth stating explicitly rather than leaving it implicit in
`internal/transport/queue.go`.

## Decision

A bounded in-memory channel with a **non-blocking** `TrySend` that returns
`ErrBackpressure` when full, plus `Receive()` and `Close()`:

```go
func NewQueue[T any](capacity int) (*Queue[T], error)   // capacity must be > 0
func (q *Queue[T]) TrySend(value T) error               // ErrBackpressure when full
func (q *Queue[T]) Receive() <-chan T
func (q *Queue[T]) Close()
```

Sustained overload drops the newest frame and **signals** the caller via
`ErrBackpressure`; it never blocks the reader goroutine. R3 (rate-overrun
degrade) consumes that signal: a counted burst of drops becomes a
`KEYFRAME_REQUEST`, replacing stale deltas with a keyframe of the latest
complete framebuffer.

## Consequences

- **+ Never ships a stale delta.** A dropped delta is superseded by a
  keyframe of the most recent complete framebuffer, matching the pixel model
  ("a delta with a mismatched base triggers KEYFRAME_REQUEST, never partial
  render").
- **+ No goroutine stalls and no unbounded memory under overload.** The reader
  keeps flowing; excess work is shed, not queued.
- **+ Cheap and obvious.** One mutex, one channel, one error.
- **− Non-blocking drop means backpressure is signalled, not applied
  synchronously.** The reader does not wait for the writer. This is
  intentional for a video stream — a fresh keyframe is always better than a
  late delta — but it means "drop" is a correctness property of the caller
  (must turn a drop into a keyframe), not of the queue alone.
- **− Capacity is fixed at construction (minimum 1); no dynamic resizing.**
  Adequate for the single pending-frame slot between encoder and writer. A
  larger or adaptive buffer would be a separate decision.

## Alternatives considered

- **Blocking `Send` with a full-channel wait.** Simpler API, but stalls the
  reader goroutine under sustained overload and risks head-of-line blocking
  across the one-session-per-host invariant. Rejected.
- **Unbounded queue.** Bounded memory guaranteed, but an attacker or a slow
  client could exhaust host memory. Rejected (matches the fixed-budget framing
  elsewhere in the protocol).
- **Drop-oldest (evict queued frames).** More complex; for a live video stream
  the newest frame is always the most useful, so drop-newest is the right
  default.
