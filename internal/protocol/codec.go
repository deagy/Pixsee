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
}

func NewEncoder(w io.Writer, limits Limits) *Encoder {
	return &Encoder{w: w, limits: limits.bounded()}
}
func (e *Encoder) Encode(message Message) error {
	if err := ValidateMessage(message, e.limits); err != nil {
		return err
	}
	payload, err := marshal(message)
	if err != nil {
		return err
	}
	limit := e.limits.MaxControlPayload
	if message.Type() == TypeFrame {
		limit = e.limits.MaxPixelPayload
	}
	if uint64(len(payload)) > uint64(limit) {
		return ErrMessageTooLarge
	}
	e.sequence++
	header := make([]byte, HeaderSize)
	copy(header, Magic)
	binary.BigEndian.PutUint16(header[4:6], Version1)
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
	r        io.Reader
	limits   Limits
	sequence uint64
}

func NewDecoder(r io.Reader, limits Limits) *Decoder {
	return &Decoder{r: r, limits: limits.bounded()}
}
func (d *Decoder) Decode() (Message, error) {
	header := make([]byte, HeaderSize)
	if _, err := io.ReadFull(d.r, header); err != nil {
		return nil, err
	}
	if string(header[:4]) != Magic {
		return nil, fmt.Errorf("%w: magic", ErrMalformed)
	}
	if binary.BigEndian.Uint16(header[4:6]) != Version1 {
		return nil, ErrVersion
	}
	t := Type(binary.BigEndian.Uint16(header[6:8]))
	if !t.valid() {
		return nil, ErrUnknownType
	}
	if binary.BigEndian.Uint16(header[8:10]) != 0 || binary.BigEndian.Uint16(header[10:12]) != 0 {
		return nil, fmt.Errorf("%w: flags or reserved", ErrMalformed)
	}
	length := binary.BigEndian.Uint32(header[12:16])
	limit := d.limits.MaxControlPayload
	if t == TypeFrame {
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
		w.u16(uint16(len(v.Rectangles)))
		for _, q := range v.Rectangles {
			w.u32(q.X)
			w.u32(q.Y)
			w.u32(q.Width)
			w.u32(q.Height)
			w.u16(uint16(q.Encoding))
			w.u32(uint32(len(q.Pixels)))
			w.Write(q.Pixels)
		}
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
		if e == nil && n > l.MaxRectangles {
			e = ErrMessageTooLarge
		}
		if e == nil {
			v.Rectangles = make([]Rectangle, n)
			for i := range v.Rectangles {
				q := &v.Rectangles[i]
				q.X, e = r.u32()
				if e == nil {
					q.Y, e = r.u32()
				}
				if e == nil {
					q.Width, e = r.u32()
				}
				if e == nil {
					q.Height, e = r.u32()
				}
				var x uint16
				if e == nil {
					x, e = r.u16()
					q.Encoding = Encoding(x)
				}
				var z uint32
				if e == nil {
					z, e = r.u32()
				}
				if e == nil && uint64(z) > uint64(r.Len()) {
					e = ErrMalformed
				}
				if e == nil {
					q.Pixels = make([]byte, z)
					_, e = io.ReadFull(r, q.Pixels)
				}
				if e != nil {
					break
				}
			}
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
