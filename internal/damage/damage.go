package damage

import (
	"bytes"
	"compress/zlib"
	"errors"
	"hash/fnv"
	"virtualdesktop/internal/protocol"
)

var ErrInvalidImage = errors.New("invalid captured image")

type Image struct {
	Width, Height uint32
	Pixels        []byte // packed BGRA8888
}

type Config struct {
	TileSize      uint32
	DirtyRatio    float64
	MaxRectangles uint16
}

type Detector struct {
	config     Config
	previous   Image
	hashes     []uint64
	generation uint64
	sequence   uint64
}

func NewDetector(config Config) *Detector {
	if config.TileSize == 0 {
		config.TileSize = 64
	}
	if config.DirtyRatio <= 0 || config.DirtyRatio > 1 {
		config.DirtyRatio = .60
	}
	if config.MaxRectangles == 0 || config.MaxRectangles > protocol.MaxRectanglesHard {
		config.MaxRectangles = protocol.MaxRectanglesHard
	}
	return &Detector{config: config}
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
		frame := d.keyframe(image)
		d.remember(image)
		return frame, true, nil
	}

	rects, dirtyPixels, hashes := d.changedRectangles(image)
	if len(rects) == 0 {
		d.sequence--
		return protocol.Frame{}, false, nil
	}
	if len(rects) > int(d.config.MaxRectangles) || float64(dirtyPixels)/float64(uint64(image.Width)*uint64(image.Height)) > d.config.DirtyRatio {
		frame := d.keyframe(image)
		d.rememberWithHashes(image, hashes)
		return frame, true, nil
	}

	encoded := make([]protocol.Rectangle, 0, len(rects))
	for _, r := range rects {
		encoded = append(encoded, encodeRectangle(image, r))
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

func (d *Detector) keyframe(image Image) protocol.Frame {
	return protocol.Frame{Generation: d.generation, FrameSequence: d.sequence, Keyframe: true, Rectangles: []protocol.Rectangle{encodeRectangle(image, rect{0, 0, image.Width, image.Height})}}
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
