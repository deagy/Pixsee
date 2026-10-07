package damage

import (
	"bytes"
	"compress/zlib"
	"errors"
	"fmt"
	"hash/fnv"
	"virtualdesktop/internal/protocol"
)

var ErrInvalidImage = errors.New("invalid captured image")

// ErrRectLimit reports that a keyframe cannot be represented within the
// configured rectangle limit even after banding. A keyframe is the last
// resort, so this is a configuration error (MaxRectangles too small for the
// capture) rather than something a delta fallback can resolve.
var ErrRectLimit = errors.New("damage: keyframe exceeds rectangle limit")

// MaxRectangleBytes is the default per-rectangle band budget: the largest
// uncompressed BGRA byte size a single encoded rectangle may occupy. It leaves
// 4096 bytes of headroom under the 16 MiB pixel-payload hard cap for the
// frame-part and rectangle headers that wrap a rectangle on the wire.
const MaxRectangleBytes = uint64(protocol.MaxPixelPayloadHard) - 4096

type Image struct {
	Width, Height uint32
	Pixels        []byte // packed BGRA8888
}

type Config struct {
	TileSize      uint32
	DirtyRatio    float64
	MaxRectangles uint16
	// MaxRectangleBytes bounds the uncompressed BGRA size of a single encoded
	// rectangle; a larger rectangle is split into horizontal bands so each
	// band stays within the budget (and, on v2, within one FRAME_PART). Zero or
	// an oversized value selects MaxRectangleBytes. NewDetectorWithLimits
	// further clamps it to the session's effective pixel-payload budget less
	// the frame/rectangle header margin.
	MaxRectangleBytes uint64
}

type Detector struct {
	config     Config
	previous   Image
	hashes     []uint64
	generation uint64
	sequence   uint64
}

// ErrPayloadBudget reports that the effective pixel-payload limit cannot carry
// even a one-pixel frame, so no amount of banding can make a frame fit.
var ErrPayloadBudget = errors.New("damage: pixel-payload limit cannot carry a one-pixel frame")

// frameWireFloor is the wire overhead reserved above a rectangle's pixel bytes
// so the frame/part and rectangle metadata that wrap it still fit the
// pixel-payload budget. It is 53 by construction twice over: a FRAME_PART
// header (31) plus one rectangle's metadata (22) is the largest fixed overhead
// a single rectangle can ride, and a full FRAME's header (27) plus that
// metadata plus one 1x1 raw pixel (4) is the smallest frame payload possible.
// It therefore serves both as the banding margin and as the floor below which
// no frame of any size fits.
const frameWireFloor = uint64(53)

// effectiveWireLimits bounds the two protocol limits that constrain a frame's
// rectangles, mirroring protocol.Limits' own bounding: a zero or oversized
// field falls back to the protocol hard cap, exactly as the codec, the
// validator, and SplitFrame compute it.
func effectiveWireLimits(limits protocol.Limits) (uint32, uint16) {
	bytes := limits.MaxPixelPayload
	if bytes == 0 || bytes > protocol.MaxPixelPayloadHard {
		bytes = protocol.MaxPixelPayloadHard
	}
	rectangles := limits.MaxRectangles
	if rectangles == 0 || rectangles > protocol.MaxRectanglesHard {
		rectangles = protocol.MaxRectanglesHard
	}
	return bytes, rectangles
}

