package protocol

import (
	"errors"
	"fmt"
)

const (
	Magic      = "VDP1"
	Version1   = uint16(1)
	Version2   = uint16(2)
	HeaderSize = 24
	// MaxFramePartsHard bounds the number of FRAME_PART messages a single
	// logical frame may be split into. A frame needing more parts is rejected
	// rather than streamed unbounded.
	MaxFramePartsHard     = uint16(32)
	MaxControlPayloadHard = uint32(64 << 10)
	MaxPixelPayloadHard   = uint32(16 << 20)
	MaxRectanglesHard     = uint16(256)
	MaxDimensionHard      = uint32(8192)
	// MaxWheelClicks is the largest wheel detent count a single PointerWheel
	// event may request (delta / 120). The protocol validation below clamps
	// the wheel delta to this bound so the host's input injector — which is
	// the only place a delta is actually replayed — can never reject a
	// message the wire accepted. Keeping the single source of truth here
	// (rather than duplicated in the input package) means validation and
	// injection cannot drift apart.
	MaxWheelClicks = 20
)

var (
	ErrMalformed       = errors.New("malformed protocol message")
	ErrMessageTooLarge = errors.New("protocol message too large")
	ErrUnknownType     = errors.New("unknown message type")
	ErrDirection       = errors.New("message sent in invalid direction")
	ErrSequence        = errors.New("invalid record sequence")
	ErrVersion         = errors.New("unsupported protocol version")
	ErrNoCommonVersion = errors.New("no common protocol version")
	ErrState           = errors.New("message invalid for session state")
)

type Type uint16

const (
	TypeAuth Type = iota + 1
	TypeClientHello
	TypeServerHello
	TypeDisplayConfig
	TypeFrame
	TypeKeyframeRequest
	TypeKey
	TypePointerMove
	TypePointerButton
	TypePointerWheel
	TypeFocusLost
	TypePing
	TypePong
	TypeError
	TypeClose
	// TypeFramePart is a v2-only message carrying one part of a frame split
	// across multiple messages. Appending it keeps every earlier type number
	// stable; range-based validators below are widened deliberately.
	TypeFramePart
)

func (t Type) String() string {
	names := [...]string{"", "AUTH", "CLIENT_HELLO", "SERVER_HELLO", "DISPLAY_CONFIG", "FRAME", "KEYFRAME_REQUEST", "KEY", "POINTER_MOVE", "POINTER_BUTTON", "POINTER_WHEEL", "FOCUS_LOST", "PING", "PONG", "ERROR", "CLOSE", "FRAME_PART"}
	if int(t) < len(names) && t > 0 {
		return names[t]
	}
	return fmt.Sprintf("TYPE_%d", t)
}

func (t Type) valid() bool { return t >= TypeAuth && t <= TypeFramePart }

// usesPixelPayload reports whether a message type carries rectangle pixel
// payloads and therefore draws on the pixel (not control) payload budget.
func usesPixelPayload(t Type) bool { return t == TypeFrame || t == TypeFramePart }

// requiresVersion2 reports whether a message type is only defined by protocol
// version 2 or later. The record envelope's version gates it: a v1-pinned
// encoder or decoder rejects these types with ErrVersion.
func requiresVersion2(t Type) bool { return t == TypeFramePart }

type Message interface{ Type() Type }
type Auth struct {
	Version uint16
	Token   [32]byte
}

func (Auth) Type() Type { return TypeAuth }

type ClientHello struct {
	MinVersion, MaxVersion uint16
	Capabilities           uint64
}

func (ClientHello) Type() Type { return TypeClientHello }

type ServerHello struct {
	Version      uint16
	Capabilities uint64
}

func (ServerHello) Type() Type { return TypeServerHello }

type PixelFormat uint16

const PixelBGRA8888 PixelFormat = 1

type DisplayConfig struct {
	Generation                                       uint64
	Width, Height, PhysicalWidthMM, PhysicalHeightMM uint32
	PixelFormat                                      PixelFormat
}

func (DisplayConfig) Type() Type { return TypeDisplayConfig }

type Encoding uint16

const (
	EncodingRawBGRA  Encoding = 1
	EncodingZlibBGRA Encoding = 2
)

type Rectangle struct {
	X, Y, Width, Height uint32
	Encoding            Encoding
	Pixels              []byte
}
type Frame struct {
	Generation, FrameSequence, BaseFrameSequence uint64
	Keyframe                                     bool
	Rectangles                                   []Rectangle
}

func (Frame) Type() Type { return TypeFrame }

// FramePart is one ordered piece of a frame whose rectangles did not fit in a
// single FRAME message (protocol v2). Every part repeats the frame's logical
// header so a part is self-describing, and adds its zero-based PartIndex and
// the PartCount of the sequence it belongs to. Rectangles are never split
// across parts; a part's rectangle list is a subset of the frame's, in order.
type FramePart struct {
	Generation, FrameSequence, BaseFrameSequence uint64
	Keyframe                                     bool
	PartIndex, PartCount                         uint16
	Rectangles                                   []Rectangle
}

func (FramePart) Type() Type { return TypeFramePart }

type KeyframeRequest struct{ Generation uint64 }

func (KeyframeRequest) Type() Type { return TypeKeyframeRequest }

type Action uint8

const (
	ActionDown Action = 1
	ActionUp   Action = 2
)

type Key struct {
	Generation, InputSequence uint64
	Usage                     uint16
	Action                    Action
	Modifiers                 uint8
}

func (Key) Type() Type { return TypeKey }

type PointerMove struct {
	Generation, InputSequence uint64
	X, Y                      uint32
}

func (PointerMove) Type() Type { return TypePointerMove }

type Button uint8

