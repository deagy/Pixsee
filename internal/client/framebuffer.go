package client

import (
	"bytes"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"sync"

	"virtualdesktop/internal/protocol"
)

var (
	ErrDisplayNotConfigured = errors.New("client: display not configured")
	ErrKeyframeRequired     = errors.New("client: keyframe required")
)

type Snapshot struct {
	Generation, FrameSequence uint64
	Width, Height             uint32
	Pixels                    []byte
}

// rectMeta is the bounded geometry of one rectangle within a logical frame the
// reassembler is tracking — either already applied to the in-progress staging
// buffer, or consumed while draining a rejected sequence's tail. It
// deliberately carries no pixels: geometry is retained only so cross-part
// overlaps can be detected without accumulating encoded part bodies.
type rectMeta struct{ x, y, width, height uint32 }

// overlaps reports whether a candidate rectangle overlaps this one. Both are
// already validated in-bounds and within MaxDimension, so the sums cannot
// overflow uint32.
func (r rectMeta) overlaps(o protocol.Rectangle) bool {
	return o.X < r.x+r.width && r.x < o.X+o.Width && o.Y < r.y+r.height && r.y < o.Y+o.Height
}

// frameStaging is the single in-progress FRAME_PART sequence. At most one is
// live at a time; it is swapped into the visible framebuffer on commit and
// dropped on abort. pixels is always a distinct allocation from the visible
// framebuffer, so an intermediate part can never mutate committed pixels.
type frameStaging struct {
	generation, frameSequence, baseFrameSequence uint64
	keyframe                                     bool
	partCount                                    uint16
	nextPartIndex                                uint16
	rectCount                                    int
	covered                                      uint64
	rectangles                                   []rectMeta
	pixels                                       []byte
}

// frameDiscard drains the tail of a rejected delta FRAME_PART sequence. When
// part 0 of a delta cannot be applied (unknown base, or a keyframe is
// required), the host is already mid-transmission and will send the remaining
// ordered parts of that same logical frame. Tracking its header and next index
// lets the client consume and validate that tail without touching pixels, so
// the stream stays framed and the host's forced keyframe is accepted rather
// than the whole session being torn down. It mirrors frameStaging's bounded
// geometry (no pixel buffer) so malformed, out-of-order, or overlapping tail
// parts stay fatal, but nothing is ever decoded or blitted.
type frameDiscard struct {
	generation, frameSequence, baseFrameSequence uint64
	keyframe                                     bool
	partCount                                    uint16
	nextPartIndex                                uint16
	rectCount                                    int
	rectangles                                   []rectMeta
}

type Framebuffer struct {
	mu                        sync.RWMutex
	limits                    protocol.Limits
	generation, frameSequence uint64
	width, height             uint32
	pixels                    []byte
	// spare is the second framebuffer-sized buffer. It serves as the staging
	// buffer while a FRAME_PART sequence is in progress, and as the scratch
	// next-frame buffer for an ordinary single-message FRAME. It is always a
	// distinct allocation from pixels, so neither a partially reassembled part
	// nor an in-progress ordinary frame can mutate committed pixels. On commit
	// the two are swapped, so at most two framebuffer-sized buffers are ever
	// retained and no per-frame full-size allocation is needed.
	spare            []byte
	keyframeRequired bool
	staging          *frameStaging
	// discard drains the tail of a rejected delta sequence (see frameDiscard).
	discard *frameDiscard
}

func NewFramebuffer(limits protocol.Limits) *Framebuffer { return &Framebuffer{limits: limits} }

// Reset drops all display state so the next connection starts with clean
// generation and sequence accounting (F1). Each host service restarts its
// generation at 1 per connection; retaining the previous session's generation
// would reject every new DISPLAY_CONFIG as stale forever. Any partially
// reassembled FRAME_PART or pending drain state is dropped so a reconnect can
// never reuse it.
func (f *Framebuffer) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.generation, f.frameSequence = 0, 0
	f.width, f.height = 0, 0
	f.pixels = nil
	f.spare = nil
	f.keyframeRequired = false
	f.staging = nil
	f.discard = nil
}

