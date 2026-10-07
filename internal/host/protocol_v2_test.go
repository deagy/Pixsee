package host

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"virtualdesktop/internal/damage"
	"virtualdesktop/internal/protocol"
)

// noisePixels returns a deterministic, high-entropy BGRA buffer that zlib
// cannot shrink, so an oversized v2 frame stays raw and must be split.
func noisePixels(w, h int) []byte {
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

// uniformPixels fills a capture with a per-pixel value replicated across all
// four channels, so box averages are easy to state exactly.
func uniformPixels(w, h int, at func(x, y int) byte) []byte {
	p := make([]byte, w*h*4)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := at(x, y)
			off := (y*w + x) * 4
			p[off], p[off+1], p[off+2], p[off+3] = v, v, v, v
		}
	}
	return p
}

func TestDownscaleFactorSmallestPowerOfTwo(t *testing.T) {
	budget := damage.MaxRectangleBytes
	cases := []struct {
		w, h uint32
		want uint32
	}{
		{4, 4, 0},       // already inside budget
		{3000, 1500, 1}, // 18 MB > budget, 1500x750 fits
		{8192, 8192, 3}, // 256 MB needs a 2^3 reduction
	}
	for _, tc := range cases {
		k := downscaleFactor(tc.w, tc.h, budget)
		if k != tc.want {
			t.Fatalf("downscaleFactor(%d,%d)=%d want %d", tc.w, tc.h, k, tc.want)
		}
		if uint64(tc.w>>k)*uint64(tc.h>>k)*4 > budget {
			t.Fatalf("factor %d does not fit budget for %dx%d", k, tc.w, tc.h)
		}
		if k > 0 && uint64(tc.w>>(k-1))*uint64(tc.h>>(k-1))*4 <= budget {
			t.Fatalf("factor %d is not the smallest for %dx%d", k, tc.w, tc.h)
		}
	}
}

func TestDownscaleImageBoxFilterReference(t *testing.T) {
	// 2x2 with distinct channel values -> k=1 -> one 1x1 average, rounded.
	src := damage.Image{Width: 2, Height: 2, Pixels: []byte{
		10, 20, 30, 40,
		50, 60, 70, 80,
		90, 100, 110, 120,
		130, 140, 150, 160,
	}}
	got := downscaleImage(src, 1)
	want := damage.Image{Width: 1, Height: 1, Pixels: []byte{70, 80, 90, 100}}
	if got.Width != want.Width || got.Height != want.Height || !equalBytes(got.Pixels, want.Pixels) {
		t.Fatalf("2x2 downscale = %v, want %v", got, want)
	}

	// Odd 5x3 with value 10x+y: k=1 -> 2x1, the trailing column/row dropped.
	odd := damage.Image{Width: 5, Height: 3, Pixels: uniformPixels(5, 3, func(x, y int) byte {
		return byte(10*x + y)
	})}
	got = downscaleImage(odd, 1)
	if got.Width != 2 || got.Height != 1 {
		t.Fatalf("5x3 downscale dims = %dx%d, want 2x1", got.Width, got.Height)
	}
	// Block (0,0): cols 0-1 rows 0-1 -> {0,1,10,11} avg 5.5 -> 6.
	// Block (1,0): cols 2-3 rows 0-1 -> {20,21,30,31} avg 25.5 -> 26.
	want = damage.Image{Width: 2, Height: 1, Pixels: []byte{6, 6, 6, 6, 26, 26, 26, 26}}
	if !equalBytes(got.Pixels, want.Pixels) {
		t.Fatalf("5x3 downscale = %v, want %v", got.Pixels, want.Pixels)
	}
}

