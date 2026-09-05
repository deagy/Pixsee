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
		if v.MinVersion == 0 || v.MinVersion > v.MaxVersion || v.Capabilities != 0 {
			return ErrMalformed
		}
	case ServerHello:
		if v.Version != Version1 {
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
		if v.Generation == 0 || v.FrameSequence == 0 || len(v.Rectangles) == 0 {
			return ErrMalformed
		}
		if (v.Keyframe && v.BaseFrameSequence != 0) || (!v.Keyframe && v.BaseFrameSequence == 0) || v.BaseFrameSequence >= v.FrameSequence {
			return ErrMalformed
		}
		if len(v.Rectangles) > int(limits.MaxRectangles) {
			return ErrMessageTooLarge
		}
		for i, r := range v.Rectangles {
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
			for _, earlier := range v.Rectangles[:i] {
				if rectanglesOverlap(r, earlier) {
					return fmt.Errorf("%w: overlapping rectangles", ErrMalformed)
				}
			}
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
	payload, err := marshal(m)
	if err != nil {
		return err
	}
	limit := limits.MaxControlPayload
	if m.Type() == TypeFrame {
		limit = limits.MaxPixelPayload
	}
	if uint64(len(payload)) > uint64(limit) {
		return ErrMessageTooLarge
	}
	return nil
}