func (f *Framebuffer) Configure(config protocol.DisplayConfig) error {
	if err := protocol.ValidateMessage(config, f.limits); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if config.Generation <= f.generation {
		return fmt.Errorf("client: stale display generation")
	}
	size := uint64(config.Width) * uint64(config.Height) * 4
	if size > uint64(protocol.MaxPixelPayloadHard)*16 {
		return protocol.ErrMessageTooLarge
	}
	// A DISPLAY_CONFIG may not interleave an in-progress part sequence (or a
	// drain of a rejected one): drop the partial staging so a half-reassembled
	// frame can never be reused, then reject the interleave.
	interleaved := f.staging != nil || f.discard != nil
	f.staging = nil
	f.discard = nil
	f.generation, f.frameSequence = config.Generation, 0
	f.width, f.height = config.Width, config.Height
	f.pixels = make([]byte, int(size))
	f.spare = make([]byte, int(size))
	f.keyframeRequired = true
	if interleaved {
		return fmt.Errorf("client: display config interleaved with frame parts")
	}
	return nil
}

func (f *Framebuffer) Snapshot() Snapshot {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return Snapshot{f.generation, f.frameSequence, f.width, f.height, append([]byte(nil), f.pixels...)}
}

// Generation returns the current display generation without copying pixels.
// Session recovery paths (KEYFRAME_REQUEST) only need the generation, so they
// must not pay for a full-framebuffer Snapshot copy.
func (f *Framebuffer) Generation() uint64 {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.generation
}

// Apply commits an ordinary single-message FRAME. It is rejected while a
// FRAME_PART sequence is in progress or a rejected one is still draining: the
// host must finish the logical frame before sending another one.
func (f *Framebuffer) Apply(frame protocol.Frame) error {
	if err := protocol.ValidateMessage(frame, f.limits); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.staging != nil || f.discard != nil {
		return fmt.Errorf("client: FRAME interleaved with frame parts")
	}
	return f.applyFrameLocked(frame)
}

func (f *Framebuffer) applyFrameLocked(frame protocol.Frame) error {
	if f.generation == 0 {
		return ErrDisplayNotConfigured
	}
	if frame.Generation != f.generation {
		return fmt.Errorf("client: stale display generation")
	}
	if f.keyframeRequired && !frame.Keyframe {
		return ErrKeyframeRequired
	}
	if !frame.Keyframe && frame.BaseFrameSequence != f.frameSequence {
		return ErrKeyframeRequired
	}
	// A keyframe must advance the sequence. A same-generation keyframe at or
	// below the last committed sequence is a duplicate or replayed frame: it
	// would rewind frameSequence and re-present superseded pixels. Reject it
	// before the scratch buffer is touched so the committed pixels and
	// sequence are left untouched. A delta can never trip this: its validated
	// base equals the committed sequence and is strictly below its own.
	if frame.Keyframe && frame.FrameSequence <= f.frameSequence {
		return fmt.Errorf("client: stale frame sequence")
	}

	// Reuse the spare buffer as the scratch next-frame buffer: it is always a
	// distinct allocation from pixels, so a validation or decode failure below
	// leaves the last committed pixels untouched, and a successful commit
	// swaps it into place without allocating a new full-size buffer.
	next := f.stagingBufferLocked()
	if next == nil {
		return ErrDisplayNotConfigured
	}
	if !frame.Keyframe {
		copy(next, f.pixels)
	}
	covered := uint64(0)
	for _, rect := range frame.Rectangles {
		if rect.X >= f.width || rect.Y >= f.height || rect.Width > f.width-rect.X || rect.Height > f.height-rect.Y {
			return fmt.Errorf("client: rectangle outside display")
		}
		decoded, err := decodeRectangle(rect, f.limits)
		if err != nil {
			return err
		}
		blitRectangle(next, decoded, rect, f.width)
		covered += uint64(rect.Width) * uint64(rect.Height)
	}
	if frame.Keyframe && covered != uint64(f.width)*uint64(f.height) {
		return fmt.Errorf("client: incomplete keyframe")
	}
	f.pixels, f.spare = next, f.pixels
	f.frameSequence, f.keyframeRequired = frame.FrameSequence, false
	return nil
}

