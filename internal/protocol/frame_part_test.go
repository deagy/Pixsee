package protocol

import (
	"errors"
	"reflect"
	"testing"
)

// framePartTestRect returns a 2x1 raw BGRA rectangle whose marshaled payload is
// 30 bytes (22 header + 8 pixels) at column i, so a part holding n rectangles
// has a payload of 31 + 30n bytes.
func framePartTestRect(i uint32) Rectangle {
	return Rectangle{X: i * 2, Y: 0, Width: 2, Height: 1, Encoding: EncodingRawBGRA, Pixels: make([]byte, 8)}
}

func TestSplitFrameDeterministicPacking(t *testing.T) {
	frame := Frame{Generation: 3, FrameSequence: 5, BaseFrameSequence: 4, Rectangles: []Rectangle{
		framePartTestRect(0), framePartTestRect(1), framePartTestRect(2), framePartTestRect(3), framePartTestRect(4),
	}}
	// budget 91 fits exactly two 30-byte rectangles (31 + 60) and not three
	// (31 + 90 = 121), so a greedy pack yields parts of 2, 2, 1.
	limits := Limits{MaxPixelPayload: 91}
	parts, err := SplitFrame(frame, limits)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 3 {
		t.Fatalf("got %d parts, want 3", len(parts))
	}
	for i, want := range []int{2, 2, 1} {
		if len(parts[i].Rectangles) != want {
			t.Fatalf("part %d has %d rectangles, want %d", i, len(parts[i].Rectangles), want)
		}
		if parts[i].PartIndex != uint16(i) || parts[i].PartCount != 3 {
			t.Fatalf("part %d has index/count %d/%d, want %d/3", i, parts[i].PartIndex, parts[i].PartCount, i)
		}
		if parts[i].Generation != frame.Generation || parts[i].FrameSequence != frame.FrameSequence || parts[i].BaseFrameSequence != frame.BaseFrameSequence {
			t.Fatalf("part %d dropped the frame header: %#v", i, parts[i])
		}
		if err := ValidateMessage(parts[i], DefaultLimits()); err != nil {
			t.Fatalf("part %d invalid: %v", i, err)
		}
		if got := framePartPayloadSize(parts[i]); got > 91 {
			t.Fatalf("part %d payload %d exceeds budget 91", i, got)
		}
	}

	// Rectangles keep their original order across the parts.
	var order []Rectangle
	for _, p := range parts {
		order = append(order, p.Rectangles...)
	}
	if !reflect.DeepEqual(order, frame.Rectangles) {
		t.Fatalf("rectangle order changed:\n got %#v\nwant %#v", order, frame.Rectangles)
	}

	// Determinism: the same frame and limits yield the same partition.
	again, err := SplitFrame(frame, limits)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parts, again) {
		t.Fatalf("SplitFrame is not deterministic:\n%#v\n%#v", parts, again)
	}
}

func TestSplitFrameKeepsWholeRectangles(t *testing.T) {
	// A budget that fits one rectangle per part must not split any rectangle.
	frame := Frame{Generation: 1, FrameSequence: 2, BaseFrameSequence: 1, Rectangles: []Rectangle{
		framePartTestRect(0), framePartTestRect(1), framePartTestRect(2),
	}}
	parts, err := SplitFrame(frame, Limits{MaxPixelPayload: 61})
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 3 {
		t.Fatalf("got %d parts, want 3", len(parts))
	}
	for i, p := range parts {
		if len(p.Rectangles) != 1 {
			t.Fatalf("part %d holds %d rectangles, want 1", i, len(p.Rectangles))
		}
	}
}

func TestSplitFrameSignals(t *testing.T) {
	two := Frame{Generation: 1, FrameSequence: 2, BaseFrameSequence: 1, Rectangles: []Rectangle{framePartTestRect(0), framePartTestRect(1)}}
	if _, err := SplitFrame(two, Limits{MaxPixelPayload: 1 << 20}); !errors.Is(err, ErrFrameFits) {
		t.Fatalf("fits-in-one: got %v, want ErrFrameFits", err)
	}

	one := Frame{Generation: 1, FrameSequence: 1, Keyframe: true, Rectangles: []Rectangle{framePartTestRect(0)}}
	if _, err := SplitFrame(one, Limits{MaxPixelPayload: 40}); !errors.Is(err, ErrPartTooLarge) {
		t.Fatalf("oversized rectangle: got %v, want ErrPartTooLarge", err)
	}

	many := Frame{Generation: 1, FrameSequence: 1, Keyframe: true}
	for i := uint32(0); i < uint32(MaxFramePartsHard)+1; i++ {
		many.Rectangles = append(many.Rectangles, framePartTestRect(i))
	}
	if _, err := SplitFrame(many, Limits{MaxPixelPayload: 61}); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("too many parts: got %v, want ErrFrameTooLarge", err)
	}

	if _, err := SplitFrame(Frame{Generation: 1, FrameSequence: 1, Keyframe: true}, Limits{MaxPixelPayload: 1 << 20}); !errors.Is(err, ErrMalformed) {
		t.Fatalf("empty frame: got %v, want ErrMalformed", err)
	}
}

