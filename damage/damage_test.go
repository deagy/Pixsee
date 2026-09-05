package damage

import (
	"bytes"
	"compress/zlib"
	"io"
	"testing"
	"virtualdesktop/internal/protocol"
)

func solid(width, height int, b byte) []byte {
	return bytes.Repeat([]byte{b, b, b, 0xff}, width*height)
}

func decodedPixels(t *testing.T, r protocol.Rectangle) []byte {
	t.Helper()
	if r.Encoding == protocol.EncodingRawBGRA {
		return r.Pixels
	}
	zr, err := zlib.NewReader(bytes.NewReader(r.Pixels))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	pixels, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return pixels
}

func TestDetectorEmitsInitialKeyframeAndSkipsUnchangedFrame(t *testing.T) {
	d := NewDetector(Config{TileSize: 2, DirtyRatio: .60, MaxRectangles: 256})
	pixels := solid(4, 4, 1)

	first, changed, err := d.Compare(Image{Width: 4, Height: 4, Pixels: pixels}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || !first.Keyframe || first.BaseFrameSequence != 0 || len(first.Rectangles) != 1 {
		t.Fatalf("unexpected initial frame: %#v, changed=%v", first, changed)
	}
	if !bytes.Equal(decodedPixels(t, first.Rectangles[0]), pixels) {
		t.Fatal("keyframe pixels differ")
	}

	_, changed, err = d.Compare(Image{Width: 4, Height: 4, Pixels: append([]byte(nil), pixels...)}, false)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("unchanged capture emitted an update")
	}
}

func TestDetectorCoalescesChangedTilesAndEncodesOnlyDirtyPixels(t *testing.T) {
	d := NewDetector(Config{TileSize: 2, DirtyRatio: .90, MaxRectangles: 256})
	base := solid(6, 4, 1)
	if _, _, err := d.Compare(Image{Width: 6, Height: 4, Pixels: base}, false); err != nil {
		t.Fatal(err)
	}

	next := append([]byte(nil), base...)
	// Change the two adjacent tiles on the top row; they must merge into 4x2.
	for y := 0; y < 2; y++ {
		for x := 0; x < 4; x++ {
			next[(y*6+x)*4] = 9
		}
	}
	frame, changed, err := d.Compare(Image{Width: 6, Height: 4, Pixels: next}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || frame.Keyframe || frame.BaseFrameSequence != 1 {
		t.Fatalf("unexpected delta: %#v", frame)
	}
	if len(frame.Rectangles) != 1 {
		t.Fatalf("got %d rectangles", len(frame.Rectangles))
	}
	r := frame.Rectangles[0]
	if r.X != 0 || r.Y != 0 || r.Width != 4 || r.Height != 2 {
		t.Fatalf("bad merged rectangle: %#v", r)
	}
	if r.Encoding != protocol.EncodingRawBGRA && r.Encoding != protocol.EncodingZlibBGRA {
		t.Fatalf("bad encoding %v", r.Encoding)
	}
}

func TestDetectorForcesKeyframeForDirtyThresholdGeometryChangeAndRequest(t *testing.T) {
	for _, tc := range []struct {
		name  string
		next  Image
		force bool
	}{
		{"dirty threshold", Image{Width: 4, Height: 4, Pixels: solid(4, 4, 2)}, false},
		{"geometry change", Image{Width: 2, Height: 2, Pixels: solid(2, 2, 1)}, false},
		{"request", Image{Width: 4, Height: 4, Pixels: solid(4, 4, 1)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := NewDetector(Config{TileSize: 2, DirtyRatio: .60, MaxRectangles: 256})
			if _, _, err := d.Compare(Image{Width: 4, Height: 4, Pixels: solid(4, 4, 1)}, false); err != nil {
				t.Fatal(err)
			}
			frame, changed, err := d.Compare(tc.next, tc.force)
			if err != nil {
				t.Fatal(err)
			}
			if !changed || !frame.Keyframe || frame.BaseFrameSequence != 0 || len(frame.Rectangles) != 1 {
				t.Fatalf("wanted keyframe, got %#v", frame)
			}
		})
	}
}

func TestDetectorRejectsMalformedCapture(t *testing.T) {
	d := NewDetector(Config{})
	for _, image := range []Image{
		{},
		{Width: 8193, Height: 1, Pixels: make([]byte, 4)},
		{Width: 2, Height: 2, Pixels: make([]byte, 15)},
	} {
		if _, _, err := d.Compare(image, false); err == nil {
			t.Fatalf("accepted %#v", image)
		}
	}
}
