package protocol

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
)

// TestNegotiationMatrix pins the version negotiation decisions, including the
// downgrade path that needs no retry: a v2 client offer {1,2} against a
// deployed v1 host {1,1} resolves to v1 on the first handshake, because the
// client max is clamped against the server max before the supported-version
// guard runs.
func TestNegotiationMatrix(t *testing.T) {
	tests := []struct {
		name                 string
		clientMin, clientMax uint16
		serverMin, serverMax uint16
		want                 uint16
		wantErr              error
	}{
		{"v1 client to v1 host", 1, 1, 1, 1, 1, nil},
		{"v2 client to v1 host downgrades", 1, 2, 1, 1, 1, nil},
		{"v1 client to v2 host downgrades", 1, 1, 1, 2, 1, nil},
		{"v2 client to v2 host", 1, 2, 1, 2, 2, nil},
		{"v2-only client to v2 host", 2, 2, 1, 2, 2, nil},
		{"v2-only client to v1 host has no common version", 2, 2, 1, 1, 0, ErrNoCommonVersion},
		{"v1-only client to v2-only host has no common version", 1, 1, 2, 2, 0, ErrNoCommonVersion},
		{"forward-compatible client range to v2 host clamps to 2", 1, 3, 1, 2, 2, nil},
		{"forward-compatible client range to v1 host clamps to 1", 1, 3, 1, 1, 1, nil},
		{"forward-compatible client range with no common version", 3, 3, 1, 2, 0, ErrNoCommonVersion},
		{"agreed unsupported higher version rejected", 1, 3, 1, 3, 0, ErrVersion},
		{"zero minimum rejected", 0, 1, 1, 1, 0, ErrMalformed},
		{"client min above max rejected", 2, 1, 1, 1, 0, ErrMalformed},
		{"server min above max rejected", 1, 1, 3, 2, 0, ErrMalformed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NegotiateVersion(tc.clientMin, tc.clientMax, tc.serverMin, tc.serverMax)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("got (%d, %v), want error %v", got, err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got (%d, %v), want (%d, nil)", got, err, tc.want)
			}
		})
	}
}

// TestClientHelloForwardCompatibleRange proves validation accepts a client
// version range whose max extends past the versions this build speaks, so a
// newer client can negotiate down without a validation failure. Negotiation,
// not validation, clamps the range and rejects an unsupported agreed version.
// Only a zero minimum, an inverted range, or unknown capabilities are rejected.
func TestClientHelloForwardCompatibleRange(t *testing.T) {
	valid := []ClientHello{
		{MinVersion: Version1, MaxVersion: Version1},
		{MinVersion: Version1, MaxVersion: Version2},
		{MinVersion: Version1, MaxVersion: 3},
		{MinVersion: Version2, MaxVersion: 3},
	}
	for _, hello := range valid {
		if err := ValidateMessage(hello, DefaultLimits()); err != nil {
			t.Fatalf("rejected forward-compatible ClientHello %#v: %v", hello, err)
		}
	}
	bad := []ClientHello{
		{MinVersion: 0, MaxVersion: Version1},
		{MinVersion: Version2, MaxVersion: Version1},
		{MinVersion: Version1, MaxVersion: Version2, Capabilities: 1},
	}
	for _, hello := range bad {
		if err := ValidateMessage(hello, DefaultLimits()); !errors.Is(err, ErrMalformed) {
			t.Fatalf("accepted malformed ClientHello %#v: %v", hello, err)
		}
	}
}

// TestV1EnvelopeGoldenUnchanged locks the v1 byte layout: the default encoder
// stamps Version1 and a decoder with the default v1-only window reads it back.
// Any change to the default envelope version or header layout fails here.
func TestV1EnvelopeGoldenUnchanged(t *testing.T) {
	var wire bytes.Buffer
	if err := NewEncoder(&wire, DefaultLimits()).Encode(Ping{Nonce: 42}); err != nil {
		t.Fatal(err)
	}
	want := headerBytes(Magic, Version1, TypePing, 0, 0, 8, 1)
	want = append(want, 0, 0, 0, 0, 0, 0, 0, 42)
	if got := wire.Bytes(); !bytes.Equal(got, want) {
		t.Fatalf("v1 golden mismatch\n got: %x\nwant: %x", got, want)
	}
	got, err := NewDecoder(bytes.NewReader(wire.Bytes()), DefaultLimits()).Decode()
	if err != nil {
		t.Fatal(err)
	}
	if got != (Ping{Nonce: 42}) {
		t.Fatalf("v1 decode mismatch: %#v", got)
	}
}

