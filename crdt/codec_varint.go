package crdt

import (
	"bytes"
	"encoding/binary"
)

// codec_varint.go holds the sign+magnitude varint helpers shared by ALL the V2
// column codecs (IntDiffOptRle and UintOptRle both encode/decode through
// writeVarIntMag / readVarIntSigned — the logic lives here once, not copied per
// codec).
//
// These are deliberately kept separate from encoding.go's WriteVarInt /
// decoding.go's ReadVarInt, which cannot be reused without breaking byte-parity:
//
//   - The column codecs need byte-exact parity with lib0, including a subtlety
//     ReadVarInt/WriteVarInt drop: the negative flag on a zero value. lib0's
//     UintOptRle/IntDiffOptRle encoders rely on writeVarInt(-0) setting the sign
//     bit even though the magnitude is 0 (the run-vs-single signal), and the
//     decoders detect it via isNegativeZero. readVarIntSigned therefore returns
//     the magnitude together with the raw sign flag instead of a combined signed
//     int.
//   - writeVarIntMag takes the magnitude as a full-range uint64 so values in
//     [2^63, 2^64) survive; WriteVarInt takes an int and would corrupt them.
//
// readVarIntSigned shares ReadVarInt's overflow discipline (a capped byte count
// and an explicit overflow error) so an overlong/hostile varint can't silently
// wrap — see the guard below.

// binaryReadUvarint reads a lib0/protobuf-style VarUint from buf.
func binaryReadUvarint(buf *bytes.Buffer) (uint64, error) {
	return binary.ReadUvarint(buf)
}

// writeVarIntSigned writes an int64 using lib0's writeVarInt scheme. When
// negative is true the sign bit is set even if num == 0 (negative zero), so a
// value of 0 can still carry a "count follows" signal for the Opt-RLE codecs.
func writeVarIntSigned(buf *bytes.Buffer, num int64) {
	negative := num < 0
	if negative {
		num = -num
	}
	writeVarIntMag(buf, uint64(num), negative)
}

// writeVarIntMag writes a sign + magnitude pair using lib0's writeVarInt scheme,
// taking the magnitude as a full-range uint64. This is the unsigned-safe core
// used by the Opt-RLE codecs: it must not lose the high bit of values in the
// [2^63, 2^64) range, which an int64 cast would flip to negative.
func writeVarIntMag(buf *bytes.Buffer, num uint64, negative bool) {
	first := byte(bits6 & num)
	if num > uint64(bits6) {
		first |= byte(bit8)
	}
	if negative {
		first |= byte(bit7)
	}
	buf.WriteByte(first)
	num >>= 6

	for num > 0 {
		b := byte(bits7 & num)
		if num > uint64(bits7) {
			b |= byte(bit8)
		}
		buf.WriteByte(b)
		num >>= 7
	}
}

// appendVarIntMag is the slice-backed form used by the hot V2 column encoders.
// It emits exactly the same sign-and-magnitude bytes as writeVarIntMag without
// a bytes.Buffer method call for every one- or two-byte field.
func appendVarIntMag(dst []byte, num uint64, negative bool) []byte {
	first := byte(bits6 & num)
	if negative {
		first |= byte(bit7)
	}
	if num <= uint64(bits6) {
		return append(dst, first)
	}
	first |= byte(bit8)
	if num < 1<<13 {
		return append(dst, first, byte(num>>6))
	}
	dst = append(dst, first, byte(num>>6)|byte(bit8))
	num >>= 13

	for num > 0 {
		b := byte(bits7 & num)
		if num > uint64(bits7) {
			b |= byte(bit8)
		}
		dst = append(dst, b)
		num >>= 7
	}
	return dst
}

// readVarIntSigned reads a lib0 writeVarInt value, returning the unsigned
// magnitude and whether the sign bit was set (preserving negative zero).
//
// The byte stream is capped and checked for overflow, mirroring ReadVarInt's
// guard in decoding.go. Without it a hostile overlong varint would (a) read
// forever-ish and (b) wrap `mult *= 128` to 0, producing a bogus magnitude that
// silently desyncs the column. The first byte carries 6 magnitude bits, each
// continuation byte 7 more; a 64-bit magnitude fits in at most 9 continuation
// bytes (6 + 9*7 = 69 >= 64). Past that, or if a continuation contributes bits
// above bit 63, the value cannot be represented — reject it loudly.
func readVarIntSigned(buf *bytes.Buffer) (magnitude uint64, negative bool, err error) {
	r, err := buf.ReadByte()
	if err != nil {
		return 0, false, err
	}

	num := uint64(r & bits6)
	negative = (r & bit7) > 0
	shift := uint(6) // bit position the next continuation byte's 7 bits start at

	if r&bit8 == 0 {
		return num, negative, nil
	}

	// At most 9 continuation bytes can contribute to a 64-bit magnitude.
	for i := 0; i < 9; i++ {
		r, err = buf.ReadByte()
		if err != nil {
			return num, negative, err
		}
		chunk := uint64(r & bits7)
		// Reject any bit that would land at or beyond bit 64 (would be lost on the
		// shift / overflow the uint64). shift maxes at 6+8*7 = 62 on the 9th byte,
		// where only the low 2 bits are representable.
		if shift >= 64 || (chunk>>(64-shift)) != 0 {
			return num, negative, errOverflow
		}
		num += chunk << shift
		shift += 7
		if r < bit8 {
			return num, negative, nil
		}
	}

	// A 10th continuation byte (or more) cannot fit in 64 bits.
	return num, negative, errOverflow
}