// normalizeConfig applies the detector defaults and then clamps the rectangle
// byte budget and rectangle count to the effective wire limits, so the detector
// never bands a rectangle larger than, or emits more whole rectangles than, the
// active limits accept. A wire budget that leaves no banding room still yields a
// workable (one-row) configuration; NewDetectorWithLimits rejects the limits
// that cannot carry a frame at all.
func normalizeConfig(config Config, limits protocol.Limits) Config {
	if config.TileSize == 0 {
		config.TileSize = 64
	}
	if config.DirtyRatio <= 0 || config.DirtyRatio > 1 {
		config.DirtyRatio = .60
	}
	if config.MaxRectangles == 0 || config.MaxRectangles > protocol.MaxRectanglesHard {
		config.MaxRectangles = protocol.MaxRectanglesHard
	}
	if config.MaxRectangleBytes == 0 || config.MaxRectangleBytes > MaxRectangleBytes {
		config.MaxRectangleBytes = MaxRectangleBytes
	}
	wireBytes, wireRectangles := effectiveWireLimits(limits)
	if wireRectangles < config.MaxRectangles {
		config.MaxRectangles = wireRectangles
	}
	budget := uint64(0)
	if uint64(wireBytes) > frameWireFloor {
		budget = uint64(wireBytes) - frameWireFloor
	}
	if budget < config.MaxRectangleBytes {
		config.MaxRectangleBytes = budget
	}
	return config
}

// EffectiveConfig returns cfg with the detector defaults applied and its
// rectangle byte budget and count clamped to the effective wire limits, so a
// caller can size a v1 downscale from the same budget the detector bands with.
func EffectiveConfig(config Config, limits protocol.Limits) Config {
	return normalizeConfig(config, limits)
}

func NewDetector(config Config) *Detector {
	return &Detector{config: normalizeConfig(config, protocol.DefaultLimits())}
}

// NewDetectorWithLimits builds a detector whose rectangle byte budget and
// rectangle count are clamped to the active wire limits, so the frames it
// produces are ones the codec and SplitFrame accept. It returns
// ErrPayloadBudget when those limits cannot carry even a one-pixel frame.
func NewDetectorWithLimits(config Config, limits protocol.Limits) (*Detector, error) {
	wireBytes, _ := effectiveWireLimits(limits)
	if uint64(wireBytes) < frameWireFloor {
		return nil, fmt.Errorf("%w: MaxPixelPayload=%d, need >= %d", ErrPayloadBudget, wireBytes, frameWireFloor)
	}
	return &Detector{config: normalizeConfig(config, limits)}, nil
}

func (d *Detector) Compare(image Image, forceKeyframe bool) (protocol.Frame, bool, error) {
	if err := validateImage(image); err != nil {
		return protocol.Frame{}, false, err
	}
	geometryChanged := d.previous.Width != image.Width || d.previous.Height != image.Height
	if d.generation == 0 || geometryChanged {
		d.generation++
	}

	d.sequence++
	if d.previous.Pixels == nil || geometryChanged || forceKeyframe {
		frame, err := d.keyframe(image)
		if err != nil {
			return protocol.Frame{}, false, err
		}
		d.remember(image)
		return frame, true, nil
	}

	rects, dirtyPixels, hashes := d.changedRectangles(image)
	if len(rects) == 0 {
		d.sequence--
		return protocol.Frame{}, false, nil
	}
	if len(rects) > int(d.config.MaxRectangles) || float64(dirtyPixels)/float64(uint64(image.Width)*uint64(image.Height)) > d.config.DirtyRatio {
		frame, err := d.keyframe(image)
		if err != nil {
			return protocol.Frame{}, false, err
		}
		d.rememberWithHashes(image, hashes)
		return frame, true, nil
	}

	encoded, ok := d.encodeRects(image, rects)
	if !ok {
		// Banding the dirty rectangles expanded their count past the rectangle
		// limit. Fall back to a banded keyframe: it re-covers the whole image
		// with far fewer rectangles and still fits the wire.
		frame, err := d.keyframe(image)
		if err != nil {
			return protocol.Frame{}, false, err
		}
		d.rememberWithHashes(image, hashes)
		return frame, true, nil
	}
	frame := protocol.Frame{Generation: d.generation, FrameSequence: d.sequence, BaseFrameSequence: d.sequence - 1, Rectangles: encoded}
	d.rememberWithHashes(image, hashes)
	return frame, true, nil
}

