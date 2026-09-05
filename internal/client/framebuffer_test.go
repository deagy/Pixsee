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