func TestFramePartRoundTrip(t *testing.T) {
	pixel := []byte{1, 2, 3, 0xff}
	parts := []FramePart{
		{Generation: 2, FrameSequence: 4, BaseFrameSequence: 3, PartIndex: 1, PartCount: 3, Rectangles: []Rectangle{{X: 1, Y: 2, Width: 1, Height: 1, Encoding: EncodingRawBGRA, Pixels: pixel}}},
		{Generation: 2, FrameSequence: 1, Keyframe: true, PartIndex: 0, PartCount: 2, Rectangles: []Rectangle{{X: 0, Y: 0, Width: 1, Height: 1, Encoding: EncodingRawBGRA, Pixels: pixel}}},
		{Generation: 2, FrameSequence: 4, BaseFrameSequence: 3, PartIndex: 2, PartCount: 3, Rectangles: []Rectangle{{X: 0, Y: 0, Width: 1, Height: 1, Encoding: EncodingZlibBGRA, Pixels: []byte{0x78, 0x9c}}}},
	}
	for _, want := range parts {
		t.Run(want.Type().String(), func(t *testing.T) {
			var wire bytes.Buffer
			enc := NewEncoder(&wire, DefaultLimits())
			enc.PinVersion(Version2)
			if err := enc.Encode(want); err != nil {
				t.Fatal(err)
			}
			dec := NewDecoder(&wire, DefaultLimits())
			dec.PinVersion(Version2)
			got, err := dec.Decode()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("round trip mismatch\n got: %#v\nwant: %#v", got, want)
			}
		})
	}
}

func TestFramePartValidation(t *testing.T) {
	pixel := []byte{0, 0, 0, 0xff}
	valid := func() FramePart {
		return FramePart{Generation: 1, FrameSequence: 2, BaseFrameSequence: 1, PartIndex: 0, PartCount: 2,
			Rectangles: []Rectangle{{Width: 1, Height: 1, Encoding: EncodingRawBGRA, Pixels: pixel}}}
	}
	bad := map[string]FramePart{
		"part count below two":   func() FramePart { p := valid(); p.PartCount = 1; return p }(),
		"part count above cap":   func() FramePart { p := valid(); p.PartCount = MaxFramePartsHard + 1; return p }(),
		"part index at count":    func() FramePart { p := valid(); p.PartIndex = 2; return p }(),
		"part index above count": func() FramePart { p := valid(); p.PartIndex = 3; return p }(),
		"no rectangles":          func() FramePart { p := valid(); p.Rectangles = nil; return p }(),
		"zero generation":        func() FramePart { p := valid(); p.Generation = 0; return p }(),
		"zero frame sequence":    func() FramePart { p := valid(); p.FrameSequence = 0; return p }(),
		"keyframe with base":     func() FramePart { p := valid(); p.Keyframe = true; p.BaseFrameSequence = 1; return p }(),
		"non-keyframe no base":   func() FramePart { p := valid(); p.BaseFrameSequence = 0; return p }(),
		"base at sequence":       func() FramePart { p := valid(); p.BaseFrameSequence = p.FrameSequence; return p }(),
		"zero width":             func() FramePart { p := valid(); p.Rectangles[0].Width = 0; return p }(),
		"unknown encoding":       func() FramePart { p := valid(); p.Rectangles[0].Encoding = Encoding(99); return p }(),
		"raw pixel length":       func() FramePart { p := valid(); p.Rectangles[0].Pixels = []byte{1}; return p }(),
	}
	for name, part := range bad {
		t.Run(name, func(t *testing.T) {
			if err := ValidateMessage(part, DefaultLimits()); err == nil {
				t.Fatalf("accepted %#v", part)
			}
		})
	}
	if err := ValidateMessage(valid(), DefaultLimits()); err != nil {
		t.Fatalf("rejected a valid part: %v", err)
	}
}

// TestFramePartRequiresVersion2 proves the version gate lives in the codec: a
// v1-pinned encoder refuses to emit FRAME_PART, and a decoder rejects a
// FRAME_PART that arrives on a v1 record envelope even when its window accepts
// v1.
func TestFramePartRequiresVersion2(t *testing.T) {
	part := FramePart{Generation: 1, FrameSequence: 1, Keyframe: true, PartIndex: 0, PartCount: 2,
		Rectangles: []Rectangle{{Width: 1, Height: 1, Encoding: EncodingRawBGRA, Pixels: []byte{0, 0, 0, 0xff}}}}

	if err := NewEncoder(&bytes.Buffer{}, DefaultLimits()).Encode(part); !errors.Is(err, ErrVersion) {
		t.Fatalf("v1 encoder accepted FRAME_PART: %v", err)
	}

	// A structurally valid FRAME_PART payload on a v1 envelope: the decoder's
	// default v1 window accepts the version, then the type's v2 requirement
	// rejects it.
	payload, err := marshal(part)
	if err != nil {
		t.Fatal(err)
	}
	wire := headerBytes(Magic, Version1, TypeFramePart, 0, 0, uint32(len(payload)), 1)
	wire = append(wire, payload...)
	if _, err := NewDecoder(bytes.NewReader(wire), DefaultLimits()).Decode(); !errors.Is(err, ErrVersion) {
		t.Fatalf("v1 decoder accepted v1-enveloped FRAME_PART: %v", err)
	}
}

