package protocol

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
)

type Encoder struct {
	w        io.Writer
	limits   Limits
	sequence uint64
	version  uint16
}

func NewEncoder(w io.Writer, limits Limits) *Encoder {
	return &Encoder{w: w, limits: limits.bounded(), version: Version1}
}

// PinVersion sets the record-envelope version the encoder stamps on every
// subsequent message. The default is Version1, preserving v1 byte behavior for
// callers that never pin; a session pins to the negotiated version once
// SERVER_HELLO fixes it. The record sequence counter is unaffected.
func (e *Encoder) PinVersion(v uint16) { e.version = v }

func (e *Encoder) Encode(message Message) error {
	if err := ValidateMessage(message, e.limits); err != nil {
		return err
	}
	// v2-only message types cannot ride a v1 envelope: a v1 receiver would
	// decode the type number as unknown.
	if requiresVersion2(message.Type()) && e.version < Version2 {
		return ErrVersion
	}
	payload, err := marshal(message)
	if err != nil {
		return err
	}
	// ValidateMessage has already bounded the payload arithmetically for pixel
	// messages, so this is the only marshal Encode performs; re-checking the
	// marshaled length keeps the exact wire-size invariant (and catches any
	// arithmetic/marshal drift) without a second copy.
	limit := e.limits.MaxControlPayload
	if usesPixelPayload(message.Type()) {
		limit = e.limits.MaxPixelPayload
	}
	if uint64(len(payload)) > uint64(limit) {
		return ErrMessageTooLarge
	}
	e.sequence++
	header := make([]byte, HeaderSize)
	copy(header, Magic)
	binary.BigEndian.PutUint16(header[4:6], e.version)
	binary.BigEndian.PutUint16(header[6:8], uint16(message.Type()))
	binary.BigEndian.PutUint32(header[12:16], uint32(len(payload)))
	binary.BigEndian.PutUint64(header[16:24], e.sequence)
	if err := writeFull(e.w, header); err != nil {
		return err
	}
	return writeFull(e.w, payload)
}

func writeFull(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}

type Decoder struct {
	r          io.Reader
	limits     Limits
	sequence   uint64
	minVersion uint16
	maxVersion uint16
}

func NewDecoder(r io.Reader, limits Limits) *Decoder {
	return &Decoder{r: r, limits: limits.bounded(), minVersion: Version1, maxVersion: Version1}
}

// SetVersionWindow widens (or narrows) the inclusive range of record-envelope
// versions the decoder accepts. A v2-capable peer calls
// SetVersionWindow(Version1, Version2) while it is still awaiting the HELLO
// exchange; the default is v1-only, preserving v1 byte behavior.
func (d *Decoder) SetVersionWindow(min, max uint16) { d.minVersion, d.maxVersion = min, max }

// PinVersion pins the decoder to a single record-envelope version once
// SERVER_HELLO fixes the negotiation. It does not touch the strict
// record-sequence counter, so a pinned session continues to enforce sequence
// continuity across the transition.
func (d *Decoder) PinVersion(v uint16) { d.minVersion, d.maxVersion = v, v }

