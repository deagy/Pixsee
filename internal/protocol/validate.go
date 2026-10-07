package protocol

import (
	"fmt"
	"unicode/utf8"
)

func validInputHeader(generation, sequence uint64) bool {
	return generation != 0 && sequence != 0
}

// Version 1 accepts defined keyboard-page usages and rejects reserved gaps.
func validKeyboardUsage(usage uint16) bool {
	return (usage >= 0x04 && usage <= 0xa4) || (usage >= 0xb0 && usage <= 0xdd) || (usage >= 0xe0 && usage <= 0xe7)
}

func rectanglesOverlap(a, b Rectangle) bool {
	return a.X < b.X+b.Width && b.X < a.X+a.Width && a.Y < b.Y+b.Height && b.Y < a.Y+a.Height
}

// supportedVersion reports whether v is a protocol version this build speaks.
func supportedVersion(v uint16) bool { return v == Version1 || v == Version2 }

// validateFrameHeader enforces the invariants shared by FRAME and FRAME_PART:
// a nonzero generation and sequence, a coherent keyframe/base pairing, and a
// nonempty rectangle list within the per-message rectangle cap.
func validateFrameHeader(generation, frameSequence, baseFrameSequence uint64, keyframe bool, rectangles []Rectangle, limits Limits) error {
	if generation == 0 || frameSequence == 0 || len(rectangles) == 0 {
		return ErrMalformed
	}
	if (keyframe && baseFrameSequence != 0) || (!keyframe && baseFrameSequence == 0) || baseFrameSequence >= frameSequence {
		return ErrMalformed
	}
	if len(rectangles) > int(limits.MaxRectangles) {
		return ErrMessageTooLarge
	}
	return nil
}

// validateRectangles enforces the per-rectangle rules shared by FRAME and
// FRAME_PART: nonzero in-bounds geometry, a known encoding, an exact raw-pixel
// length (or a nonempty compressed body), the per-rectangle pixel cap, and no
// overlap with any earlier rectangle.
func validateRectangles(rectangles []Rectangle, limits Limits) error {
	for i, r := range rectangles {
		if r.Width == 0 || r.Height == 0 || r.X >= limits.MaxDimension || r.Y >= limits.MaxDimension || r.Width > limits.MaxDimension-r.X || r.Height > limits.MaxDimension-r.Y {
			return ErrMalformed
		}
		if r.Encoding != EncodingRawBGRA && r.Encoding != EncodingZlibBGRA {
			return ErrMalformed
		}
		pixels := uint64(r.Width) * uint64(r.Height) * 4
		if pixels > uint64(limits.MaxPixelPayload) {
			return ErrMessageTooLarge
		}
		if r.Encoding == EncodingRawBGRA && uint64(len(r.Pixels)) != pixels {
			return fmt.Errorf("%w: raw pixels", ErrMalformed)
		}
		if r.Encoding == EncodingZlibBGRA && len(r.Pixels) == 0 {
			return ErrMalformed
		}
		for _, earlier := range rectangles[:i] {
			if rectanglesOverlap(r, earlier) {
				return fmt.Errorf("%w: overlapping rectangles", ErrMalformed)
			}
		}
	}
	return nil
}

// absWheelClicks returns the total number of wheel detents a PointerWheel
// event requests, summed across both axes (delta / 120).
func absWheelClicks(horizontal, vertical int16) int {
	h, v := int(horizontal)/120, int(vertical)/120
	if h < 0 {
		h = -h
	}
	if v < 0 {
		v = -v
	}
	return h + v
}

