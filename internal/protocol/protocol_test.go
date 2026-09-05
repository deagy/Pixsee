package protocol

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestMessageRoundTrips(t *testing.T) {
	tests := []Message{
		Auth{Version: Version1, Token: [32]byte{1, 2, 3}},
		ClientHello{MinVersion: 1, MaxVersion: 1},
		ServerHello{Version: 1},
		DisplayConfig{Generation: 2, Width: 1920, Height: 1080, PhysicalWidthMM: 500, PhysicalHeightMM: 300, PixelFormat: PixelBGRA8888},
		Frame{Generation: 2, FrameSequence: 4, BaseFrameSequence: 3, Rectangles: []Rectangle{{X: 1, Y: 2, Width: 1, Height: 1, Encoding: EncodingRawBGRA, Pixels: []byte{1, 2, 3, 0xff}}}},
		KeyframeRequest{Generation: 2}, Key{Generation: 2, InputSequence: 3, Usage: 4, Action: ActionDown, Modifiers: 1},
		PointerMove{Generation: 2, InputSequence: 4, X: 100, Y: 200}, PointerButton{Generation: 2, InputSequence: 5, Button: ButtonLeft, Action: ActionUp},
		PointerWheel{Generation: 2, InputSequence: 6, Horizontal: -120, Vertical: 120}, FocusLost{Generation: 2, InputSequence: 7},
		Ping{Nonce: 8}, Pong{Nonce: 8}, ErrorMessage{Code: ErrorProtocol, Diagnostic: "bad record"}, Close{Code: CloseNormal, Reason: "done"},
	}
	for _, want := range tests {
		t.Run(want.Type().String(), func(t *testing.T) {
			var wire bytes.Buffer
			enc := NewEncoder(&wire, DefaultLimits())
			if err := enc.Encode(want); err != nil {
				t.Fatal(err)
			}
			got, err := NewDecoder(&wire, DefaultLimits()).Decode()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("round trip mismatch\n got: %#v\nwant: %#v", got, want)
			}
		})
	}
}

func TestVersionNegotiation(t *testing.T) {
	v, err := NegotiateVersion(1, 1, 1, 1)
	if err != nil || v != 1 {
		t.Fatalf("got version %d, err %v", v, err)
	}
	if _, err := NegotiateVersion(2, 3, 1, 1); !errors.Is(err, ErrNoCommonVersion) {
		t.Fatalf("got %v", err)
	}
	if _, err := NegotiateVersion(2, 1, 1, 1); !errors.Is(err, ErrMalformed) {
		t.Fatalf("got %v", err)
	}
}

func TestUnknownCapabilitiesAndControlCodesAreRejected(t *testing.T) {
	bad := []Message{
		ClientHello{MinVersion: Version1, MaxVersion: Version1, Capabilities: 1},
		ServerHello{Version: Version1, Capabilities: 1},
		ErrorMessage{Code: ErrorCode(99)},
		Close{Code: CloseCode(99)},
	}
	for _, message := range bad {
		if err := ValidateMessage(message, DefaultLimits()); !errors.Is(err, ErrMalformed) {
			t.Fatalf("accepted %#v: %v", message, err)
		}
	}
}

func TestZeroAuthenticationTokenIsRejected(t *testing.T) {
	if err := ValidateMessage(Auth{Version: Version1}, DefaultLimits()); !errors.Is(err, ErrMalformed) {
		t.Fatalf("got %v", err)
	}
}

