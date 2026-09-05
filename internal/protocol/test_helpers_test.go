package protocol

import "encoding/binary"

func headerBytes(magic string, version uint16, typ Type, flags, reserved uint16, length uint32, sequence uint64) []byte {
	b := make([]byte, HeaderSize)
	copy(b[:4], magic)
	binary.BigEndian.PutUint16(b[4:6], version)
	binary.BigEndian.PutUint16(b[6:8], uint16(typ))
	binary.BigEndian.PutUint16(b[8:10], flags)
	binary.BigEndian.PutUint16(b[10:12], reserved)
	binary.BigEndian.PutUint32(b[12:16], length)
	binary.BigEndian.PutUint64(b[16:24], sequence)
	return b
}