// ApplyPart feeds one FRAME_PART of a logical v2 frame into the single-slot
// reassembler. It returns committed=true exactly once, on the part that
// completes the frame, after the whole frame has been atomically committed to
// the visible framebuffer. Intermediate parts return (false, nil) and never
// change the visible pixels or frame sequence.
//
// A recoverable part-0 base mismatch (or a delta while a keyframe is required)
// returns ErrKeyframeRequired and moves the reassembler into a bounded drain of
// the rejected sequence's trailing parts, so the caller can send exactly one
// KEYFRAME_REQUEST while the host finishes the logical frame it already
// started. Trailing parts that match the drained header in order are consumed
// without touching pixels; every other error is fatal for the session: a
// duplicate/out-of-order part, an inconsistent header, a stale generation,
// cross-part overlap, a cumulative rectangle overflow, an unexpected FRAME or
// DISPLAY_CONFIG mid-sequence, or an incomplete final keyframe all abort
// without presenting partial pixels.
func (f *Framebuffer) ApplyPart(part protocol.FramePart) (bool, error) {
	if err := protocol.ValidateMessage(part, f.limits); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.generation == 0 {
		return false, ErrDisplayNotConfigured
	}
	if part.Generation != f.generation {
		return false, fmt.Errorf("client: stale display generation")
	}
	if f.discard != nil {
		return f.drainDiscardLocked(part)
	}
	if part.PartIndex == 0 {
		return f.startStagingLocked(part)
	}
	return f.continueStagingLocked(part)
}

// startStagingLocked begins a logical frame at part 0. The visible pixels and
// sequence stay untouched; the staging buffer starts as a copy of the last
// committed framebuffer for a delta (a keyframe overwrites every byte and is
// only committed once full coverage is proven).
func (f *Framebuffer) startStagingLocked(part protocol.FramePart) (bool, error) {
	// A part 0 while a sequence is already in progress is a restart/duplicate,
	// not a continuation. Drop the partial frame and reject.
	if f.staging != nil {
		f.staging = nil
		return false, fmt.Errorf("client: FRAME_PART restart at part 0")
	}
	// Recoverable: the client is missing the base this delta builds on, or the
	// previous frame never completed. Drop any staging, remember the rejected
	// sequence so its already-in-flight trailing parts can be drained, and let
	// the caller request a keyframe.
	if f.keyframeRequired && !part.Keyframe {
		f.staging = nil
		f.beginDiscardLocked(part)
		return false, ErrKeyframeRequired
	}
	if !part.Keyframe && part.BaseFrameSequence != f.frameSequence {
		f.staging = nil
		f.beginDiscardLocked(part)
		return false, ErrKeyframeRequired
	}
	// A keyframe part 0 must advance the sequence. Reject a stale or duplicate
	// one before any staging buffer exists, so the committed pixels and
	// sequence stay untouched and no partial sequence is left active.
	if part.Keyframe && part.FrameSequence <= f.frameSequence {
		f.staging = nil
		return false, fmt.Errorf("client: stale frame sequence")
	}
	staging := f.stagingBufferLocked()
	if staging == nil {
		return false, ErrDisplayNotConfigured
	}
	if !part.Keyframe {
		copy(staging, f.pixels)
	}
	f.staging = &frameStaging{
		generation:        part.Generation,
		frameSequence:     part.FrameSequence,
		baseFrameSequence: part.BaseFrameSequence,
		keyframe:          part.Keyframe,
		partCount:         part.PartCount,
		pixels:            staging,
	}
	return f.applyPartRectanglesLocked(part)
}

// continueStagingLocked feeds a non-zero part, requiring it to carry the same
// logical header as part 0 and to be the next index in sequence.
func (f *Framebuffer) continueStagingLocked(part protocol.FramePart) (bool, error) {
	s := f.staging
	if s == nil {
		return false, fmt.Errorf("client: FRAME_PART %d without part 0", part.PartIndex)
	}
	if part.Generation != s.generation || part.FrameSequence != s.frameSequence ||
		part.BaseFrameSequence != s.baseFrameSequence || part.Keyframe != s.keyframe ||
		part.PartCount != s.partCount {
		return false, fmt.Errorf("client: inconsistent FRAME_PART header")
	}
	return f.applyPartRectanglesLocked(part)
}

