package damage

import (
	"bytes"
	"errors"
	"testing"

	"virtualdesktop/internal/protocol"
)

// incompressiblePixels returns a deterministic, high-entropy BGRA buffer that
// zlib cannot shrink, so banded rectangles stay raw and exercise the size caps.
func incompressiblePixels(w, h int) []byte {
	p := make([]byte, w*h*4)
	var s uint64 = 0x9e3779b97f4a7c15
	for i := range p {
		s ^= s << 13
		s ^= s >> 7
		s ^= s << 17
		p[i] = byte(s)
	}
	return p
}

// overlap reports whether two rectangles share any pixel.
func overlap(a, b protocol.Rectangle) bool {
	return a.X < b.X+b.Width && b.X < a.X+a.Width && a.Y < b.Y+b.Height && b.Y < a.Y+a.Height
}

// assertBands checks that bands tile the expected region exactly: no overlap,
// full coverage of regionW x regionH anchored at (0,0), and every band within
// the uncompressed budget.
func assertBands(t *testing.T, bands []protocol.Rectangle, regionW, regionH uint32, budget uint64) {
	t.Helper()
	covered := map[[2]uint32]bool{}
	var area uint64
	for i, b := range bands {
		if uint64(b.Width)*uint64(b.Height)*4 > budget {
			t.Fatalf("band %d uncompressed %d exceeds budget %d", i, uint64(b.Width)*uint64(b.Height)*4, budget)
		}
		area += uint64(b.Width) * uint64(b.Height)
		for _, other := range bands[:i] {
			if overlap(b, other) {
				t.Fatalf("band %d overlaps %#v", i, other)
			}
		}
		for y := b.Y; y < b.Y+b.Height; y++ {
			for x := b.X; x < b.X+b.Width; x++ {
				covered[[2]uint32{x, y}] = true
			}
		}
	}
	if area != uint64(regionW)*uint64(regionH) || len(covered) != int(regionW)*int(regionH) {
		t.Fatalf("coverage area=%d unique=%d want %d", area, len(covered), uint64(regionW)*uint64(regionH))
	}
}

func TestBandRowsChoosesLargestRowsWithinBudget(t *testing.T) {
	// 32-byte budget / (8-wide row = 32 bytes) = 1 row.
	if got := bandRows(8, 32); got != 1 {
		t.Fatalf("bandRows(8,32)=%d want 1", got)
	}
	// 32-byte budget / (2-wide row = 8 bytes) = 4 rows.
	if got := bandRows(2, 32); got != 4 {
		t.Fatalf("bandRows(2,32)=%d want 4", got)
	}
	// A degenerate zero-width rectangle still yields one row.
	if got := bandRows(0, 32); got != 1 {
		t.Fatalf("bandRows(0,32)=%d want 1", got)
	}
}

func TestEncodeRectBandedCoverageSizeAndNonOverlap(t *testing.T) {
	img := Image{Width: 8, Height: 8, Pixels: solid(8, 8, 3)}
	d := NewDetector(Config{TileSize: 1, MaxRectangleBytes: 32, MaxRectangles: 16})
	// A 8x2 rectangle bands into two 8x1 rows under the 32-byte budget.
	bands := d.encodeRectBanded(img, rect{0, 0, 8, 2})
	if len(bands) != 2 {
		t.Fatalf("bands=%d want 2", len(bands))
	}
	assertBands(t, bands, 8, 2, d.config.MaxRectangleBytes)
	// Bands are contiguous rows of the source, in order.
	if bands[0].Y != 0 || bands[0].Height != 1 || bands[1].Y != 1 || bands[1].Height != 1 {
		t.Fatalf("unexpected band geometry: %#v", bands)
	}
	// A rectangle that already fits is encoded whole (no behavior change).
	whole := d.encodeRectBanded(img, rect{0, 0, 8, 1})
	if len(whole) != 1 || whole[0].Width != 8 || whole[0].Height != 1 {
		t.Fatalf("small rect was banded: %#v", whole)
	}
}