func TestRemapCoordinateCornersEdgesAndIdentity(t *testing.T) {
	cases := []struct {
		name               string
		value, adv, native uint32
		want               uint32
	}{
		{"origin", 0, 1500, 3000, 0},
		{"exact 2x last pixel", 1499, 1500, 3000, 2999},
		{"exact 2x bottom-left", 749, 750, 1500, 1499},
		{"odd up 3->8 origin", 0, 3, 8, 0},
		{"odd up 3->8 mid", 1, 3, 8, 4},
		{"odd up 3->8 last", 2, 3, 8, 7},
		{"odd down 7->3 mid", 3, 7, 3, 1},
		{"odd down 7->3 last", 6, 7, 3, 2},
		{"native one", 4, 7, 1, 0},
		{"advertised one", 0, 1, 100, 0},
		{"advertised zero", 0, 0, 100, 0},
		{"identity zero", 0, 100, 100, 0},
		{"identity edge", 99, 100, 100, 99},
		{"clamped", 250, 100, 100, 99},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := remapCoordinate(tc.value, tc.adv, tc.native); got != tc.want {
				t.Fatalf("remap(%d,%d,%d)=%d want %d", tc.value, tc.adv, tc.native, got, tc.want)
			}
		})
	}
}

// TestRemapCoordinateEndpointPreservingAndMonotonic pins the two properties the
// endpoint-preserving remap guarantees for every advertised/native pair: the
// last advertised coordinate lands exactly on the last native coordinate, and
// the mapping never decreases. Both are checked across odd and even dimensions.
func TestRemapCoordinateEndpointPreservingAndMonotonic(t *testing.T) {
	for _, adv := range []uint32{1, 2, 3, 4, 5, 7, 16, 100, 1500} {
		for _, native := range []uint32{1, 2, 3, 4, 6, 8, 17, 99, 3000} {
			// A single advertised coordinate is degenerate: its only value, 0,
			// is both the origin and the endpoint, so it stays at the origin.
			endpoint := native - 1
			if adv <= 1 {
				endpoint = 0
			}
			if got := remapCoordinate(adv-1, adv, native); got != endpoint {
				t.Fatalf("remap(%d,%d,%d)=%d, want endpoint %d", adv-1, adv, native, got, endpoint)
			}
			prev := uint32(0)
			for v := uint32(0); v < adv; v++ {
				got := remapCoordinate(v, adv, native)
				if got < prev {
					t.Fatalf("remap(%d,%d,%d)=%d decreased from %d", v, adv, native, got, prev)
				}
				prev = got
			}
		}
	}
}

// TestV1SessionDownscalesDisplayAndRemapsPointer proves the v1 compatibility
// path: an oversized capture is advertised downscaled, only ordinary FRAMEs go
// out, and a pointer reported in the advertised space lands at the native
// coordinate.
func TestV1SessionDownscalesDisplayAndRemapsPointer(t *testing.T) {
	native := damage.Image{Width: 3000, Height: 1500, Pixels: solidPixels(3000, 1500, 1)}
	in := make(chan protocol.Message, 1)
	in <- protocol.PointerMove{Generation: 1, InputSequence: 1, X: 1499, Y: 749}
	close(in)
	input := &fakeInput{}
	cfg := testConfig() // ProtocolVersion zero -> v1
	peer := &fakePeer{incoming: in}
	if err := NewService(cfg, &fakeCapture{frames: []damage.Image{native}}, input).Run(context.Background(), peer); !errors.Is(err, io.EOF) {
		t.Fatalf("Run error=%v", err)
	}

	var display *protocol.DisplayConfig
	var frameParts int
	for _, m := range peer.messages() {
		switch v := m.(type) {
		case protocol.DisplayConfig:
			c := v
			display = &c
		case protocol.FramePart:
			frameParts++
		}
	}
	if display == nil || display.Width != 1500 || display.Height != 750 {
		t.Fatalf("v1 display config = %#v, want 1500x750", display)
	}
	if frameParts != 0 {
		t.Fatalf("v1 session emitted %d FRAME_PARTs", frameParts)
	}
	calls, _ := input.snapshot()
	if len(calls) != 1 || calls[0] != (inputCall{"move", 2999, 1499}) {
		t.Fatalf("v1 pointer remap calls=%#v, want move 2999,1499", calls)
	}
}