const (
	ButtonLeft Button = iota + 1
	ButtonMiddle
	ButtonRight
	ButtonBack
	ButtonForward
)

type PointerButton struct {
	Generation, InputSequence uint64
	Button                    Button
	Action                    Action
}

func (PointerButton) Type() Type { return TypePointerButton }

type PointerWheel struct {
	Generation, InputSequence uint64
	Horizontal, Vertical      int16
}

func (PointerWheel) Type() Type { return TypePointerWheel }

type FocusLost struct{ Generation, InputSequence uint64 }

func (FocusLost) Type() Type { return TypeFocusLost }

type Ping struct{ Nonce uint64 }

func (Ping) Type() Type { return TypePing }

type Pong struct{ Nonce uint64 }

func (Pong) Type() Type { return TypePong }

type ErrorCode uint16

const (
	ErrorProtocol       ErrorCode = 1
	ErrorAuthentication ErrorCode = 2
	ErrorBusy           ErrorCode = 3
)

type ErrorMessage struct {
	Code       ErrorCode
	Diagnostic string
}

func (ErrorMessage) Type() Type { return TypeError }

type CloseCode uint16

const (
	CloseNormal   CloseCode = 0
	CloseProtocol CloseCode = 1
)

type Close struct {
	Code   CloseCode
	Reason string
}

func (Close) Type() Type { return TypeClose }

type Limits struct {
	MaxControlPayload, MaxPixelPayload uint32
	MaxRectangles                      uint16
	MaxFrameParts                      uint16
	MaxDimension                       uint32
}

func DefaultLimits() Limits {
	return Limits{
		MaxControlPayload: MaxControlPayloadHard,
		MaxPixelPayload:   MaxPixelPayloadHard,
		MaxRectangles:     MaxRectanglesHard,
		MaxFrameParts:     MaxFramePartsHard,
		MaxDimension:      MaxDimensionHard,
	}
}

func (l Limits) bounded() Limits {
	if l.MaxControlPayload == 0 || l.MaxControlPayload > MaxControlPayloadHard {
		l.MaxControlPayload = MaxControlPayloadHard
	}
	if l.MaxPixelPayload == 0 || l.MaxPixelPayload > MaxPixelPayloadHard {
		l.MaxPixelPayload = MaxPixelPayloadHard
	}
	if l.MaxRectangles == 0 || l.MaxRectangles > MaxRectanglesHard {
		l.MaxRectangles = MaxRectanglesHard
	}
	if l.MaxFrameParts == 0 || l.MaxFrameParts > MaxFramePartsHard {
		l.MaxFrameParts = MaxFramePartsHard
	}
	if l.MaxDimension == 0 || l.MaxDimension > MaxDimensionHard {
		l.MaxDimension = MaxDimensionHard
	}
	return l
}

type Role uint8

const (
	RoleClient Role = iota + 1
	RoleHost
)

type Flow uint8

const (
	Incoming Flow = iota + 1
	Outgoing
)

type State uint8

const (
	StateConnected State = iota + 1
	StateAuthenticating
	StateNegotiating
	StateActive
	StateClosing
	StateClosed
)

func ValidateState(state State, message Message) error {
	if message == nil || state < StateConnected || state > StateClosed {
		return ErrMalformed
	}
	t := message.Type()
	valid := false
	switch state {
	case StateConnected:
		valid = false // TLS handshake carries no protocol messages.
	case StateAuthenticating:
		valid = t == TypeAuth || t == TypeError || t == TypeClose
	case StateNegotiating:
		valid = t == TypeClientHello || t == TypeServerHello || t == TypeDisplayConfig || t == TypeError || t == TypeClose
	case StateActive:
		valid = t >= TypeDisplayConfig && t <= TypeFramePart && t != TypeAuth && t != TypeClientHello && t != TypeServerHello
	case StateClosing:
		valid = t == TypeError || t == TypeClose
	case StateClosed:
		valid = false
	}
	if !valid {
		return fmt.Errorf("%w: %s", ErrState, t)
	}
	return nil
}

func ValidateDirection(role Role, flow Flow, message Message) error {
	if message == nil || (role != RoleClient && role != RoleHost) || (flow != Incoming && flow != Outgoing) {
		return ErrMalformed
	}
	t := message.Type()
	bidirectional := t == TypePing || t == TypePong || t == TypeError || t == TypeClose
	clientOrigin := t == TypeAuth || t == TypeClientHello || t == TypeKeyframeRequest || (t >= TypeKey && t <= TypeFocusLost)
	hostOrigin := t == TypeServerHello || t == TypeDisplayConfig || t == TypeFrame || t == TypeFramePart
	originClient := (role == RoleClient && flow == Outgoing) || (role == RoleHost && flow == Incoming)
	if bidirectional || (originClient && clientOrigin) || (!originClient && hostOrigin) {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrDirection, t)
}

func NegotiateVersion(clientMin, clientMax, serverMin, serverMax uint16) (uint16, error) {
	if clientMin == 0 || serverMin == 0 || clientMin > clientMax || serverMin > serverMax {
		return 0, ErrMalformed
	}
	max := clientMax
	if serverMax < max {
		max = serverMax
	}
	min := clientMin
	if serverMin > min {
		min = serverMin
	}
	if min > max {
		return 0, ErrNoCommonVersion
	}
	// The overlap window is resolved; now reject any agreed version this
	// implementation does not speak. v1.4.1 clamped the client max against the
	// server max before this guard, so a v2 offer {1,2} against a deployed v1
	// host {1,1} resolves to 1 here and negotiates down on the first handshake
	// — no retry. The guard is what stops two v2 peers from agreeing on an
	// unsupported higher version.
	if max != Version1 && max != Version2 {
		return 0, ErrVersion
	}
	return max, nil
}