func TestFramePartDirectionAndState(t *testing.T) {
	part := FramePart{Generation: 1, FrameSequence: 1, Keyframe: true, PartIndex: 0, PartCount: 2}
	rejected := []struct {
		name string
		role Role
		flow Flow
		msg  Message
	}{
		{"client cannot send frame part", RoleClient, Outgoing, part},
		{"host cannot receive frame part", RoleHost, Incoming, part},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateDirection(tc.role, tc.flow, tc.msg); !errors.Is(err, ErrDirection) {
				t.Fatalf("got %v", err)
			}
		})
	}
	for _, tc := range []struct {
		role Role
		flow Flow
	}{
		{RoleHost, Outgoing},
		{RoleClient, Incoming},
	} {
		if err := ValidateDirection(tc.role, tc.flow, part); err != nil {
			t.Fatalf("direction %v/%v rejected: %v", tc.role, tc.flow, err)
		}
	}
	if err := ValidateState(StateActive, part); err != nil {
		t.Fatalf("active state rejected FRAME_PART: %v", err)
	}
	if err := ValidateState(StateNegotiating, part); !errors.Is(err, ErrState) {
		t.Fatalf("negotiating state accepted FRAME_PART: %v", err)
	}
}

// TestVersionPinAndSequenceContinuity proves the decoder keeps its strict
// record-sequence counter across a window change and a pin: a v1 message and a
// v2 message are decoded in order, and the v2 message must carry the next
// sequence number, not a restarted one.
func TestVersionPinAndSequenceContinuity(t *testing.T) {
	var wire bytes.Buffer
	enc := NewEncoder(&wire, DefaultLimits())
	if err := enc.Encode(Ping{Nonce: 1}); err != nil {
		t.Fatal(err)
	}
	enc.PinVersion(Version2)
	if err := enc.Encode(Ping{Nonce: 2}); err != nil {
		t.Fatal(err)
	}
	// A v2-only type is refused while pinned back to v1 and must not consume a
	// sequence number.
	enc.PinVersion(Version1)
	part := FramePart{Generation: 1, FrameSequence: 1, Keyframe: true, PartIndex: 0, PartCount: 2,
		Rectangles: []Rectangle{{Width: 1, Height: 1, Encoding: EncodingRawBGRA, Pixels: []byte{0, 0, 0, 0xff}}}}
	if err := enc.Encode(part); !errors.Is(err, ErrVersion) {
		t.Fatalf("got %v", err)
	}

	dec := NewDecoder(&wire, DefaultLimits())
	dec.SetVersionWindow(Version1, Version2)
	if got, err := dec.Decode(); err != nil || got != (Ping{Nonce: 1}) {
		t.Fatalf("first decode: %#v, %v", got, err)
	}
	dec.PinVersion(Version2)
	if got, err := dec.Decode(); err != nil || got != (Ping{Nonce: 2}) {
		t.Fatalf("second decode after pin: %#v, %v", got, err)
	}
	if _, err := dec.Decode(); err == nil {
		t.Fatal("expected an error decoding past the end")
	}
}

func TestDecoderVersionWindowRejectsUnsupported(t *testing.T) {
	dec := NewDecoder(bytes.NewReader(headerBytes(Magic, 3, TypePing, 0, 0, 0, 1)), DefaultLimits())
	dec.SetVersionWindow(Version1, Version2)
	if _, err := dec.Decode(); !errors.Is(err, ErrVersion) {
		t.Fatalf("got %v", err)
	}
}

// TestFramePartPerMessageLimit proves a FRAME_PART draws on the pixel payload
// budget, exactly as FRAME does.
func TestFramePartPerMessageLimit(t *testing.T) {
	wire := headerBytes(Magic, Version2, TypeFramePart, 0, 0, MaxPixelPayloadHard+1, 1)
	dec := NewDecoder(bytes.NewReader(wire), DefaultLimits())
	dec.PinVersion(Version2)
	if _, err := dec.Decode(); !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("got %v", err)
	}
}