// TestV2SessionKeepsNativeAndEmitsFrameParts proves the v2 path: native
// dimensions are advertised, an oversized keyframe is split into ordered
// FRAME_PARTs, no plain FRAME is sent, and pointer coordinates are unchanged.
func TestV2SessionKeepsNativeAndEmitsFrameParts(t *testing.T) {
	native := damage.Image{Width: 2100, Height: 2000, Pixels: noisePixels(2100, 2000)}
	in := make(chan protocol.Message, 1)
	in <- protocol.PointerMove{Generation: 1, InputSequence: 1, X: 2099, Y: 1999}
	close(in)
	input := &fakeInput{}
	cfg := testConfig()
	cfg.ProtocolVersion = protocol.Version2
	peer := &fakePeer{incoming: in}
	if err := NewService(cfg, &fakeCapture{frames: []damage.Image{native}}, input).Run(context.Background(), peer); !errors.Is(err, io.EOF) {
		t.Fatalf("Run error=%v", err)
	}

	var display *protocol.DisplayConfig
	var parts []protocol.FramePart
	var plainFrames int
	for _, m := range peer.messages() {
		switch v := m.(type) {
		case protocol.DisplayConfig:
			c := v
			display = &c
		case protocol.Frame:
			plainFrames++
		case protocol.FramePart:
			parts = append(parts, v)
		}
	}
	if display == nil || display.Width != 2100 || display.Height != 2000 {
		t.Fatalf("v2 display config = %#v, want 2100x2000 native", display)
	}
	if plainFrames != 0 {
		t.Fatalf("v2 oversized keyframe was sent as %d plain FRAMEs", plainFrames)
	}
	if len(parts) < 2 {
		t.Fatalf("v2 parts=%d want at least 2", len(parts))
	}
	for i, p := range parts {
		if p.PartIndex != uint16(i) || p.PartCount != uint16(len(parts)) {
			t.Fatalf("part %d index/count = %d/%d, want %d/%d", i, p.PartIndex, p.PartCount, i, len(parts))
		}
		if !p.Keyframe {
			t.Fatalf("part %d dropped the keyframe flag", i)
		}
	}
	calls, _ := input.snapshot()
	if len(calls) != 1 || calls[0] != (inputCall{"move", 2099, 1999}) {
		t.Fatalf("v2 pointer calls=%#v, want move 2099,1999", calls)
	}
}

// TestV2SplitterErrorIsReported proves finding M2: a keyframe the configured
// MaxFrameParts cannot carry ends the session with a clear error naming the
// limiting value, and — critically — does so BEFORE any DISPLAY_CONFIG reaches
// the peer, so the client never sees a display announced with no frame that
// could complete it.
func TestV2SplitterErrorIsReported(t *testing.T) {
	native := damage.Image{Width: 2100, Height: 2000, Pixels: noisePixels(2100, 2000)}
	cfg := testConfig()
	cfg.ProtocolVersion = protocol.Version2
	cfg.Limits = protocol.Limits{MaxFrameParts: 1}
	peer := &fakePeer{incoming: make(chan protocol.Message)}
	err := NewService(cfg, &fakeCapture{frames: []damage.Image{native}}, &fakeInput{}).Run(context.Background(), peer)
	if !errors.Is(err, protocol.ErrFrameTooLarge) {
		t.Fatalf("Run error=%v, want protocol.ErrFrameTooLarge", err)
	}
	if !strings.Contains(err.Error(), "MaxFrameParts=1") {
		t.Fatalf("Run error=%q does not name the limiting MaxFrameParts=1", err)
	}
	if len(peer.messages()) != 0 {
		t.Fatalf("peer received %#v before the infeasible keyframe was detected; want no DISPLAY_CONFIG", peer.messages())
	}
}

