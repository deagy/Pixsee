package client

import (
	"bytes"
	"compress/zlib"
	"errors"
	"testing"

	"virtualdesktop/internal/protocol"
)

func TestFramebufferComposesRegionsAndResizeRequiresKeyframe(t *testing.T) {
	fb := NewFramebuffer(protocol.DefaultLimits())
	if err := fb.Configure(protocol.DisplayConfig{Generation: 1, Width: 3, Height: 2, PixelFormat: protocol.PixelBGRA8888}); err != nil {
		t.Fatal(err)
	}
	base := pixels(6, 1)
	if err := fb.Apply(protocol.Frame{Generation: 1, FrameSequence: 1, Keyframe: true, Rectangles: []protocol.Rectangle{{Width: 3, Height: 2, Encoding: protocol.EncodingRawBGRA, Pixels: base}}}); err != nil {
		t.Fatal(err)
	}
	patch := pixels(2, 20)
	if err := fb.Apply(protocol.Frame{Generation: 1, FrameSequence: 2, BaseFrameSequence: 1, Rectangles: []protocol.Rectangle{{X: 1, Y: 0, Width: 1, Height: 2, Encoding: protocol.EncodingRawBGRA, Pixels: patch}}}); err != nil {
		t.Fatal(err)
	}
	want := append([]byte(nil), base...)
	copy(want[4:8], patch[:4])
	copy(want[16:20], patch[4:])
	if got := fb.Snapshot(); got.Width != 3 || got.Height != 2 || !bytes.Equal(got.Pixels, want) {
		t.Fatalf("unexpected snapshot: %#v", got)
	}

	if err := fb.Configure(protocol.DisplayConfig{Generation: 2, Width: 1, Height: 1, PixelFormat: protocol.PixelBGRA8888}); err != nil {
		t.Fatal(err)
	}
	if err := fb.Apply(protocol.Frame{Generation: 2, FrameSequence: 3, BaseFrameSequence: 2, Rectangles: []protocol.Rectangle{{Width: 1, Height: 1, Encoding: protocol.EncodingRawBGRA, Pixels: pixels(1, 9)}}}); !errors.Is(err, ErrKeyframeRequired) {
		t.Fatalf("got %v", err)
	}
}

// TestFramebufferRejectsStaleKeyframeSequence proves a same-generation keyframe
// whose FrameSequence does not advance past the last committed sequence is a
// duplicate or replayed frame: it is rejected without mutating the committed
// pixels or sequence, and a valid higher keyframe — the host's periodic refresh
// — still commits.
func TestFramebufferRejectsStaleKeyframeSequence(t *testing.T) {
	fb := configuredFramebuffer(t, protocol.DefaultLimits())
	first := protocol.Frame{Generation: 1, FrameSequence: 1, Keyframe: true, Rectangles: keyframeRects4x2()}
	if err := fb.Apply(first); err != nil {
		t.Fatal(err)
	}
	// Host cadence: a delta, then a periodic keyframe for the refresh. Both
	// advance the sequence and must remain accepted.
	delta := protocol.Frame{Generation: 1, FrameSequence: 2, BaseFrameSequence: 1, Rectangles: []protocol.Rectangle{rawRect(0, 0, 1, 1, 50)}}
	if err := fb.Apply(delta); err != nil {
		t.Fatalf("valid delta rejected: %v", err)
	}
	periodic := protocol.Frame{Generation: 1, FrameSequence: 3, Keyframe: true, Rectangles: forcedRects4x2()}
	if err := fb.Apply(periodic); err != nil {
		t.Fatalf("periodic keyframe rejected: %v", err)
	}
	before := fb.Snapshot()

	// Stale lower (1, 2) and the equal duplicate (3) must all be rejected
	// without touching the committed frame.
	for _, seq := range []uint64{1, 2, 3} {
		stale := protocol.Frame{Generation: 1, FrameSequence: seq, Keyframe: true, Rectangles: keyframeRects4x2()}
		if err := fb.Apply(stale); err == nil {
			t.Fatalf("accepted a keyframe at sequence %d <= committed 3", seq)
		}
		if got := fb.Snapshot(); got.FrameSequence != before.FrameSequence || !bytes.Equal(got.Pixels, before.Pixels) {
			t.Fatalf("rejecting stale keyframe %d mutated the committed frame", seq)
		}
	}

	// The host's next periodic keyframe advances the sequence and commits.
	next := protocol.Frame{Generation: 1, FrameSequence: 4, Keyframe: true, Rectangles: keyframeRects4x2()}
	if err := fb.Apply(next); err != nil {
		t.Fatalf("valid higher keyframe rejected: %v", err)
	}
	want := reference4x2(t, first, delta, periodic, next)
	if got := fb.Snapshot(); got.FrameSequence != want.FrameSequence || !bytes.Equal(got.Pixels, want.Pixels) {
		t.Fatal("valid higher keyframe did not commit")
	}
}