func validateImage(image Image) error {
	if image.Width == 0 || image.Height == 0 || image.Width > protocol.MaxDimensionHard || image.Height > protocol.MaxDimensionHard {
		return ErrInvalidImage
	}
	expected := uint64(image.Width) * uint64(image.Height) * 4
	if expected > uint64(^uint(0)>>1) || uint64(len(image.Pixels)) != expected {
		return ErrInvalidImage
	}
	return nil
}

type rect struct{ x, y, w, h uint32 }

func (d *Detector) changedRectangles(image Image) ([]rect, uint64, []uint64) {
	ts := d.config.TileSize
	cols := (image.Width + ts - 1) / ts
	rows := (image.Height + ts - 1) / ts
	hashes := make([]uint64, cols*rows)
	dirty := make([]bool, cols*rows)
	var dirtyPixels uint64
	for ty := uint32(0); ty < rows; ty++ {
		for tx := uint32(0); tx < cols; tx++ {
			r := tileRect(tx, ty, ts, image.Width, image.Height)
			i := ty*cols + tx
			hashes[i] = hashRect(image, r)
			if int(i) >= len(d.hashes) || hashes[i] != d.hashes[i] {
				// A hash is only a candidate; compare bytes before declaring damage.
				dirty[i] = !equalRect(image, d.previous, r)
				if dirty[i] {
					dirtyPixels += uint64(r.w) * uint64(r.h)
				}
			}
		}
	}
	var runs []rect
	for ty := uint32(0); ty < rows; ty++ {
		for tx := uint32(0); tx < cols; {
			if !dirty[ty*cols+tx] {
				tx++
				continue
			}
			start := tx
			for tx < cols && dirty[ty*cols+tx] {
				tx++
			}
			x := start * ts
			end := tx * ts
			if end > image.Width {
				end = image.Width
			}
			y := ty * ts
			h := ts
			if y+h > image.Height {
				h = image.Height - y
			}
			runs = append(runs, rect{x, y, end - x, h})
		}
	}
	merged := make([]rect, 0, len(runs))
	for _, r := range runs {
		if len(merged) > 0 {
			last := &merged[len(merged)-1]
			if last.x == r.x && last.w == r.w && last.y+last.h == r.y {
				last.h += r.h
				continue
			}
		}
		merged = append(merged, r)
	}
	return merged, dirtyPixels, hashes
}

func tileRect(tx, ty, size, width, height uint32) rect {
	r := rect{x: tx * size, y: ty * size, w: size, h: size}
	if r.x+r.w > width {
		r.w = width - r.x
	}
	if r.y+r.h > height {
		r.h = height - r.y
	}
	return r
}

func hashRect(image Image, r rect) uint64 {
	h := fnv.New64a()
	for y := uint32(0); y < r.h; y++ {
		start := (uint64(r.y+y)*uint64(image.Width) + uint64(r.x)) * 4
		_, _ = h.Write(image.Pixels[start : start+uint64(r.w)*4])
	}
	return h.Sum64()
}

func equalRect(a, b Image, r rect) bool {
	for y := uint32(0); y < r.h; y++ {
		aStart := (uint64(r.y+y)*uint64(a.Width) + uint64(r.x)) * 4
		bStart := (uint64(r.y+y)*uint64(b.Width) + uint64(r.x)) * 4
		if !bytes.Equal(a.Pixels[aStart:aStart+uint64(r.w)*4], b.Pixels[bStart:bStart+uint64(r.w)*4]) {
			return false
		}
	}
	return true
}