func TestDecoderRejectsMalformedAndOversizedBeforeAllocation(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxControlPayload = 8
	for name, wire := range map[string][]byte{
		"magic":        append([]byte("NOPE"), make([]byte, HeaderSize-4)...),
		"reserved":     headerBytes(Magic, Version1, TypePing, 0, 1, 0, 1),
		"oversized":    headerBytes(Magic, Version1, TypePing, 0, 0, 9, 1),
		"unknown type": headerBytes(Magic, Version1, Type(99), 0, 0, 0, 1),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewDecoder(bytes.NewReader(wire), limits).Decode()
			if err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestDecoderSequenceAndDirection(t *testing.T) {
	var wire bytes.Buffer
	enc := NewEncoder(&wire, DefaultLimits())
	if err := enc.Encode(Ping{Nonce: 1}); err != nil {
		t.Fatal(err)
	}
	if err := enc.Encode(Pong{Nonce: 1}); err != nil {
		t.Fatal(err)
	}
	data := wire.Bytes()
	secondHeader := HeaderSize + 8 // PING payload is one uint64.
	copy(data[secondHeader+16:secondHeader+24], data[16:24])
	dec := NewDecoder(bytes.NewReader(data), DefaultLimits())
	if _, err := dec.Decode(); err != nil {
		t.Fatal(err)
	}
	if _, err := dec.Decode(); !errors.Is(err, ErrSequence) {
		t.Fatalf("got %v", err)
	}

	for _, test := range []struct {
		name string
		role Role
		flow Flow
		msg  Message
	}{
		{"client cannot send frame", RoleClient, Outgoing, Frame{}},
		{"host cannot receive frame", RoleHost, Incoming, Frame{}},
		{"host cannot send input", RoleHost, Outgoing, Key{}},
		{"client cannot receive input", RoleClient, Incoming, Key{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateDirection(test.role, test.flow, test.msg); !errors.Is(err, ErrDirection) {
				t.Fatalf("got %v", err)
			}
		})
	}
	for _, test := range []struct {
		role Role
		flow Flow
		msg  Message
	}{
		{RoleClient, Incoming, Frame{}},
		{RoleHost, Incoming, Key{}},
		{RoleClient, Outgoing, Key{}},
		{RoleHost, Outgoing, Frame{}},
	} {
		if err := ValidateDirection(test.role, test.flow, test.msg); err != nil {
			t.Fatal(err)
		}
	}
}

func TestConfiguredLimitsCannotExceedProtocolHardCaps(t *testing.T) {
	limits := Limits{MaxControlPayload: ^uint32(0), MaxPixelPayload: ^uint32(0), MaxRectangles: ^uint16(0), MaxDimension: ^uint32(0)}
	controlWire := headerBytes(Magic, Version1, TypePing, 0, 0, (64<<10)+1, 1)
	if _, err := NewDecoder(bytes.NewReader(controlWire), limits).Decode(); !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("control hard cap: got %v", err)
	}

	pixelWire := headerBytes(Magic, Version1, TypeFrame, 0, 0, (16<<20)+1, 1)
	if _, err := NewDecoder(bytes.NewReader(pixelWire), limits).Decode(); !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("pixel hard cap: got %v", err)
	}
}

func TestEncoderLimitsDiagnosticAndFrame(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxControlPayload = 32
	limits.MaxPixelPayload = 64
	if err := NewEncoder(io.Discard, limits).Encode(Close{Reason: strings.Repeat("x", 64)}); !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("got %v", err)
	}
	pixels := make([]byte, 80)
	frame := Frame{Generation: 1, FrameSequence: 1, Keyframe: true, Rectangles: []Rectangle{{Width: 5, Height: 4, Encoding: EncodingRawBGRA, Pixels: pixels}}}
	if err := NewEncoder(io.Discard, limits).Encode(frame); !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("got %v", err)
	}
}