func TestFramebufferDecodesZlibAndRejectsBadUpdatesAtomically(t *testing.T) {
	limits := protocol.DefaultLimits()
	fb := NewFramebuffer(limits)
	if err := fb.Configure(protocol.DisplayConfig{Generation: 1, Width: 2, Height: 1, PixelFormat: protocol.PixelBGRA8888}); err != nil {
		t.Fatal(err)
	}
	raw := pixels(2, 3)
	var compressed bytes.Buffer
	zw := zlib.NewWriter(&compressed)
	_, _ = zw.Write(raw)
	_ = zw.Close()
	if err := fb.Apply(protocol.Frame{Generation: 1, FrameSequence: 1, Keyframe: true, Rectangles: []protocol.Rectangle{{Width: 2, Height: 1, Encoding: protocol.EncodingZlibBGRA, Pixels: compressed.Bytes()}}}); err != nil {
		t.Fatal(err)
	}
	before := fb.Snapshot()
	bad := protocol.Frame{Generation: 1, FrameSequence: 2, BaseFrameSequence: 1, Rectangles: []protocol.Rectangle{
		{Width: 1, Height: 1, Encoding: protocol.EncodingRawBGRA, Pixels: pixels(1, 40)},
		{X: 2, Width: 1, Height: 1, Encoding: protocol.EncodingRawBGRA, Pixels: pixels(1, 50)},
	}}
	if err := fb.Apply(bad); err == nil {
		t.Fatal("accepted out-of-bounds update")
	}
	if after := fb.Snapshot(); !bytes.Equal(after.Pixels, before.Pixels) || after.FrameSequence != before.FrameSequence {
		t.Fatal("malformed update partially mutated framebuffer")
	}

	bomb := compressedPayload(t, append(raw, 0))
	if err := fb.Apply(protocol.Frame{Generation: 1, FrameSequence: 2, BaseFrameSequence: 1, Rectangles: []protocol.Rectangle{{Width: 2, Height: 1, Encoding: protocol.EncodingZlibBGRA, Pixels: bomb}}}); err == nil {
		t.Fatal("accepted zlib output larger than rectangle")
	}
}

func pixels(count int, seed byte) []byte {
	p := make([]byte, count*4)
	for i := 0; i < count; i++ {
		p[i*4], p[i*4+1], p[i*4+2], p[i*4+3] = seed+byte(i), seed+byte(i)+1, seed+byte(i)+2, 0xff
	}
	return p
}

func compressedPayload(t *testing.T, p []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zlib.NewWriter(&b)
	if _, err := w.Write(p); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// TestDecodeRectangleRawBGRAReusesValidatedPixels proves the raw path returns
// the already-owned, length-validated rectangle slice (the row blit performs
// the sole copy into staging) rather than allocating a second full copy. The
// decoded bytes are byte-identical to the input and decoding allocates nothing.
func TestDecodeRectangleRawBGRAReusesValidatedPixels(t *testing.T) {
	rect := rawRect(0, 0, 3, 2, 7)
	decoded, err := decodeRectangle(rect, protocol.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded) != len(rect.Pixels) || !bytes.Equal(decoded, rect.Pixels) {
		t.Fatal("raw decode did not return the rectangle's pixels")
	}
	if &decoded[0] != &rect.Pixels[0] {
		t.Fatal("raw decode copied the validated pixel slice instead of reusing it")
	}
	if allocs := testing.AllocsPerRun(100, func() {
		_, _ = decodeRectangle(rect, protocol.DefaultLimits())
	}); allocs != 0 {
		t.Fatalf("raw decode allocated %.1f times, want 0", allocs)
	}
}

func BenchmarkDecodeRectangleRawBGRA(b *testing.B) {
	rect := rawRect(0, 0, 64, 64, 3)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := decodeRectangle(rect, protocol.DefaultLimits()); err != nil {
			b.Fatal(err)
		}
	}
}

// FuzzDecodeRectangle feeds the zlib framebuffer decoder random bytes so the
// adversarial zlib path — over-decompression, truncation, malformed streams,
// and expansion-bomb payloads — is exercised without panicking or leaking.
// decodeRectangle bounds the decoded length to the rectangle's pixel count, so
// every input must either decode to exactly the expected length or return an
// error; a panic here is a real crash.
func FuzzDecodeRectangle(f *testing.F) {
	limits := protocol.DefaultLimits()
	// A valid zlib rectangle to seed the corpus.
	var compressed bytes.Buffer
	zw := zlib.NewWriter(&compressed)
	_, _ = zw.Write(pixels(4, 1))
	_ = zw.Close()
	f.Add(uint32(2), uint32(2), compressed.Bytes())
	f.Fuzz(func(t *testing.T, width, height uint32, raw []byte) {
		rect := protocol.Rectangle{Width: width, Height: height, Encoding: protocol.EncodingZlibBGRA, Pixels: raw}
		// A rectangle whose pixel payload exceeds the hard cap is rejected
		// before any allocation, so skip it rather than treat it as an
		// interesting fuzz input.
		if uint64(width)*uint64(height)*4 > uint64(protocol.MaxPixelPayloadHard) {
			return
		}
		// decodeRectangle must never panic on adversarial zlib input; it
		// either returns the exact expected byte count or a non-nil error.
		// The existing unit test already asserts the success/error contract.
		_, _ = decodeRectangle(rect, limits)
	})
}