func ValidateMessage(m Message, limits Limits) error {
	limits = limits.bounded()
	if m == nil {
		return ErrMalformed
	}
	switch v := m.(type) {
	case Auth:
		if v.Version != Version1 {
			return ErrVersion
		}
		var zero [32]byte
		if v.Token == zero {
			return ErrMalformed
		}
	case ClientHello:
		// A client may advertise a range extending past the versions this
		// build speaks (forward compatibility); negotiation clamps the range
		// to the supported server window and rejects an unsupported agreed
		// version. Validation only rejects an incoherent range or unknown
		// capabilities.
		if v.MinVersion < Version1 || v.MaxVersion < v.MinVersion || v.Capabilities != 0 {
			return ErrMalformed
		}
	case ServerHello:
		if !supportedVersion(v.Version) {
			return ErrVersion
		}
		if v.Capabilities != 0 {
			return ErrMalformed
		}
	case DisplayConfig:
		if v.Generation == 0 || v.Width == 0 || v.Height == 0 || v.Width > limits.MaxDimension || v.Height > limits.MaxDimension || v.PixelFormat != PixelBGRA8888 {
			return ErrMalformed
		}
	case Frame:
		if err := validateFrameHeader(v.Generation, v.FrameSequence, v.BaseFrameSequence, v.Keyframe, v.Rectangles, limits); err != nil {
			return err
		}
		if err := validateRectangles(v.Rectangles, limits); err != nil {
			return err
		}
	case FramePart:
		if err := validateFrameHeader(v.Generation, v.FrameSequence, v.BaseFrameSequence, v.Keyframe, v.Rectangles, limits); err != nil {
			return err
		}
		// A FRAME_PART sequence is only meaningful with at least two parts;
		// a single-part frame is sent as an ordinary FRAME.
		if v.PartCount < 2 || v.PartCount > limits.MaxFrameParts || v.PartIndex >= v.PartCount {
			return ErrMalformed
		}
		if err := validateRectangles(v.Rectangles, limits); err != nil {
			return err
		}
	case KeyframeRequest:
		if v.Generation == 0 {
			return ErrMalformed
		}
	case Key:
		if !validInputHeader(v.Generation, v.InputSequence) || !validKeyboardUsage(v.Usage) || v.Action < ActionDown || v.Action > ActionUp {
			return ErrMalformed
		}
	case PointerMove:
		if !validInputHeader(v.Generation, v.InputSequence) || v.X >= limits.MaxDimension || v.Y >= limits.MaxDimension {
			return ErrMalformed
		}
	case PointerButton:
		if !validInputHeader(v.Generation, v.InputSequence) || v.Button < ButtonLeft || v.Button > ButtonForward || v.Action < ActionDown || v.Action > ActionUp {
			return ErrMalformed
		}
	case PointerWheel:
		if !validInputHeader(v.Generation, v.InputSequence) || (v.Horizontal == 0 && v.Vertical == 0) || v.Horizontal%120 != 0 || v.Vertical%120 != 0 {
			return ErrMalformed
		}
		if absWheelClicks(v.Horizontal, v.Vertical) > MaxWheelClicks {
			return fmt.Errorf("%w: wheel delta exceeds %d detents", ErrMalformed, MaxWheelClicks)
		}
	case FocusLost:
		if !validInputHeader(v.Generation, v.InputSequence) {
			return ErrMalformed
		}
	case Ping, Pong:
	case ErrorMessage:
		if v.Code < ErrorProtocol || v.Code > ErrorBusy || len(v.Diagnostic) > 1024 || !utf8.ValidString(v.Diagnostic) {
			return ErrMalformed
		}
	case Close:
		if v.Code > CloseProtocol || len(v.Reason) > 1024 || !utf8.ValidString(v.Reason) {
			return ErrMalformed
		}
	default:
		return ErrUnknownType
	}
	// A pixel-bearing message is bounded arithmetically so an oversized frame
	// is rejected before it is marshaled, which would copy every rectangle's
	// pixels into an aggregate that is then discarded. Control messages are
	// small (their hard cap is 64 KiB), so they keep the marshal-based check
	// that reflects their exact wire length.
	if usesPixelPayload(m.Type()) {
		size, ok := pixelPayloadSize(m)
		if !ok {
			// Unreachable while every pixel-bearing type has an arithmetic
			// size; falling back to an exact marshal keeps the bound from
			// being skipped if one is ever added without one.
			payload, err := marshal(m)
			if err != nil {
				return err
			}
			size = uint64(len(payload))
		}
		if size > uint64(limits.MaxPixelPayload) {
			return ErrMessageTooLarge
		}
		return nil
	}
	payload, err := marshal(m)
	if err != nil {
		return err
	}
	if uint64(len(payload)) > uint64(limits.MaxControlPayload) {
		return ErrMessageTooLarge
	}
	return nil
}
