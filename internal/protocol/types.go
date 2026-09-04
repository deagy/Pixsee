package protocol

import (
	"errors"
	"fmt"
)

const (
	Magic                 = "VDP1"
	Version1              = uint16(1)
	HeaderSize            = 24
	MaxControlPayloadHard = uint32(64 << 10)
	MaxPixelPayloadHard   = uint32(16 << 20)
	MaxRectanglesHard     = uint16(256)
	MaxDimensionHard      = uint32(8192)
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
)

func (t Type) String() string {
	names := [...]string{"", "AUTH", "CLIENT_HELLO", "SERVER_HELLO", "DISPLAY_CONFIG", "FRAME", "KEYFRAME_REQUEST", "KEY", "POINTER_MOVE", "POINTER_BUTTON", "POINTER_WHEEL", "FOCUS_LOST", "PING", "PONG", "ERROR", "CLOSE"}
	if int(t) < len(names) && t > 0 {
		return names[t]
	}
	return fmt.Sprintf("TYPE_%d", t)
}

func (t Type) valid() bool { return t >= TypeAuth && t <= TypeClose }

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
	MaxDimension                       uint32
}

func DefaultLimits() Limits {
	return Limits{MaxControlPayloadHard, MaxPixelPayloadHard, MaxRectanglesHard, MaxDimensionHard}
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
		valid = t >= TypeDisplayConfig && t <= TypeClose && t != TypeAuth && t != TypeClientHello && t != TypeServerHello
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
	hostOrigin := t == TypeServerHello || t == TypeDisplayConfig || t == TypeFrame
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
	if max != Version1 {
		return 0, ErrVersion
	}
	return max, nil
}