// beginDiscardLocked records the header of a rejected delta sequence so the
// host's already-in-flight trailing parts can be drained instead of tearing the
// session down. PartCount is at least two (validated), so the next expected
// index is 1. Part 0's geometry is seeded so cross-part overlap and the
// cumulative rectangle cap are still enforced across the whole sequence; its
// pixels are never decoded or blitted.
func (f *Framebuffer) beginDiscardLocked(part protocol.FramePart) {
	rects := make([]rectMeta, 0, len(part.Rectangles))
	for _, rect := range part.Rectangles {
		rects = append(rects, rectMeta{rect.X, rect.Y, rect.Width, rect.Height})
	}
	f.discard = &frameDiscard{
		generation:        part.Generation,
		frameSequence:     part.FrameSequence,
		baseFrameSequence: part.BaseFrameSequence,
		keyframe:          part.Keyframe,
		partCount:         part.PartCount,
		nextPartIndex:     1,
		rectCount:         len(part.Rectangles),
		rectangles:        rects,
	}
}

// drainDiscardLocked consumes one trailing part of a rejected sequence. It
// requires the same logical header and the next index, and validates the
// part's bounded geometry (in-bounds, no cross-part overlap, cumulative
// rectangle cap) without decoding or blitting any pixels. On the final part the
// drain clears itself so the host's forced keyframe can begin a fresh
// sequence. A restart at part 0, a mismatched header or order, or a malformed
// part is fatal.
func (f *Framebuffer) drainDiscardLocked(part protocol.FramePart) (bool, error) {
	d := f.discard
	// A part 0 while draining is a restart, never a continuation; a keyframe
	// cannot begin until the rejected sequence's tail has drained.
	if part.PartIndex == 0 {
		return false, fmt.Errorf("client: FRAME_PART restart at part 0 during drain")
	}
	if part.Generation != d.generation || part.FrameSequence != d.frameSequence ||
		part.BaseFrameSequence != d.baseFrameSequence || part.Keyframe != d.keyframe ||
		part.PartCount != d.partCount {
		return false, fmt.Errorf("client: inconsistent FRAME_PART header during drain")
	}
	if part.PartIndex != d.nextPartIndex {
		return false, fmt.Errorf("client: unexpected FRAME_PART index %d, want %d", part.PartIndex, d.nextPartIndex)
	}
	if d.rectCount+len(part.Rectangles) > int(f.maxRectangles()) {
		return false, fmt.Errorf("client: cumulative rectangle count exceeds limit")
	}
	for _, rect := range part.Rectangles {
		if rect.X >= f.width || rect.Y >= f.height || rect.Width > f.width-rect.X || rect.Height > f.height-rect.Y {
			return false, fmt.Errorf("client: rectangle outside display")
		}
		for _, earlier := range d.rectangles {
			if earlier.overlaps(rect) {
				return false, fmt.Errorf("client: overlapping rectangles across parts")
			}
		}
		d.rectangles = append(d.rectangles, rectMeta{rect.X, rect.Y, rect.Width, rect.Height})
		d.rectCount++
	}
	d.nextPartIndex++
	if d.nextPartIndex >= d.partCount {
		f.discard = nil
	}
	return false, nil
}