// TestSplitFrameHonorsConfiguredMaxParts proves SplitFrame uses the caller's
// active part cap rather than the hard maximum: a frame that needs two parts
// under a MaxFrameParts of one is rejected, while the same frame with the
// default cap splits normally.
func TestSplitFrameHonorsConfiguredMaxParts(t *testing.T) {
	frame := Frame{Generation: 1, FrameSequence: 2, BaseFrameSequence: 1, Rectangles: []Rectangle{
		framePartTestRect(0), framePartTestRect(1),
	}}
	// Budget 61 fits exactly one 30-byte rectangle per part, so two parts are
	// required.
	if _, err := SplitFrame(frame, Limits{MaxPixelPayload: 61, MaxFrameParts: 1}); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("got %v, want ErrFrameTooLarge", err)
	}
	parts, err := SplitFrame(frame, Limits{MaxPixelPayload: 61})
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 {
		t.Fatalf("got %d parts, want 2", len(parts))
	}
}

// TestSplitFrameHonorsConfiguredPayloadBudget proves SplitFrame uses the
// caller's active per-part payload budget, not the hard pixel maximum.
func TestSplitFrameHonorsConfiguredPayloadBudget(t *testing.T) {
	// Each rectangle marshals to 30 bytes, so a 61-byte budget forces one per
	// part while the default hard budget packs all three together.
	frame := Frame{Generation: 1, FrameSequence: 2, BaseFrameSequence: 1, Rectangles: []Rectangle{
		framePartTestRect(0), framePartTestRect(1), framePartTestRect(2),
	}}
	parts, err := SplitFrame(frame, Limits{MaxPixelPayload: 61})
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 3 {
		t.Fatalf("reduced budget: got %d parts, want 3", len(parts))
	}
	if _, err := SplitFrame(frame, DefaultLimits()); !errors.Is(err, ErrFrameFits) {
		t.Fatalf("default budget: got %v, want ErrFrameFits", err)
	}
}

// TestSplitFrameMaxPartsFits confirms a frame needing exactly the hard part cap
// is accepted.
func TestSplitFrameMaxPartsFits(t *testing.T) {
	frame := Frame{Generation: 1, FrameSequence: 1, Keyframe: true}
	for i := uint32(0); i < uint32(MaxFramePartsHard); i++ {
		frame.Rectangles = append(frame.Rectangles, framePartTestRect(i))
	}
	parts, err := SplitFrame(frame, Limits{MaxPixelPayload: 61})
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != int(MaxFramePartsHard) {
		t.Fatalf("got %d parts, want %d", len(parts), MaxFramePartsHard)
	}
}

// TestFramePartPayloadSizeMatchesMarshal proves the incremental arithmetic size
// equals the bytes marshal actually produces for a varied mix of raw and zlib
// rectangles, including a large rectangle list.
func TestFramePartPayloadSizeMatchesMarshal(t *testing.T) {
	large := make([]Rectangle, 0, MaxRectanglesHard)
	for i := uint32(0); i < uint32(MaxRectanglesHard); i++ {
		large = append(large, framePartTestRect(i))
	}
	cases := []FramePart{
		{Generation: 1, FrameSequence: 2, BaseFrameSequence: 1, PartIndex: 0, PartCount: 2, Rectangles: []Rectangle{framePartTestRect(0)}},
		{Generation: 5, FrameSequence: 6, Keyframe: true, PartIndex: 3, PartCount: 7, Rectangles: []Rectangle{
			{Width: 1, Height: 1, Encoding: EncodingRawBGRA, Pixels: make([]byte, 4)},
			{Width: 4, Height: 2, Encoding: EncodingZlibBGRA, Pixels: []byte{0x78, 0x9c, 0x03}},
		}},
		{Generation: 9, FrameSequence: 10, BaseFrameSequence: 8, PartIndex: 1, PartCount: 2, Rectangles: []Rectangle{framePartTestRect(2), framePartTestRect(3), framePartTestRect(4)}},
		{Generation: 1, FrameSequence: 1, Keyframe: true, PartIndex: 0, PartCount: 2, Rectangles: large},
	}
	for i, part := range cases {
		payload, err := marshal(part)
		if err != nil {
			t.Fatalf("case %d: marshal: %v", i, err)
		}
		if got, want := framePartPayloadSize(part), uint64(len(payload)); got != want {
			t.Fatalf("case %d: arithmetic size %d != marshal size %d", i, got, want)
		}
	}
}
