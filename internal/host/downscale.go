package host

import "virtualdesktop/internal/damage"

// downscaleFactor returns the smallest k >= 0 for which a 2^k box downscale of
// a width-by-height capture fits the per-rectangle byte budget:
// (width>>k)*(height>>k)*4 <= budget. It returns 0 when the capture already
// fits, so an in-budget v1 frame is never resampled. A degenerate capture that
// cannot be reduced without a zero dimension keeps its last valid factor.
func downscaleFactor(width, height uint32, budget uint64) uint32 {
	if width == 0 || height == 0 {
		return 0
	}
	for k := uint32(0); k < 32; k++ {
		w, h := width>>k, height>>k
		if w == 0 || h == 0 {
			return k - 1
		}
		if uint64(w)*uint64(h)*4 <= budget {
			return k
		}
	}
	return 31
}

// downscaleImage returns a box-averaged 2^k reduction of src. Each output pixel
// is the rounded average of the 2^k-by-2^k source block at its top-left; source
// rows and columns past (dim>>k)<<k are dropped, matching the integer
// (width>>k, height>>k) output geometry. All four BGRA channels are averaged.
// The transform is deterministic: integer sums with round-half-up division.
func downscaleImage(src damage.Image, k uint32) damage.Image {
	if k == 0 {
		return src
	}
	block := uint32(1) << k
	outW, outH := src.Width>>k, src.Height>>k
	if outW == 0 || outH == 0 {
		return src
	}
	dst := damage.Image{Width: outW, Height: outH, Pixels: make([]byte, uint64(outW)*uint64(outH)*4)}
	area := uint64(block) * uint64(block)
	for oy := uint32(0); oy < outH; oy++ {
		for ox := uint32(0); ox < outW; ox++ {
			var sum [4]uint64
			for by := uint32(0); by < block; by++ {
				sy := oy*block + by
				for bx := uint32(0); bx < block; bx++ {
					sx := ox*block + bx
					off := (uint64(sy)*uint64(src.Width) + uint64(sx)) * 4
					for c := 0; c < 4; c++ {
						sum[c] += uint64(src.Pixels[off+uint64(c)])
					}
				}
			}
			dstOff := (uint64(oy)*uint64(outW) + uint64(ox)) * 4
			for c := 0; c < 4; c++ {
				dst.Pixels[dstOff+uint64(c)] = byte((sum[c] + area/2) / area)
			}
		}
	}
	return dst
}

// remapCoordinate maps a pointer coordinate reported in the advertised
// (possibly downscaled) display back to the native capture dimension. The
// mapping is endpoint-preserving: 0 maps to 0 and advertised-1 maps to
// native-1, so the extreme advertised pixels land on the extreme native pixels
// instead of stopping one short — the old value*native/advertised form sent
// 1499/1500 to 2998/3000 on an exact 2x scale. Interior coordinates are
// linearly interpolated between those endpoints and rounded to nearest. Values
// at or past advertised are clamped, and a degenerate advertised dimension
// (<= 1) or a zero native dimension collapses to 0, so the function can never
// divide by zero. When advertised equals native it is the identity, so v2 and
// non-downscaled v1 sessions are unaffected.
func remapCoordinate(value, advertised, native uint32) uint32 {
	if advertised <= 1 || native == 0 {
		return 0
	}
	if value >= advertised {
		value = advertised - 1
	}
	span := uint64(advertised - 1)
	mapped := (uint64(value)*uint64(native-1) + span/2) / span
	if mapped >= uint64(native) {
		mapped = uint64(native - 1)
	}
	return uint32(mapped)
}
