package protocol

import "errors"

var (
	// ErrFrameFits reports that every rectangle in a frame fits inside a single
	// message under the caller's budget, so the frame should be sent as an
	// ordinary FRAME rather than a FRAME_PART sequence. A valid FRAME_PART
	// sequence requires at least two parts.
	ErrFrameFits = errors.New("protocol: frame fits in a single message")
	// ErrFrameTooLarge reports that a frame needs more than the configured
	// MaxFrameParts parts to fit the budget.
	ErrFrameTooLarge = errors.New("protocol: frame needs too many parts")
	// ErrPartTooLarge reports that a single rectangle does not fit the budget
	// on its own. Pixel rectangles are never split, so host-side banding of an
	// oversized rectangle is out of scope for this packer.
	ErrPartTooLarge = errors.New("protocol: rectangle exceeds part budget")
)

const (
	// framePartHeaderSize is the fixed marshaled size of a FramePart payload
	// before its rectangle list: generation, frame sequence and base sequence
	// (8+8+8), the keyframe flag (1), part index and part count (2+2), and the
	// rectangle count (2).
	framePartHeaderSize = uint64(31)
	// frameRectMetadataSize is the fixed marshaled size of one rectangle's
	// metadata: x, y, width, height (4+4+4+4), encoding (2) and pixel length
	// (4). Its pixel bytes follow.
	frameRectMetadataSize = uint64(22)
)

// SplitFrame groups frame's already-encoded whole rectangles into ordered
// FRAME_PART messages whose marshaled payloads each stay within
// limits.MaxPixelPayload bytes (the same pixel-payload budget the codec
// enforces, excluding the 24-byte record header). Rectangles keep their
// original order and are never split across parts. Every part carries frame's
// generation/sequence/keyframe header plus a PartIndex/PartCount pair.
//
// SplitFrame is deterministic: it packs rectangles greedily in order, so the
// same frame and limits always yield the same partition. If all rectangles fit
// in a single part it returns ErrFrameFits because the caller should send an
// ordinary FRAME; if a part cannot hold even one rectangle it returns
// ErrPartTooLarge; if the frame needs more than limits.MaxFrameParts parts it
// returns ErrFrameTooLarge. limits is passed through bounded(), so a zero or
// oversized field is clamped to the protocol hard cap; the caller's active
// limits therefore govern both the per-part byte budget and the part count.
func SplitFrame(frame Frame, limits Limits) ([]FramePart, error) {
	if len(frame.Rectangles) == 0 {
		return nil, ErrMalformed
	}
	limits = limits.bounded()
	budget := uint64(limits.MaxPixelPayload)
	maxParts := int(limits.MaxFrameParts)

	parts := make([]FramePart, 0, 2)
	current := FramePart{
		Generation:        frame.Generation,
		FrameSequence:     frame.FrameSequence,
		BaseFrameSequence: frame.BaseFrameSequence,
		Keyframe:          frame.Keyframe,
	}
	// currentSize is the payload length the part would marshal to, carried
	// incrementally so no candidate is ever marshaled or has its pixels copied.
	// An empty part still counts its fixed header.
	currentSize := framePartHeaderSize
	flush := func() {
		if len(current.Rectangles) == 0 {
			return
		}
		parts = append(parts, current)
		current.Rectangles = nil
		currentSize = framePartHeaderSize
	}
	for _, rect := range frame.Rectangles {
		rectSize := framePartRectSize(rect)
		if currentSize+rectSize <= budget {
			current.Rectangles = append(current.Rectangles, rect)
			currentSize += rectSize
			continue
		}
		// This rectangle pushed the part over budget. If the part is already
		// empty the rectangle cannot fit anywhere (rectangles are not split).
		if len(current.Rectangles) == 0 {
			return nil, ErrPartTooLarge
		}
		// Close the current part and start a new one with this rectangle. If
		// the configured part cap is already reached, the frame can never fit.
		flush()
		if len(parts) >= maxParts {
			return nil, ErrFrameTooLarge
		}
		if framePartHeaderSize+rectSize > budget {
			return nil, ErrPartTooLarge
		}
		current.Rectangles = append(current.Rectangles, rect)
		currentSize = framePartHeaderSize + rectSize
	}
	flush()
	if len(parts) <= 1 {
		return nil, ErrFrameFits
	}
	if len(parts) > maxParts {
		return nil, ErrFrameTooLarge
	}
	for i := range parts {
		parts[i].PartIndex = uint16(i)
		parts[i].PartCount = uint16(len(parts))
	}
	return parts, nil
}

// framePartPayloadSize returns the marshaled payload length of a part, computed
// arithmetically so callers can size a candidate part without marshaling it or
// copying its pixels. The payload size is independent of the PartCount/PartIndex
// values, so callers may size a candidate part before those are finalized. The
// sum is carried in uint64, so it cannot overflow.
func framePartPayloadSize(part FramePart) uint64 {
	size := framePartHeaderSize
	for _, rect := range part.Rectangles {
		size += framePartRectSize(rect)
	}
	return size
}

// framePartRectSize returns the marshaled size a single rectangle contributes
// to a part payload: fixed metadata plus its pixel bytes.
func framePartRectSize(rect Rectangle) uint64 {
	return frameRectMetadataSize + uint64(len(rect.Pixels))
}
