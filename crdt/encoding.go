package crdt

import (
	"bytes"
	"encoding/binary"
	"math"

	"github.com/antst/go-yjs/internal/lib0"
)

// writeByte writes a single uint8 number to the encoder buffer.
func writeByte(encoder *bytes.Buffer, number uint8) {
	_ = encoder.WriteByte(number)
}

// WriteUint8Array writes a byte array to the encoder buffer.
// The first byte is the length of the array, and the following bytes are the array elements.
func writeVarUint8Array(encoder *bytes.Buffer, buf []uint8) {
	lib0.WriteVarUint8Array(encoder, buf)
}

// writeUint8Array writes a byte array to the encoder buffer.
// The first byte is the length of the array, and the following bytes are the array elements.
func writeUint8Array(encoder *bytes.Buffer, buf []uint8) {
	encoder.Write(buf)
}

// writeVarUint writes a variable-length uint64 number to the encoder buffer.
func writeVarUint(encoder *bytes.Buffer, number uint64) {
	lib0.WriteVarUint(encoder, number)
}

// writeVarInt writes a variable-length int64 number to the encoder buffer.
func writeVarInt(encoder *bytes.Buffer, number int) {
	bitSign := 0 // sign bit
	if number < 0 {
		number = -number
		bitSign = bit7
	}

	// bitNext indicates whether there are more bytes to read after this one.
	// If the highest bit is 1, there are more bytes to read.
	bitNext := 0
	if number > bits6 {
		bitNext = bit8
	}

	buf := make([]byte, 1)
	buf[0] = uint8(bitNext|bitSign) | uint8(bits6&number) // [next_flag sign_flag low_6_bits_data]
	encoder.Write(buf)

	number >>= 6

	for number > 0 {
		bitNext := uint8(0)
		if number > bits7 {
			bitNext = bit8
		}

		buf := make([]byte, 1)
		buf[0] = bitNext | uint8(bits7&number) // [next_flag low_7_bits_data]
		encoder.Write(buf)
		number >>= 7
	}
}

// writeFloat32 writes a 4-byte float32 to the encoder buffer using big-endian encoding.
func writeFloat32(encoder *bytes.Buffer, f float32) {
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, math.Float32bits(f))
	encoder.Write(buf)
}

// writeFloat64 writes an 8-byte float64 to the encoder buffer using big-endian encoding.
func writeFloat64(encoder *bytes.Buffer, f float64) {
	bs := make([]byte, 8)
	binary.BigEndian.PutUint64(bs, math.Float64bits(f))
	encoder.Write(bs)
}

// writeInt64 writes an 8-byte int64 to the encoder buffer using big-endian encoding.
func writeInt64(encoder *bytes.Buffer, n int64) {
	bs := make([]byte, 8)
	binary.BigEndian.PutUint64(bs, uint64(n))
	encoder.Write(bs)
}

// writeString writes a variable-length string to the encoder buffer.
func writeString(encoder *bytes.Buffer, str string) error {
	return lib0.WriteString(encoder, str)
}

// writeObject writes an object to the encoder buffer, byte-identically to lib0's
// writeAny object branch: the key count, then each key/value pair with the keys
// emitted in INSERTION order. Object preserves that order (see type_define.go),
// so a multi-key object decoded from a JS-produced update re-encodes to identical
// bytes (the byte-parity property the cross-impl fuzz gate asserts).
func writeObject(encoder *bytes.Buffer, obj Object) error {
	// write the object size.
	writeVarUint(encoder, uint64(obj.Len()))

	// write the object key-value pairs in insertion order.
	var writeErr error
	obj.Range(func(key string, value any) {
		if writeErr != nil {
			return
		}
		if err := writeString(encoder, key); err != nil {
			writeErr = err
			return
		}
		if err := writeAny(encoder, value); err != nil {
			writeErr = err
		}
	})

	return writeErr
}

// writeArray writes an array(any) to the encoder buffer.
func writeArray(encoder *bytes.Buffer, array []any) error {
	// write the array size.
	writeVarUint(encoder, uint64(len(array)))

	// write the array elements.
	for _, value := range array {
		if err := writeAny(encoder, value); err != nil {
			return err
		}
	}

	return nil
}

// isFloat32 reports whether f is exactly representable as a 32-bit float, the
// same test lib0's writeAny uses to decide between tag 124 and tag 123.
func isFloat32(f float64) bool {
	return float64(float32(f)) == f
}

// writeAnyNumber encodes a JS-number-valued quantity following lib0's writeAny
// cascade exactly: an integer with |n| <= bits31 is a varint (125); otherwise a
// float32-representable value is float32 (124); otherwise float64 (123). All Go
// integer "number" types and float64 route through here so the byte stream is
// identical to lib0 regardless of which Go type carried the value.
func writeAnyNumber(encoder *bytes.Buffer, f float64) {
	if f == math.Trunc(f) && !math.IsInf(f, 0) && f >= -float64(bits31) && f <= float64(bits31) {
		writeByte(encoder, 125)
		writeVarInt(encoder, int(f))
		return
	}
	if isFloat32(f) {
		writeByte(encoder, 124)
		writeFloat32(encoder, float32(f))
		return
	}
	writeByte(encoder, 123)
	writeFloat64(encoder, f)
}

// writeAny writes any type to the encoder buffer, byte-identically to lib0's
// writeAny.
func writeAny(encoder *bytes.Buffer, any any) error {
	if isUndefined(any) {
		writeByte(encoder, 127)
		return nil
	}

	if isNull(any) {
		writeByte(encoder, 126)
		return nil
	}

	switch v := any.(type) {
	case string:
		writeByte(encoder, 119)
		if err := writeString(encoder, v); err != nil {
			return err
		}
	case int8:
		writeAnyNumber(encoder, float64(v))
	case int16:
		writeAnyNumber(encoder, float64(v))
	case int32:
		writeAnyNumber(encoder, float64(v))
	case Number: // int
		writeAnyNumber(encoder, float64(v))
	case uint8:
		writeAnyNumber(encoder, float64(v))
	case uint16:
		writeAnyNumber(encoder, float64(v))
	case uint32:
		writeAnyNumber(encoder, float64(v))
	case int64:
		// Genuine bigint: matches lib0's typeof 'bigint' branch (tag 122).
		writeByte(encoder, 122)
		writeInt64(encoder, v)
	case float32:
		writeAnyNumber(encoder, float64(v))
	case float64:
		writeAnyNumber(encoder, v)
	case bool:
		if v {
			writeByte(encoder, 120)
		} else {
			writeByte(encoder, 121)
		}
	case []uint8:
		writeByte(encoder, 116)
		writeVarUint8Array(encoder, v)
	case ArrayAny:
		writeByte(encoder, 117)
		if err := writeArray(encoder, v); err != nil {
			return err
		}
	case Object:
		writeByte(encoder, 118)
		if err := writeObject(encoder, v); err != nil {
			return err
		}
	default:
		// Unknown Go type: lib0 falls through to undefined (127), not null.
		writeByte(encoder, 127)
	}

	return nil
}