func encodeRectangle(image Image, r rect) protocol.Rectangle {
	raw := make([]byte, uint64(r.w)*uint64(r.h)*4)
	for y := uint32(0); y < r.h; y++ {
		start := (uint64(r.y+y)*uint64(image.Width) + uint64(r.x)) * 4
		copy(raw[uint64(y)*uint64(r.w)*4:], image.Pixels[start:start+uint64(r.w)*4])
	}
	var compressed bytes.Buffer
	zw := zlib.NewWriter(&compressed)
	_, _ = zw.Write(raw)
	_ = zw.Close()
	if compressed.Len() < len(raw) {
		return protocol.Rectangle{X: r.x, Y: r.y, Width: r.w, Height: r.h, Encoding: protocol.EncodingZlibBGRA, Pixels: compressed.Bytes()}
	}
	return protocol.Rectangle{X: r.x, Y: r.y, Width: r.w, Height: r.h, Encoding: protocol.EncodingRawBGRA, Pixels: raw}
}

func (d *Detector) keyframe(image Image) (protocol.Frame, error) {
	bands := d.encodeRectBanded(image, rect{0, 0, image.Width, image.Height})
	if len(bands) > int(d.config.MaxRectangles) {
		return protocol.Frame{}, ErrRectLimit
	}
	return protocol.Frame{Generation: d.generation, FrameSequence: d.sequence, Keyframe: true, Rectangles: bands}, nil
}

// encodeRects encodes each dirty rectangle, banding any that exceeds the
// per-rectangle byte budget. It returns false as soon as the running rectangle
// count would exceed the configured limit, so the caller can fall back to a
// keyframe without encoding the rest of the frame.
func (d *Detector) encodeRects(image Image, rects []rect) ([]protocol.Rectangle, bool) {
	limit := int(d.config.MaxRectangles)
	out := make([]protocol.Rectangle, 0, len(rects))
	for _, r := range rects {
		bands := d.encodeRectBanded(image, r)
		if len(out)+len(bands) > limit {
			return nil, false
		}
		out = append(out, bands...)
	}
	return out, true
}

// encodeRectBanded encodes one source rectangle, splitting it along its height
// into bands so each band's uncompressed BGRA bytes fit the configured budget.
// Coverage is exact and bands never overlap; each band is independently
// zlib-compressed when that is smaller. A rectangle that already fits is
// encoded whole, preserving the pre-banding bytes exactly.
func (d *Detector) encodeRectBanded(image Image, r rect) []protocol.Rectangle {
	rows := bandRows(r.w, d.config.MaxRectangleBytes)
	if rows >= r.h {
		return []protocol.Rectangle{encodeRectangle(image, r)}
	}
	bands := make([]protocol.Rectangle, 0, (r.h+rows-1)/rows)
	for y := uint32(0); y < r.h; y += rows {
		h := rows
		if y+h > r.h {
			h = r.h - y
		}
		bands = append(bands, encodeRectangle(image, rect{x: r.x, y: r.y + y, w: r.w, h: h}))
	}
	return bands
}

// bandRows returns the greatest whole number of rows of a width-wide rectangle
// whose uncompressed BGRA bytes fit budget, and at least one. A single row of
// any protocol-legal width (<= MaxDimensionHard, so <= 32 KiB of pixels) is
// far below the production budget, so a band always fits in practice.
func bandRows(width uint32, budget uint64) uint32 {
	if width == 0 {
		return 1
	}
	rows := budget / (uint64(width) * 4)
	if rows < 1 {
		return 1
	}
	if rows > uint64(^uint32(0)) {
		return ^uint32(0)
	}
	return uint32(rows)
}

func (d *Detector) remember(image Image) {
	ts := d.config.TileSize
	cols, rows := (image.Width+ts-1)/ts, (image.Height+ts-1)/ts
	hashes := make([]uint64, cols*rows)
	for ty := uint32(0); ty < rows; ty++ {
		for tx := uint32(0); tx < cols; tx++ {
			hashes[ty*cols+tx] = hashRect(image, tileRect(tx, ty, ts, image.Width, image.Height))
		}
	}
	d.rememberWithHashes(image, hashes)
}

func (d *Detector) rememberWithHashes(image Image, hashes []uint64) {
	d.previous = Image{Width: image.Width, Height: image.Height, Pixels: append([]byte(nil), image.Pixels...)}
	d.hashes = hashes
}
