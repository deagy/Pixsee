package client

import (
	"bytes"
	"encoding/binary"
	"testing"

	"virtualdesktop/internal/protocol"
)

// The fuzz script is a compact sequence of FRAME_PART descriptors. Rectangle
// pixel bytes are drawn from a separate pool argument (zero-filled when the
// pool runs out), so a script can describe many near-valid parts without the
// fuzzer having to guess exact raw-pixel lengths.
//
//	step  (30 bytes): generation u64 | frameSequence u64 | baseSequence u64 |
//	                  flags u8 | partIndex u16 | partCount u16 | rectCount u8
//	rect  (16 bytes): x u32 | y u32 | width u32 | height u32
const (
	fuzzStepSize = 30
	fuzzRectSize = 16
	fuzzMaxSteps = 8
	fuzzMaxRects = 2
)

// encodeFuzzScript serializes parts into the fuzz script format. It is only
// used to build seed corpus entries from valid split frames and their
// structural mutations.
func encodeFuzzScript(parts []protocol.FramePart) []byte {
	var script []byte
	for _, part := range parts {
		step := make([]byte, fuzzStepSize)
		binary.BigEndian.PutUint64(step[0:8], part.Generation)
		binary.BigEndian.PutUint64(step[8:16], part.FrameSequence)
		binary.BigEndian.PutUint64(step[16:24], part.BaseFrameSequence)
		if part.Keyframe {
			step[24] = 1
		}
		binary.BigEndian.PutUint16(step[25:27], part.PartIndex)
		binary.BigEndian.PutUint16(step[27:29], part.PartCount)
		rects := part.Rectangles
		if len(rects) > fuzzMaxRects {
			rects = rects[:fuzzMaxRects]
		}
		step[29] = byte(len(rects))
		script = append(script, step...)
		for _, rect := range rects {
			buf := make([]byte, fuzzRectSize)
			binary.BigEndian.PutUint32(buf[0:4], rect.X)
			binary.BigEndian.PutUint32(buf[4:8], rect.Y)
			binary.BigEndian.PutUint32(buf[8:12], rect.Width)
			binary.BigEndian.PutUint32(buf[12:16], rect.Height)
			script = append(script, buf...)
		}
	}
	return script
}

// decodeFuzzParts interprets a script (bounded to fuzzMaxSteps parts, each with
// at most fuzzMaxRects rectangles) into FRAME_PARTS whose geometry always fits
// a 4x2 display and whose raw pixels are drawn from the pool.
func decodeFuzzParts(script, pool []byte) []protocol.FramePart {
	var parts []protocol.FramePart
	pos, poolAt := 0, 0
	for len(parts) < fuzzMaxSteps && pos+fuzzStepSize <= len(script) {
		step := script[pos : pos+fuzzStepSize]
		pos += fuzzStepSize
		part := protocol.FramePart{
			Generation:        binary.BigEndian.Uint64(step[0:8]),
			FrameSequence:     binary.BigEndian.Uint64(step[8:16]),
			BaseFrameSequence: binary.BigEndian.Uint64(step[16:24]),
			Keyframe:          step[24]&1 == 1,
			PartIndex:         binary.BigEndian.Uint16(step[25:27]),
			PartCount:         binary.BigEndian.Uint16(step[27:29]),
		}
		rectCount := int(step[29] % (fuzzMaxRects + 1))
		for i := 0; i < rectCount && pos+fuzzRectSize <= len(script); i++ {
			buf := script[pos : pos+fuzzRectSize]
			pos += fuzzRectSize
			x := binary.BigEndian.Uint32(buf[0:4]) % 4
			y := binary.BigEndian.Uint32(buf[4:8]) % 2
			w := binary.BigEndian.Uint32(buf[8:12])%4 + 1
			h := binary.BigEndian.Uint32(buf[12:16])%2 + 1
			if x+w > 4 {
				w = 4 - x
			}
			if y+h > 2 {
				h = 2 - y
			}
			size := int(w*h) * 4
			pixels := make([]byte, size)
			for j := range pixels {
				if poolAt < len(pool) {
					pixels[j] = pool[poolAt]
					poolAt++
				}
			}
			part.Rectangles = append(part.Rectangles, protocol.Rectangle{
				X: x, Y: y, Width: w, Height: h,
				Encoding: protocol.EncodingRawBGRA, Pixels: pixels,
			})
		}
		parts = append(parts, part)
	}
	return parts
}

// FuzzFramebufferApplyPartSequence feeds a scripted sequence of FRAME_PARTS into
// one configured client Framebuffer. Its invariants hold for every input:
//   - a call that errors or does not commit must never change the committed
//     pixels or frame sequence (no partial present/commit);
//   - a call that commits must leave a full-size framebuffer.
//
// Seeds include a valid split keyframe sequence and mutated index/count/header
// variants, so the fuzzer starts near the interesting reassembly states.
func FuzzFramebufferApplyPartSequence(f *testing.F) {
	frame := protocol.Frame{Generation: 1, FrameSequence: 1, Keyframe: true, Rectangles: keyframeRects4x2()}
	parts, err := protocol.SplitFrame(frame, partLimits())
	if err != nil {
		f.Fatal(err)
	}
	var pool []byte
	for _, rect := range frame.Rectangles {
		pool = append(pool, rect.Pixels...)
	}
	f.Add(encodeFuzzScript(parts), pool)

	mutated := append([]protocol.FramePart(nil), parts...)
	mutated[1].PartIndex++
	f.Add(encodeFuzzScript(mutated), pool)

	mutated = append([]protocol.FramePart(nil), parts...)
	mutated[0].PartCount = 1
	f.Add(encodeFuzzScript(mutated), pool)

	mutated = append([]protocol.FramePart(nil), parts...)
	mutated[len(mutated)-1].FrameSequence = 99
	f.Add(encodeFuzzScript(mutated), pool)

	f.Add([]byte{}, []byte{})

	f.Fuzz(func(t *testing.T, script, pool []byte) {
		if len(script) > 1<<12 || len(pool) > 1<<12 {
			t.Skip()
		}
		fb := NewFramebuffer(protocol.DefaultLimits())
		if err := fb.Configure(protocol.DisplayConfig{Generation: 1, Width: 4, Height: 2, PixelFormat: protocol.PixelBGRA8888}); err != nil {
			t.Fatalf("configure: %v", err)
		}
		for _, part := range decodeFuzzParts(script, pool) {
			before := fb.Snapshot()
			committed, err := fb.ApplyPart(part)
			after := fb.Snapshot()
			if err != nil || !committed {
				if after.FrameSequence != before.FrameSequence || !bytes.Equal(after.Pixels, before.Pixels) {
					t.Fatalf("part %#v changed committed state (err=%v committed=%v)", part, err, committed)
				}
				continue
			}
			if len(after.Pixels) != 4*2*4 {
				t.Fatalf("commit produced a %d-byte framebuffer, want %d", len(after.Pixels), 4*2*4)
			}
		}
	})
}
