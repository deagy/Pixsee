package protocol

import (
	"io"
	"testing"
)

// benchFrame returns a valid FRAME with a large raw rectangle plus a small zlib
// rectangle, large enough that a full-payload copy is visible in allocations.
func benchFrame() Frame {
	return Frame{Generation: 1, FrameSequence: 2, BaseFrameSequence: 1, Rectangles: []Rectangle{
		{X: 0, Y: 0, Width: 1024, Height: 1024, Encoding: EncodingRawBGRA, Pixels: make([]byte, 1024*1024*4)},
		{X: 1024, Y: 0, Width: 4, Height: 4, Encoding: EncodingZlibBGRA, Pixels: []byte{0x78, 0x9c, 0x03}},
	}}
}

// BenchmarkValidateLargeFrame measures the arithmetic size check on a pixel
// message. Before the fix this marshaled (and copied) the whole payload; after
// it, a large frame validates with ~0 allocs, which is the direct evidence that
// the avoidable copy is gone.
func BenchmarkValidateLargeFrame(b *testing.B) {
	frame := benchFrame()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := ValidateMessage(frame, DefaultLimits()); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkEncodeLargeFrame measures a full Encode of the same frame. After the
// fix Encode marshals exactly once; its allocation reflects that single payload
// copy plus the record header, not the former second copy inside validation.
func BenchmarkEncodeLargeFrame(b *testing.B) {
	frame := benchFrame()
	if size, ok := pixelPayloadSize(frame); ok {
		b.SetBytes(int64(size))
	}
	b.ReportAllocs()
	b.ResetTimer()
	enc := NewEncoder(io.Discard, DefaultLimits())
	for i := 0; i < b.N; i++ {
		if err := enc.Encode(frame); err != nil {
			b.Fatal(err)
		}
	}
}