func TestEncodeRectsRejectsWhenBandingExceedsLimit(t *testing.T) {
	img := Image{Width: 8, Height: 8, Pixels: solid(8, 8, 1)}
	d := NewDetector(Config{TileSize: 1, MaxRectangleBytes: 32, MaxRectangles: 4})
	// Three 8x2 rectangles each band into two rows: six bands exceed the limit
	// of four, so encodeRects must report failure for a keyframe fallback.
	if _, ok := d.encodeRects(img, []rect{{0, 0, 8, 2}, {0, 3, 8, 2}, {0, 6, 8, 2}}); ok {
		t.Fatal("band expansion past MaxRectangles was accepted")
	}
	// Within the limit the same shapes encode and band correctly.
	got, ok := d.encodeRects(img, []rect{{0, 0, 8, 2}, {0, 3, 8, 1}})
	if !ok || len(got) != 3 {
		t.Fatalf("within-limit encode: ok=%v bands=%d want 3", ok, len(got))
	}
	assertBands(t, got, 8, 3, d.config.MaxRectangleBytes)
}

// TestCompareBandsTallDirtyRectangle proves delta banding end to end: a single
// dirty rectangle taller than the per-row band height is split, keeps exact
// coverage, and stays a delta (not a keyframe).
func TestCompareBandsTallDirtyRectangle(t *testing.T) {
	d := NewDetector(Config{TileSize: 1, DirtyRatio: 0.9, MaxRectangleBytes: 32, MaxRectangles: 16})
	base := Image{Width: 8, Height: 16, Pixels: solid(8, 16, 1)}
	if _, _, err := d.Compare(base, false); err != nil {
		t.Fatal(err)
	}
	next := append([]byte(nil), base.Pixels...)
	// Dirty columns 0-1 for every row: a single 2x16 rectangle that bands into
	// four 2x4 rows under the 32-byte budget.
	for y := 0; y < 16; y++ {
		for x := 0; x < 2; x++ {
			next[(y*8+x)*4] = 200
		}
	}
	frame, changed, err := d.Compare(Image{Width: 8, Height: 16, Pixels: next}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || frame.Keyframe {
		t.Fatalf("want a delta frame, got %#v", frame)
	}
	if len(frame.Rectangles) != 4 {
		t.Fatalf("bands=%d want 4", len(frame.Rectangles))
	}
	assertBands(t, frame.Rectangles, 2, 16, d.config.MaxRectangleBytes)
}

// TestCompareFallsBackToKeyframeWhenDirtyRectanglesExceedLimit pins the
// configured rectangle-limit fallback with banding in play: more dirty
// rectangles than MaxRectangles forces a keyframe, and that keyframe is itself
// banded to stay within the per-rectangle budget.
func TestCompareFallsBackToKeyframeWhenDirtyRectanglesExceedLimit(t *testing.T) {
	d := NewDetector(Config{TileSize: 1, MaxRectangleBytes: 32, MaxRectangles: 16})
	base := Image{Width: 8, Height: 16, Pixels: solid(8, 16, 1)}
	if _, _, err := d.Compare(base, false); err != nil {
		t.Fatal(err)
	}
	next := append([]byte(nil), base.Pixels...)
	// Four isolated single-pixel dirty runs per row over sixteen rows produce
	// far more than sixteen rectangles.
	for y := 0; y < 16; y++ {
		for _, x := range []int{0, 2, 4, 6} {
			next[(y*8+x)*4] = 200
		}
	}
	frame, changed, err := d.Compare(Image{Width: 8, Height: 16, Pixels: next}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || !frame.Keyframe {
		t.Fatalf("want a keyframe fallback, got changed=%v frame=%#v", changed, frame)
	}
	if len(frame.Rectangles) != 16 {
		t.Fatalf("banded keyframe rectangles=%d want 16", len(frame.Rectangles))
	}
	assertBands(t, frame.Rectangles, 8, 16, d.config.MaxRectangleBytes)
}

// TestCompareKeyframeExceedsRectLimitErrors pins the terminal case: when even
// the banded keyframe cannot fit the configured rectangle limit, Compare
// reports ErrRectLimit rather than emitting an invalid frame.
func TestCompareKeyframeExceedsRectLimitErrors(t *testing.T) {
	d := NewDetector(Config{TileSize: 1, MaxRectangleBytes: 32, MaxRectangles: 4})
	img := Image{Width: 8, Height: 16, Pixels: solid(8, 16, 1)}
	if _, _, err := d.Compare(img, false); !errors.Is(err, ErrRectLimit) {
		t.Fatalf("error=%v want ErrRectLimit", err)
	}
}

// TestKeyframeBandsIncompressibleHighResUnderBudget is the headline banding
// test: a 3840x2400 incompressible capture is emitted as a keyframe whose every
// rectangle stays under the decoded per-rectangle cap, and whose rectangles
// split into FRAME_PARTs that each fit the 16 MiB wire budget with exact whole
// coverage.
func TestKeyframeBandsIncompressibleHighResUnderBudget(t *testing.T) {
	const w, h = 3840, 2400
	pixels := incompressiblePixels(w, h)
	d := NewDetector(Config{})
	frame, changed, err := d.Compare(Image{Width: w, Height: h, Pixels: pixels}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || !frame.Keyframe {
		t.Fatalf("want a keyframe, got changed=%v frame=%#v", changed, frame)
	}
	if len(frame.Rectangles) < 2 {
		t.Fatalf("expected a banded keyframe, got %d rectangles", len(frame.Rectangles))
	}
	for i, r := range frame.Rectangles {
		if r.Encoding != protocol.EncodingRawBGRA {
			t.Fatalf("rectangle %d: incompressible capture was compressed (%v)", i, r.Encoding)
		}
		if uint64(r.Width)*uint64(r.Height)*4 > MaxRectangleBytes {
			t.Fatalf("rectangle %d uncompressed %d exceeds cap %d", i, uint64(r.Width)*uint64(r.Height)*4, MaxRectangleBytes)
		}
	}
	// Reconstruct the image from the bands to prove exact coverage.
	canvas := make([]byte, w*h*4)
	for _, r := range frame.Rectangles {
		dec := decodedPixels(t, r)
		rowBytes := int(r.Width) * 4
		for y := 0; y < int(r.Height); y++ {
			dst := ((int(r.Y)+y)*w + int(r.X)) * 4
			copy(canvas[dst:dst+rowBytes], dec[y*rowBytes:(y+1)*rowBytes])
		}
	}
	if !bytes.Equal(canvas, pixels) {
		t.Fatal("banded keyframe does not reconstruct the capture")
	}

	// The v2 splitter must turn the bands into ordered parts that each validate
	// under the 16 MiB pixel-payload budget.
	parts, err := protocol.SplitFrame(frame, protocol.DefaultLimits())
	if err != nil {
		t.Fatalf("SplitFrame: %v", err)
	}
	if len(parts) < 2 {
		t.Fatalf("parts=%d want at least 2", len(parts))
	}
	var reassembled []protocol.Rectangle
	for i, p := range parts {
		if p.PartIndex != uint16(i) || p.PartCount != uint16(len(parts)) {
			t.Fatalf("part %d has index/count %d/%d", i, p.PartIndex, p.PartCount)
		}
		if err := protocol.ValidateMessage(p, protocol.DefaultLimits()); err != nil {
			t.Fatalf("part %d invalid: %v", i, err)
		}
		reassembled = append(reassembled, p.Rectangles...)
	}
	if len(reassembled) != len(frame.Rectangles) {
		t.Fatalf("reassembled %d rectangles, want %d", len(reassembled), len(frame.Rectangles))
	}
}

// TestNewDetectorWithLimitsClampsBudgetAndCount proves the detector's rectangle
// byte budget and rectangle count are clamped to the active wire limits, so a
// reduced limit cannot produce a frame the wire rejects. The 1024x1024
// incompressible capture exceeds the reduced budget, so the keyframe bands into
// rectangles that each fit it (within the reduced count), and those rectangles
// split into parts that each validate under the same limits.
func TestNewDetectorWithLimitsClampsBudgetAndCount(t *testing.T) {
	limits := protocol.Limits{MaxPixelPayload: 1 << 20, MaxRectangles: 8}
	d, err := NewDetectorWithLimits(Config{}, limits)
	if err != nil {
		t.Fatalf("NewDetectorWithLimits: %v", err)
	}
	if want := uint64(1<<20) - frameWireFloor; d.config.MaxRectangleBytes != want {
		t.Fatalf("budget = %d, want %d", d.config.MaxRectangleBytes, want)
	}
	if d.config.MaxRectangles != 8 {
		t.Fatalf("MaxRectangles = %d, want 8", d.config.MaxRectangles)
	}

	img := Image{Width: 1024, Height: 1024, Pixels: incompressiblePixels(1024, 1024)}
	frame, changed, err := d.Compare(img, false)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || !frame.Keyframe {
		t.Fatalf("want a keyframe, got changed=%v frame=%#v", changed, frame)
	}
	if len(frame.Rectangles) < 2 || len(frame.Rectangles) > 8 {
		t.Fatalf("rectangles=%d, want 2..8", len(frame.Rectangles))
	}
	for i, r := range frame.Rectangles {
		if uint64(r.Width)*uint64(r.Height)*4 > d.config.MaxRectangleBytes {
			t.Fatalf("rectangle %d uncompressed size exceeds the reduced budget", i)
		}
	}
	parts, err := protocol.SplitFrame(frame, limits)
	if err != nil {
		t.Fatalf("SplitFrame: %v", err)
	}
	if len(parts) < 2 {
		t.Fatalf("parts=%d, want at least 2", len(parts))
	}
	for i, p := range parts {
		if err := protocol.ValidateMessage(p, limits); err != nil {
			t.Fatalf("part %d invalid under reduced limits: %v", i, err)
		}
	}
}

// TestNewDetectorWithDefaultLimitsPreservesGlobalCaps pins that default limits
// leave the historical budget and count untouched, so the wire clamp never
// changes default behavior.
func TestNewDetectorWithDefaultLimitsPreservesGlobalCaps(t *testing.T) {
	d, err := NewDetectorWithLimits(Config{}, protocol.DefaultLimits())
	if err != nil {
		t.Fatalf("NewDetectorWithLimits: %v", err)
	}
	if d.config.MaxRectangleBytes != MaxRectangleBytes {
		t.Fatalf("budget = %d, want default %d", d.config.MaxRectangleBytes, MaxRectangleBytes)
	}
	if d.config.MaxRectangles != protocol.MaxRectanglesHard {
		t.Fatalf("MaxRectangles = %d, want %d", d.config.MaxRectangles, protocol.MaxRectanglesHard)
	}
}

// TestNewDetectorWithLimitsRejectsImpossibleBudget pins the terminal floor: a
// pixel-payload limit that cannot carry even a one-pixel frame is reported as a
// configuration error, not a detector that later emits an invalid frame.
func TestNewDetectorWithLimitsRejectsImpossibleBudget(t *testing.T) {
	limits := protocol.Limits{MaxPixelPayload: uint32(frameWireFloor) - 1}
	if _, err := NewDetectorWithLimits(Config{}, limits); !errors.Is(err, ErrPayloadBudget) {
		t.Fatalf("error = %v, want ErrPayloadBudget", err)
	}
}
