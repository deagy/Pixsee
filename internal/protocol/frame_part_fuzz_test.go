package protocol

import (
	"bytes"
	"reflect"
	"testing"
)

// fuzzFramePartWire builds a full record envelope for a FRAME_PART without
// validating it, so a seed can carry a structurally invalid part (bad index,
// count, header, or coverage) rather than being rejected at encode time.
func fuzzFramePartWire(t testing.TB, part FramePart, version uint16) []byte {
	t.Helper()
	payload, err := marshal(part)
	if err != nil {
		t.Fatal(err)
	}
	wire := headerBytes(Magic, version, TypeFramePart, 0, 0, uint32(len(payload)), 1)
	return append(wire, payload...)
}

// FuzzFramePartDecode fuzzes the record decoder against FRAME_PART wire bytes.
// Seeds include a valid split frame's parts plus mutations of the part index,
// part count, keyframe flag, base sequence, and rectangle coverage, and a valid
// part on a v1 envelope (which the v2 gate must reject). The decoder bounds its
// own reads (24-byte header, length-limited payload), so a bounded input cannot
// allocate unboundedly.
func FuzzFramePartDecode(f *testing.F) {
	frame := Frame{Generation: 3, FrameSequence: 5, BaseFrameSequence: 4, Rectangles: []Rectangle{
		framePartTestRect(0), framePartTestRect(1), framePartTestRect(2),
	}}
	parts, err := SplitFrame(frame, Limits{MaxPixelPayload: 61})
	if err != nil {
		f.Fatal(err)
	}
	for _, part := range parts {
		f.Add(fuzzFramePartWire(f, part, Version2))

		mutated := part
		mutated.PartIndex++
		f.Add(fuzzFramePartWire(f, mutated, Version2))

		mutated = part
		mutated.PartCount--
		f.Add(fuzzFramePartWire(f, mutated, Version2))

		mutated = part
		mutated.Keyframe = !mutated.Keyframe
		f.Add(fuzzFramePartWire(f, mutated, Version2))

		mutated = part
		mutated.BaseFrameSequence = 0
		f.Add(fuzzFramePartWire(f, mutated, Version2))

		mutated = part
		mutated.Rectangles = nil
		f.Add(fuzzFramePartWire(f, mutated, Version2))
	}
	// A structurally valid part on a v1 envelope: the decoder must reject it on
	// the version gate, not decode it.
	f.Add(fuzzFramePartWire(f, parts[0], Version1))
	// A truncated valid part and a bare magic.
	valid := fuzzFramePartWire(f, parts[0], Version2)
	f.Add(valid[:len(valid)-2])
	f.Add([]byte("VDP1"))

	f.Fuzz(func(t *testing.T, wire []byte) {
		if len(wire) > 1<<16 {
			t.Skip()
		}
		for _, version := range []uint16{Version1, Version2} {
			dec := NewDecoder(bytes.NewReader(wire), DefaultLimits())
			if version == Version2 {
				dec.PinVersion(Version2)
			}
			_, _ = dec.Decode()
		}
	})
}

// FuzzFramePartValidate fuzzes FRAME_PART validation with bounded, tiny
// geometry. When the constructed part validates, it must survive a
// marshal/unmarshal round trip byte-for-byte, so validation and the codec
// cannot disagree about what a well-formed part is.
func FuzzFramePartValidate(f *testing.F) {
	f.Add(uint64(1), uint64(2), uint64(1), true, uint16(0), uint16(2), uint32(1), uint32(0), uint16(1), uint16(1), byte(0x80))
	f.Fuzz(func(t *testing.T, generation, frameSequence, base uint64, keyframe bool, partIndex, partCount uint16, x, y uint32, w, h uint16, fill byte) {
		width := uint32(w%4) + 1
		height := uint32(h%4) + 1
		pixels := make([]byte, width*height*4)
		for i := range pixels {
			pixels[i] = fill
		}
		part := FramePart{
			Generation:        generation,
			FrameSequence:     frameSequence,
			BaseFrameSequence: base,
			Keyframe:          keyframe,
			PartIndex:         partIndex % 8,
			PartCount:         partCount % 8,
			Rectangles: []Rectangle{{
				X: x % 8, Y: y % 8, Width: width, Height: height,
				Encoding: EncodingRawBGRA, Pixels: pixels,
			}},
		}
		if err := ValidateMessage(part, DefaultLimits()); err != nil {
			return
		}
		payload, err := marshal(part)
		if err != nil {
			t.Fatalf("validated FRAME_PART failed to marshal: %v", err)
		}
		got, err := unmarshal(TypeFramePart, payload, DefaultLimits())
		if err != nil {
			t.Fatalf("validated FRAME_PART failed to unmarshal: %v", err)
		}
		if !reflect.DeepEqual(got, part) {
			t.Fatalf("FRAME_PART round trip mismatch:\n got %#v\nwant %#v", got, part)
		}
	})
}