// TestInitialKeyframeRectangleLimitFailsBeforeDisplayConfig covers the other
// half of the M2 feasibility check: a keyframe whose banded rectangle count
// exceeds the configured MaxRectangles is refused before DISPLAY_CONFIG, and
// the error names that limit. Forcing a tiny per-rectangle byte budget makes a
// 512x512 capture band into far more rectangles than the reduced cap.
func TestInitialKeyframeRectangleLimitFailsBeforeDisplayConfig(t *testing.T) {
	native := damage.Image{Width: 512, Height: 512, Pixels: noisePixels(512, 512)}
	cfg := testConfig()
	cfg.ProtocolVersion = protocol.Version2
	cfg.Limits = protocol.Limits{MaxRectangles: 1}
	cfg.Damage.MaxRectangleBytes = 64
	peer := &fakePeer{incoming: make(chan protocol.Message)}
	err := NewService(cfg, &fakeCapture{frames: []damage.Image{native}}, &fakeInput{}).Run(context.Background(), peer)
	if !errors.Is(err, damage.ErrRectLimit) {
		t.Fatalf("Run error=%v, want damage.ErrRectLimit", err)
	}
	if !strings.Contains(err.Error(), "MaxRectangles=1") {
		t.Fatalf("Run error=%q does not name the limiting MaxRectangles=1", err)
	}
	if len(peer.messages()) != 0 {
		t.Fatalf("peer received %#v before the infeasible keyframe was detected; want no DISPLAY_CONFIG", peer.messages())
	}
}

// TestV2DefaultLimitsSupport4KAnd8K proves the other side of M2: the default
// (hard-cap) limits still admit an initial keyframe at the resolutions the host
// is expected to serve, so neither 4K nor 8K trips the feasibility check. A
// default-configured v2 host must announce its display and deliver a first
// visual message for both.
func TestV2DefaultLimitsSupport4KAnd8K(t *testing.T) {
	cases := []struct {
		name string
		w, h int
	}{
		{"4K", 3840, 2160},
		{"8K", 7680, 4320},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			native := damage.Image{Width: uint32(tc.w), Height: uint32(tc.h), Pixels: solidPixels(tc.w, tc.h, 0x40)}
			cfg := testConfig()
			cfg.ProtocolVersion = protocol.Version2
			// A long capture interval confines the run to the synchronous
			// initial keyframe; a closed input channel then ends it with io.EOF.
			cfg.CaptureInterval = time.Hour
			in := make(chan protocol.Message)
			close(in)
			peer := &fakePeer{incoming: in}
			if err := NewService(cfg, &fakeCapture{frames: []damage.Image{native}}, &fakeInput{}).Run(context.Background(), peer); !errors.Is(err, io.EOF) {
				t.Fatalf("Run error=%v, want io.EOF (initial keyframe must fit the default limits)", err)
			}
			var display *protocol.DisplayConfig
			var visual bool
			var rects int
			for _, m := range peer.messages() {
				switch v := m.(type) {
				case protocol.DisplayConfig:
					c := v
					display = &c
				case protocol.Frame:
					visual = true
					rects += len(v.Rectangles)
				case protocol.FramePart:
					visual = true
					rects += len(v.Rectangles)
				}
			}
			if display == nil || int(display.Width) != tc.w || int(display.Height) != tc.h {
				t.Fatalf("display config=%#v, want %dx%d native", display, tc.w, tc.h)
			}
			if !visual {
				t.Fatalf("no FRAME or FRAME_PART delivered for %s", tc.name)
			}
			if want := int(protocol.MaxRectanglesHard); rects == 0 || rects > want {
				t.Fatalf("%s keyframe carried %d rectangles, want 1..%d", tc.name, rects, want)
			}
		})
	}
}