func (f *Framebuffer) applyPartRectanglesLocked(part protocol.FramePart) (bool, error) {
	s := f.staging
	if part.PartIndex != s.nextPartIndex {
		return false, fmt.Errorf("client: unexpected FRAME_PART index %d, want %d", part.PartIndex, s.nextPartIndex)
	}
	if s.rectCount+len(part.Rectangles) > int(f.maxRectangles()) {
		return false, fmt.Errorf("client: cumulative rectangle count exceeds limit")
	}
	for _, rect := range part.Rectangles {
		if rect.X >= f.width || rect.Y >= f.height || rect.Width > f.width-rect.X || rect.Height > f.height-rect.Y {
			return false, fmt.Errorf("client: rectangle outside display")
		}
		for _, earlier := range s.rectangles {
			if earlier.overlaps(rect) {
				return false, fmt.Errorf("client: overlapping rectangles across parts")
			}
		}
		decoded, err := decodeRectangle(rect, f.limits)
		if err != nil {
			return false, err
		}
		blitRectangle(s.pixels, decoded, rect, f.width)
		s.rectangles = append(s.rectangles, rectMeta{rect.X, rect.Y, rect.Width, rect.Height})
		s.rectCount++
		s.covered += uint64(rect.Width) * uint64(rect.Height)
	}
	s.nextPartIndex++
	if s.nextPartIndex < s.partCount {
		return false, nil
	}
	return f.commitStagingLocked()
}

// commitStagingLocked swaps the fully reassembled staging buffer into the
// visible framebuffer. A keyframe that does not cover every display pixel is
// rejected here, so no partial frame is ever presented.
func (f *Framebuffer) commitStagingLocked() (bool, error) {
	s := f.staging
	if s.keyframe && s.covered != uint64(f.width)*uint64(f.height) {
		return false, fmt.Errorf("client: incomplete keyframe")
	}
	f.pixels, f.spare = s.pixels, f.pixels
	f.frameSequence = s.frameSequence
	f.keyframeRequired = false
	f.staging = nil
	return true, nil
}

// stagingBufferLocked returns the spare buffer sized to the current display,
// allocating it if it is missing or mismatched. The spare is always distinct
// from f.pixels, so staging never aliases committed pixels.
func (f *Framebuffer) stagingBufferLocked() []byte {
	n := len(f.pixels)
	if n == 0 {
		return nil
	}
	if len(f.spare) != n {
		f.spare = make([]byte, n)
	}
	return f.spare
}

// maxRectangles mirrors the clamping protocol.Limits.bounded applies, so a
// zero or oversized configured field falls back to the protocol hard cap and
// the cumulative cross-part count is enforced against the effective limit.
func (f *Framebuffer) maxRectangles() uint16 {
	if f.limits.MaxRectangles == 0 || f.limits.MaxRectangles > protocol.MaxRectanglesHard {
		return protocol.MaxRectanglesHard
	}
	return f.limits.MaxRectangles
}

// blitRectangle copies a decoded rectangle into a BGRA framebuffer at the
// rectangle's offset. Both callers guarantee the rectangle is in-bounds.
func blitRectangle(dst, decoded []byte, rect protocol.Rectangle, displayWidth uint32) {
	rowBytes := int(rect.Width) * 4
	for row := uint32(0); row < rect.Height; row++ {
		off := (int(rect.Y+row)*int(displayWidth) + int(rect.X)) * 4
		src := int(row) * rowBytes
		copy(dst[off:off+rowBytes], decoded[src:src+rowBytes])
	}
}

func decodeRectangle(rect protocol.Rectangle, limits protocol.Limits) ([]byte, error) {
	expected := uint64(rect.Width) * uint64(rect.Height) * 4
	if expected > uint64(protocol.MaxPixelPayloadHard) {
		return nil, protocol.ErrMessageTooLarge
	}
	if rect.Encoding == protocol.EncodingRawBGRA {
		// rect.Pixels is already the length-validated raw payload owned by
		// this message; the caller's row blit reads it directly into staging,
		// so there is no need to copy it first.
		return rect.Pixels, nil
	}
	zr, err := zlib.NewReader(bytes.NewReader(rect.Pixels))
	if err != nil {
		return nil, fmt.Errorf("client: zlib: %w", err)
	}
	defer zr.Close()
	out, err := io.ReadAll(io.LimitReader(zr, int64(expected)+1))
	if err != nil {
		return nil, fmt.Errorf("client: zlib: %w", err)
	}
	if uint64(len(out)) != expected {
		return nil, fmt.Errorf("client: decoded rectangle length")
	}
	return out, nil
}