func TestFrameValidation(t *testing.T) {
	pixel := []byte{0, 0, 0, 0xff}
	bad := []Frame{
		{Generation: 1, FrameSequence: 1, Keyframe: true, Rectangles: []Rectangle{{Width: 0, Height: 1}}},
		{Generation: 1, FrameSequence: 1, Keyframe: true, Rectangles: []Rectangle{{Width: 1, Height: 1, Pixels: []byte{1}}}},
		{Generation: 0, FrameSequence: 1, Keyframe: true, Rectangles: []Rectangle{{Width: 1, Height: 1, Encoding: EncodingRawBGRA, Pixels: pixel}}},
		{Generation: 1, FrameSequence: 0, Keyframe: true, Rectangles: []Rectangle{{Width: 1, Height: 1, Encoding: EncodingRawBGRA, Pixels: pixel}}},
		{Generation: 1, FrameSequence: 1, BaseFrameSequence: 1, Keyframe: true, Rectangles: []Rectangle{{Width: 1, Height: 1, Encoding: EncodingRawBGRA, Pixels: pixel}}},
		{Generation: 1, FrameSequence: 2, BaseFrameSequence: 0, Rectangles: []Rectangle{{Width: 1, Height: 1, Encoding: EncodingRawBGRA, Pixels: pixel}}},
		{Generation: 1, FrameSequence: 1, Keyframe: true, Rectangles: []Rectangle{
			{Width: 1, Height: 1, Encoding: EncodingRawBGRA, Pixels: pixel},
			{Width: 1, Height: 1, Encoding: EncodingRawBGRA, Pixels: pixel},
		}},
	}
	for _, frame := range bad {
		if err := ValidateMessage(frame, DefaultLimits()); err == nil {
			t.Fatalf("accepted %#v", frame)
		}
	}
}

func TestInputValidation(t *testing.T) {
	bad := []Message{
		Key{Generation: 1, InputSequence: 1, Usage: 1, Action: ActionDown},
		Key{Generation: 0, InputSequence: 1, Usage: 4, Action: ActionDown},
		PointerMove{Generation: 1, InputSequence: 0},
		PointerButton{Generation: 1, InputSequence: 1, Button: Button(99), Action: ActionDown},
		PointerWheel{Generation: 1, InputSequence: 1, Vertical: 1},
		FocusLost{Generation: 0, InputSequence: 1},
	}
	for _, message := range bad {
		if err := ValidateMessage(message, DefaultLimits()); !errors.Is(err, ErrMalformed) {
			t.Fatalf("accepted %#v: %v", message, err)
		}
	}
}

func TestSessionStateValidation(t *testing.T) {
	if err := ValidateState(StateAuthenticating, Auth{Version: Version1}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateState(StateAuthenticating, Frame{}); !errors.Is(err, ErrState) {
		t.Fatalf("got %v", err)
	}
	if err := ValidateState(StateNegotiating, ClientHello{MinVersion: 1, MaxVersion: 1}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateState(StateActive, Key{Generation: 1, InputSequence: 1, Usage: 4, Action: ActionDown}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateState(StateClosing, Ping{}); !errors.Is(err, ErrState) {
		t.Fatalf("got %v", err)
	}
	if err := ValidateState(StateClosed, Close{}); !errors.Is(err, ErrState) {
		t.Fatalf("got %v", err)
	}
}

func TestDecoderRejectsTruncatedPayload(t *testing.T) {
	wire := headerBytes(Magic, Version1, TypePing, 0, 0, 8, 1)
	wire = append(wire, 0, 1)
	if _, err := NewDecoder(bytes.NewReader(wire), DefaultLimits()).Decode(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("got %v", err)
	}
}

func FuzzDecoder(f *testing.F) {
	var valid bytes.Buffer
	if err := NewEncoder(&valid, DefaultLimits()).Encode(Ping{Nonce: 42}); err != nil {
		f.Fatal(err)
	}
	f.Add(valid.Bytes())
	f.Add([]byte("VDP1"))
	f.Fuzz(func(t *testing.T, wire []byte) {
		_, _ = NewDecoder(bytes.NewReader(wire), DefaultLimits()).Decode()
	})
}

func FuzzMessagePayloads(f *testing.F) {
	f.Add(uint16(TypeKey), []byte{})
	f.Add(uint16(TypeFrame), []byte{})
	f.Fuzz(func(t *testing.T, rawType uint16, payload []byte) {
		typeValue := Type(rawType)
		if !typeValue.valid() || len(payload) > 1<<20 {
			return
		}
		_, _ = unmarshal(typeValue, payload, DefaultLimits())
	})
}