// TestV1ReducedPixelBudgetDownscalesAndFits proves the damage budget tracks the
// configured wire limit: with an 1 MiB pixel-payload cap the 3000x1500 v1
// capture is downscaled — and advertised — far enough that the emitted FRAME
// validates under that same cap, instead of being sized for the 16 MiB default
// and rejected on the wire.
func TestV1ReducedPixelBudgetDownscalesAndFits(t *testing.T) {
	limits := protocol.Limits{MaxPixelPayload: 1 << 20, MaxRectangles: 16}
	native := damage.Image{Width: 3000, Height: 1500, Pixels: solidPixels(3000, 1500, 1)}
	cfg := testConfig() // ProtocolVersion zero -> v1
	cfg.Limits = limits
	// A long capture interval keeps the run to the synchronous initial
	// keyframe; a closed input channel then ends the session with io.EOF.
	cfg.CaptureInterval = time.Hour
	in := make(chan protocol.Message)
	close(in)
	peer := &fakePeer{incoming: in}
	if err := NewService(cfg, &fakeCapture{frames: []damage.Image{native}}, &fakeInput{}).Run(context.Background(), peer); !errors.Is(err, io.EOF) {
		t.Fatalf("Run error=%v", err)
	}

	var display *protocol.DisplayConfig
	var frames, parts int
	for _, m := range peer.messages() {
		switch v := m.(type) {
		case protocol.DisplayConfig:
			c := v
			display = &c
		case protocol.Frame:
			frames++
			if err := protocol.ValidateMessage(v, limits); err != nil {
				t.Fatalf("emitted FRAME rejected under reduced limits: %v", err)
			}
		case protocol.FramePart:
			parts++
		}
	}
	if display == nil || display.Width != 375 || display.Height != 187 {
		t.Fatalf("v1 reduced-limit display config = %#v, want 375x187", display)
	}
	if frames != 1 || parts != 0 {
		t.Fatalf("v1 emitted %d FRAMEs and %d FRAME_PARTs, want 1 and 0", frames, parts)
	}
}

// TestV2ReducedPixelBudgetBandsAndSplitsFits proves the same normalization on
// v2: a reduced pixel budget bands the keyframe below that budget and the
// resulting frame splits into FRAME_PARTs that each validate under it.
func TestV2ReducedPixelBudgetBandsAndSplitsFits(t *testing.T) {
	limits := protocol.Limits{MaxPixelPayload: 1 << 20, MaxRectangles: 8}
	native := damage.Image{Width: 1024, Height: 1024, Pixels: noisePixels(1024, 1024)}
	cfg := testConfig()
	cfg.ProtocolVersion = protocol.Version2
	cfg.Limits = limits
	cfg.CaptureInterval = time.Hour
	in := make(chan protocol.Message)
	close(in)
	peer := &fakePeer{incoming: in}
	if err := NewService(cfg, &fakeCapture{frames: []damage.Image{native}}, &fakeInput{}).Run(context.Background(), peer); !errors.Is(err, io.EOF) {
		t.Fatalf("Run error=%v", err)
	}

	var parts []protocol.FramePart
	var plainFrames int
	for _, m := range peer.messages() {
		switch v := m.(type) {
		case protocol.Frame:
			plainFrames++
		case protocol.FramePart:
			parts = append(parts, v)
			if err := protocol.ValidateMessage(v, limits); err != nil {
				t.Fatalf("emitted FRAME_PART rejected under reduced limits: %v", err)
			}
		}
	}
	if plainFrames != 0 {
		t.Fatalf("v2 reduced-limit keyframe was sent as %d plain FRAMEs", plainFrames)
	}
	if len(parts) < 2 {
		t.Fatalf("v2 reduced-limit parts=%d, want at least 2", len(parts))
	}
}

// TestReducedPixelBudgetFailsClearlyWhenOnePixelCannotFit proves a pixel budget
// too small for even a one-pixel frame is reported before any capture is
// attempted, instead of emitting a frame the codec would reject.
func TestReducedPixelBudgetFailsClearlyWhenOnePixelCannotFit(t *testing.T) {
	cfg := testConfig()
	cfg.Limits = protocol.Limits{MaxPixelPayload: 8}
	err := NewService(cfg, &fakeCapture{frames: []damage.Image{testImage(1)}}, &fakeInput{}).
		Run(context.Background(), &fakePeer{incoming: make(chan protocol.Message)})
	if !errors.Is(err, damage.ErrPayloadBudget) {
		t.Fatalf("Run error=%v, want damage.ErrPayloadBudget", err)
	}
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