func (d *Decoder) Decode() (Message, error) {
	header := make([]byte, HeaderSize)
	if _, err := io.ReadFull(d.r, header); err != nil {
		return nil, err
	}
	if string(header[:4]) != Magic {
		return nil, fmt.Errorf("%w: magic", ErrMalformed)
	}
	headerVersion := binary.BigEndian.Uint16(header[4:6])
	if headerVersion < d.minVersion || headerVersion > d.maxVersion {
		return nil, ErrVersion
	}
	t := Type(binary.BigEndian.Uint16(header[6:8]))
	if !t.valid() {
		return nil, ErrUnknownType
	}
	// A v2-only message type must not arrive on a v1 envelope.
	if requiresVersion2(t) && headerVersion < Version2 {
		return nil, ErrVersion
	}
	if binary.BigEndian.Uint16(header[8:10]) != 0 || binary.BigEndian.Uint16(header[10:12]) != 0 {
		return nil, fmt.Errorf("%w: flags or reserved", ErrMalformed)
	}
	length := binary.BigEndian.Uint32(header[12:16])
	limit := d.limits.MaxControlPayload
	if usesPixelPayload(t) {
		limit = d.limits.MaxPixelPayload
	}
	if length > limit {
		return nil, ErrMessageTooLarge
	}
	sequence := binary.BigEndian.Uint64(header[16:24])
	if sequence != d.sequence+1 {
		return nil, ErrSequence
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(d.r, payload); err != nil {
		return nil, err
	}
	message, err := unmarshal(t, payload, d.limits)
	if err != nil {
		return nil, err
	}
	d.sequence = sequence
	return message, nil
}

type writer struct{ bytes.Buffer }

func (w *writer) u8(v uint8)    { w.WriteByte(v) }
func (w *writer) u16(v uint16)  { _ = binary.Write(&w.Buffer, binary.BigEndian, v) }
func (w *writer) i16(v int16)   { _ = binary.Write(&w.Buffer, binary.BigEndian, v) }
func (w *writer) u32(v uint32)  { _ = binary.Write(&w.Buffer, binary.BigEndian, v) }
func (w *writer) u64(v uint64)  { _ = binary.Write(&w.Buffer, binary.BigEndian, v) }
func (w *writer) text(v string) { w.u16(uint16(len(v))); w.WriteString(v) }

type reader struct{ *bytes.Reader }

func newReader(p []byte) *reader       { return &reader{bytes.NewReader(p)} }
func (r *reader) u8() (uint8, error)   { return r.ReadByte() }
func (r *reader) u16() (uint16, error) { var v uint16; return v, binary.Read(r, binary.BigEndian, &v) }
func (r *reader) i16() (int16, error)  { var v int16; return v, binary.Read(r, binary.BigEndian, &v) }
func (r *reader) u32() (uint32, error) { var v uint32; return v, binary.Read(r, binary.BigEndian, &v) }
func (r *reader) u64() (uint64, error) { var v uint64; return v, binary.Read(r, binary.BigEndian, &v) }
func (r *reader) text() (string, error) {
	n, e := r.u16()
	if e != nil {
		return "", e
	}
	b := make([]byte, n)
	_, e = io.ReadFull(r, b)
	return string(b), e
}

const (
	// frameHeaderSize is the fixed marshaled size of a FRAME payload before its
	// rectangle list: generation, frame sequence and base sequence (8+8+8), the
	// keyframe flag (1) and the rectangle count (2). FRAME_PART adds a part
	// index and part count on top of it (see framePartHeaderSize).
	frameHeaderSize = uint64(27)
	// maxPayloadSizeSentinel saturates an arithmetic payload-size sum so an
	// absurd rectangle list cannot wrap uint64 and understate its size. Any
	// value this large is far above every configured limit, so a saturated
	// result still compares correctly against the per-message bound.
	maxPayloadSizeSentinel = ^uint64(0)
)

// pixelPayloadSize returns the exact marshaled payload length of a pixel-bearing
// message (FRAME or FRAME_PART), computed arithmetically so a caller can bound it
// without marshaling the message and copying every rectangle's pixels. The
// ok result is false for any type without an arithmetic size; the count is
// guarded by validateFrameHeader (MaxRectangles) before this is consulted, so it
// never exceeds the uint16 the wire format can index.
func pixelPayloadSize(m Message) (uint64, bool) {
	switch v := m.(type) {
	case Frame:
		return rectListPayloadSize(frameHeaderSize, v.Rectangles), true
	case FramePart:
		return rectListPayloadSize(framePartHeaderSize, v.Rectangles), true
	default:
		return 0, false
	}
}

// rectListPayloadSize returns headerSize plus each rectangle's fixed metadata
// and pixel bytes, saturating at maxPayloadSizeSentinel instead of overflowing.
// It matches the bytes writeRectangles produces for a rectangle list within the
// uint16 count the wire format can carry.
func rectListPayloadSize(headerSize uint64, rectangles []Rectangle) uint64 {
	size := headerSize
	for _, rect := range rectangles {
		// frameRectMetadataSize + len(Pixels) cannot overflow uint64: len is at
		// most MaxInt, well under 2^63, and the metadata is 22 bytes.
		increment := frameRectMetadataSize + uint64(len(rect.Pixels))
		if increment > maxPayloadSizeSentinel-size {
			return maxPayloadSizeSentinel
		}
		size += increment
	}
	return size
}

// writeRectangles serializes a rectangle list exactly as FRAME and FRAME_PART
// both define it: count, then x/y/width/height, encoding, pixel length, pixels.
func writeRectangles(w *writer, rectangles []Rectangle) {
	w.u16(uint16(len(rectangles)))
	for _, q := range rectangles {
		w.u32(q.X)
		w.u32(q.Y)
		w.u32(q.Width)
		w.u32(q.Height)
		w.u16(uint16(q.Encoding))
		w.u32(uint32(len(q.Pixels)))
		w.Write(q.Pixels)
	}
}

// readRectangles deserializes the rectangle list shared by FRAME and
// FRAME_PART. It bounds the count before allocating and rejects a rectangle
// whose declared pixel length exceeds the bytes actually remaining.
func readRectangles(r *reader, n uint16, l Limits) ([]Rectangle, error) {
	if n > l.MaxRectangles {
		return nil, ErrMessageTooLarge
	}
	rectangles := make([]Rectangle, n)
	for i := range rectangles {
		q := &rectangles[i]
		var e error
		if q.X, e = r.u32(); e != nil {
			return nil, e
		}
		if q.Y, e = r.u32(); e != nil {
			return nil, e
		}
		if q.Width, e = r.u32(); e != nil {
			return nil, e
		}
		if q.Height, e = r.u32(); e != nil {
			return nil, e
		}
		var x uint16
		if x, e = r.u16(); e != nil {
			return nil, e
		}
		q.Encoding = Encoding(x)
		var z uint32
		if z, e = r.u32(); e != nil {
			return nil, e
		}
		if uint64(z) > uint64(r.Len()) {
			return nil, ErrMalformed
		}
		q.Pixels = make([]byte, z)
		if _, e = io.ReadFull(r, q.Pixels); e != nil {
			return nil, e
		}
	}
	return rectangles, nil
}

func marshal(m Message) ([]byte, error) {
	w := &writer{}
	switch v := m.(type) {
	case Auth:
		w.u16(v.Version)
		w.Write(v.Token[:])
	case ClientHello:
		w.u16(v.MinVersion)
		w.u16(v.MaxVersion)
		w.u64(v.Capabilities)
	case ServerHello:
		w.u16(v.Version)
		w.u64(v.Capabilities)
	case DisplayConfig:
		w.u64(v.Generation)
		w.u32(v.Width)
		w.u32(v.Height)
		w.u32(v.PhysicalWidthMM)
		w.u32(v.PhysicalHeightMM)
		w.u16(uint16(v.PixelFormat))
	case Frame:
		w.u64(v.Generation)
		w.u64(v.FrameSequence)
		w.u64(v.BaseFrameSequence)
		if v.Keyframe {
			w.u8(1)
		} else {
			w.u8(0)
		}
		writeRectangles(w, v.Rectangles)
	case FramePart:
		w.u64(v.Generation)
		w.u64(v.FrameSequence)
		w.u64(v.BaseFrameSequence)
		if v.Keyframe {
			w.u8(1)
		} else {
			w.u8(0)
		}
		w.u16(v.PartIndex)
		w.u16(v.PartCount)
		writeRectangles(w, v.Rectangles)
	case KeyframeRequest:
		w.u64(v.Generation)
	case Key:
		w.u64(v.Generation)
		w.u64(v.InputSequence)
		w.u16(v.Usage)
		w.u8(uint8(v.Action))
		w.u8(v.Modifiers)
	case PointerMove:
		w.u64(v.Generation)
		w.u64(v.InputSequence)
		w.u32(v.X)
		w.u32(v.Y)
	case PointerButton:
		w.u64(v.Generation)
		w.u64(v.InputSequence)
		w.u8(uint8(v.Button))
		w.u8(uint8(v.Action))
	case PointerWheel:
		w.u64(v.Generation)
		w.u64(v.InputSequence)
		w.i16(v.Horizontal)
		w.i16(v.Vertical)
	case FocusLost:
		w.u64(v.Generation)
		w.u64(v.InputSequence)
	case Ping:
		w.u64(v.Nonce)
	case Pong:
		w.u64(v.Nonce)
	case ErrorMessage:
		w.u16(uint16(v.Code))
		w.text(v.Diagnostic)
	case Close:
		w.u16(uint16(v.Code))
		w.text(v.Reason)
	default:
		return nil, ErrUnknownType
	}
	return w.Bytes(), nil
}

func unmarshal(t Type, p []byte, l Limits) (Message, error) {
	r := newReader(p)
	var m Message
	var e error
	switch t {
	case TypeAuth:
		var v Auth
		v.Version, e = r.u16()
		if e == nil {
			_, e = io.ReadFull(r, v.Token[:])
		}
		m = v
	case TypeClientHello:
		var v ClientHello
		v.MinVersion, e = r.u16()
		if e == nil {
			v.MaxVersion, e = r.u16()
		}
		if e == nil {
			v.Capabilities, e = r.u64()
		}
		m = v
	case TypeServerHello:
		var v ServerHello
		v.Version, e = r.u16()
		if e == nil {
			v.Capabilities, e = r.u64()
		}
		m = v
	case TypeDisplayConfig:
		var v DisplayConfig
		v.Generation, e = r.u64()
		if e == nil {
			v.Width, e = r.u32()
		}
		if e == nil {
			v.Height, e = r.u32()
		}
		if e == nil {
			v.PhysicalWidthMM, e = r.u32()
		}
		if e == nil {
			v.PhysicalHeightMM, e = r.u32()
		}
		var x uint16
		if e == nil {
			x, e = r.u16()
			v.PixelFormat = PixelFormat(x)
		}
		m = v
	case TypeFrame:
		var v Frame
		v.Generation, e = r.u64()
		if e == nil {
			v.FrameSequence, e = r.u64()
		}
		if e == nil {
			v.BaseFrameSequence, e = r.u64()
		}
		var key uint8
		if e == nil {
			key, e = r.u8()
			v.Keyframe = key == 1
			if key > 1 {
				e = ErrMalformed
			}
		}
		var n uint16
		if e == nil {
			n, e = r.u16()
		}
		if e == nil {
			v.Rectangles, e = readRectangles(r, n, l)
		}
		m = v
	case TypeFramePart:
		var v FramePart
		v.Generation, e = r.u64()
		if e == nil {
			v.FrameSequence, e = r.u64()
		}
		if e == nil {
			v.BaseFrameSequence, e = r.u64()
		}
		var key uint8
		if e == nil {
			key, e = r.u8()
			v.Keyframe = key == 1
			if key > 1 {
				e = ErrMalformed
			}
		}
		if e == nil {
			v.PartIndex, e = r.u16()
		}
		if e == nil {
			v.PartCount, e = r.u16()
		}
		var n uint16
		if e == nil {
			n, e = r.u16()
		}
		if e == nil {
			v.Rectangles, e = readRectangles(r, n, l)
		}
		m = v
	case TypeKeyframeRequest:
		var v KeyframeRequest
		v.Generation, e = r.u64()
		m = v
	case TypeKey:
		var v Key
		v.Generation, e = r.u64()
		if e == nil {
			v.InputSequence, e = r.u64()
		}
		if e == nil {
			v.Usage, e = r.u16()
		}
		var x uint8
		if e == nil {
			x, e = r.u8()
			v.Action = Action(x)
		}
		if e == nil {
			v.Modifiers, e = r.u8()
		}
		m = v
	case TypePointerMove:
		var v PointerMove
		v.Generation, e = r.u64()
		if e == nil {
			v.InputSequence, e = r.u64()
		}
		if e == nil {
			v.X, e = r.u32()
		}
		if e == nil {
			v.Y, e = r.u32()
		}
		m = v
	case TypePointerButton:
		var v PointerButton
		v.Generation, e = r.u64()
		if e == nil {
			v.InputSequence, e = r.u64()
		}
		var x uint8
		if e == nil {
			x, e = r.u8()
			v.Button = Button(x)
		}
		if e == nil {
			x, e = r.u8()
			v.Action = Action(x)
		}
		m = v
	case TypePointerWheel:
		var v PointerWheel
		v.Generation, e = r.u64()
		if e == nil {
			v.InputSequence, e = r.u64()
		}
		if e == nil {
			v.Horizontal, e = r.i16()
		}
		if e == nil {
			v.Vertical, e = r.i16()
		}
		m = v
	case TypeFocusLost:
		var v FocusLost
		v.Generation, e = r.u64()
		if e == nil {
			v.InputSequence, e = r.u64()
		}
		m = v
	case TypePing:
		var v Ping
		v.Nonce, e = r.u64()
		m = v
	case TypePong:
		var v Pong
		v.Nonce, e = r.u64()
		m = v
	case TypeError:
		var v ErrorMessage
		var x uint16
		x, e = r.u16()
		v.Code = ErrorCode(x)
		if e == nil {
			v.Diagnostic, e = r.text()
		}
		m = v
	case TypeClose:
		var v Close
		var x uint16
		x, e = r.u16()
		v.Code = CloseCode(x)
		if e == nil {
			v.Reason, e = r.text()
		}
		m = v
	default:
		return nil, ErrUnknownType
	}
	if e != nil || r.Len() != 0 {
		return nil, fmt.Errorf("%w: payload", ErrMalformed)
	}
	if e = ValidateMessage(m, l); e != nil {
		return nil, e
	}
	return m, nil
}
