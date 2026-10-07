package protocol

import (
	"errors"
	"io"
	"runtime"
	"testing"
)

// TestPixelPayloadSizeMatchesMarshal proves the arithmetic pixel-payload size
// equals the bytes marshal actually produces for ordinary FRAME and FRAME_PART
// messages across raw, zlib and multi-rectangle cases. This is the size the
// codec bounds against MaxPixelPayload without marshaling.
func TestPixelPayloadSizeMatchesMarshal(t *testing.T) {
	large := make([]Rectangle, 0, MaxRectanglesHard)
	for i := uint32(0); i < uint32(MaxRectanglesHard); i++ {
		large = append(large, Rectangle{X: i * 2, Y: 0, Width: 2, Height: 1, Encoding: EncodingRawBGRA, Pixels: make([]byte, 8)})
	}
	messages := []Message{
		Frame{Generation: 1, FrameSequence: 2, BaseFrameSequence: 1, Rectangles: []Rectangle{
			{X: 0, Y: 0, Width: 1, Height: 1, Encoding: EncodingRawBGRA, Pixels: make([]byte, 4)},
		}},
		Frame{Generation: 3, FrameSequence: 4, Keyframe: true, Rectangles: []Rectangle{
			{X: 0, Y: 0, Width: 4, Height: 2, Encoding: EncodingZlibBGRA, Pixels: []byte{0x78, 0x9c, 0x03}},
			{X: 4, Y: 0, Width: 1, Height: 1, Encoding: EncodingRawBGRA, Pixels: make([]byte, 4)},
		}},
		Frame{Generation: 1, FrameSequence: 1, Keyframe: true, Rectangles: large},
		FramePart{Generation: 1, FrameSequence: 2, BaseFrameSequence: 1, PartIndex: 0, PartCount: 2, Rectangles: []Rectangle{
			{X: 0, Y: 0, Width: 1, Height: 1, Encoding: EncodingRawBGRA, Pixels: make([]byte, 4)},
		}},
		FramePart{Generation: 5, FrameSequence: 6, Keyframe: true, PartIndex: 3, PartCount: 7, Rectangles: []Rectangle{
			{X: 0, Y: 0, Width: 1, Height: 1, Encoding: EncodingRawBGRA, Pixels: make([]byte, 4)},
			{X: 1, Y: 0, Width: 4, Height: 2, Encoding: EncodingZlibBGRA, Pixels: []byte{0x78, 0x9c, 0x03}},
		}},
		FramePart{Generation: 1, FrameSequence: 1, Keyframe: true, PartIndex: 0, PartCount: 2, Rectangles: large},
	}
	for i, m := range messages {
		payload, err := marshal(m)
		if err != nil {
			t.Fatalf("case %d: marshal: %v", i, err)
		}
		got, ok := pixelPayloadSize(m)
		if !ok {
			t.Fatalf("case %d: %s has no arithmetic size", i, m.Type())
		}
		if want := uint64(len(payload)); got != want {
			t.Fatalf("case %d (%s): arithmetic size %d != marshal size %d", i, m.Type(), got, want)
		}
	}
}

// TestEncodeRejectsOversizedFrameBeforeMarshaling proves a FRAME whose aggregate
// pixel payload exceeds the pixel budget is rejected without the codec
// marshaling — and therefore copying — that aggregate. Each rectangle is at or
// under the per-rectangle cap, so only the aggregate size is at fault: exactly
// the case that used to marshal the whole payload before checking it.
func TestEncodeRejectsOversizedFrameBeforeMarshaling(t *testing.T) {
	// 2048x2048 raw BGRA is exactly MaxPixelPayloadHard (16 MiB), so it passes
	// the per-rectangle cap; a second, non-overlapping rectangle tips the
	// aggregate over the budget.
	big := Rectangle{X: 0, Y: 0, Width: 2048, Height: 2048, Encoding: EncodingRawBGRA, Pixels: make([]byte, 2048*2048*4)}
	small := Rectangle{X: 2048, Y: 0, Width: 4, Height: 4, Encoding: EncodingRawBGRA, Pixels: make([]byte, 4*4*4)}
	frame := Frame{Generation: 1, FrameSequence: 1, Keyframe: true, Rectangles: []Rectangle{big, small}}

	if err := ValidateMessage(frame, DefaultLimits()); !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("ValidateMessage accepted an oversized frame: %v", err)
	}
	if err := NewEncoder(io.Discard, DefaultLimits()).Encode(frame); !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("Encode accepted an oversized frame: %v", err)
	}

	allocated := allocatedDuring(t, func() {
		_ = NewEncoder(io.Discard, DefaultLimits()).Encode(frame)
	})
	if allocated >= uint64(len(big.Pixels)) {
		t.Fatalf("Encode allocated %d bytes rejecting an oversized frame; it must reject before marshaling the %d-byte aggregate", allocated, len(big.Pixels))
	}
}

// allocatedDuring reports the heap bytes allocated while f runs.
func allocatedDuring(t *testing.T, f func()) uint64 {
	t.Helper()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}
