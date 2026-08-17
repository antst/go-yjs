package crdt

import (
	"bytes"
	"fmt"
	"math"
)

// maxRleRunCount is the inclusive ceiling on the *actual* decoded run length
// across the Opt-RLE / RLE column decoders. Each decoder reads an encoded count
// (RleDecoder: runLen-1; UintOptRle/IntDiffOptRle: count-2) and must bound
// runLen itself — not the raw encoded value — so the guard rejects exactly the
// runs that would overflow the int/uint cast on 32-bit (or collide with the
// RleDecoder's -1 "repeat forever" sentinel). A run of MaxInt32 is the largest
// value that fits a 32-bit int; it is also unreachable for any real Yjs column
// (which never holds billions of identical entries), so this is a pure safety
// ceiling — it rejects malformed over-bound input and never a legitimate run.
const maxRleRunCount = uint64(math.MaxInt32)

// uint_opt_rle_codec.go ports lib0's UintOptRleEncoder / UintOptRleDecoder used
// by Yjs UpdateEncoderV2 for the client, typeRef and len columns (and as the
// length tracker inside StringEncoder).
//
// Optimized RLE for unsigned integers: a single value is written as a positive
// VarInt; a run is written as the negated value (sign bit set, negative zero
// for value 0) followed by VarUint(count-2). See lib0/encoding.js.

// uintOptRLEEncoder is the optimized unsigned-int RLE encoder.
type uintOptRLEEncoder struct {
	buf   []byte
	s     uint64
	count uint
}

// newDefaultUintOptRLEEncoder creates an empty UintOptRleEncoder.
func newDefaultUintOptRLEEncoder() *uintOptRLEEncoder {
	return &uintOptRLEEncoder{}
}

func newUintOptRleEncoder(capacity int) *uintOptRLEEncoder {
	return &uintOptRLEEncoder{buf: make([]byte, 0, capacity)}
}

// flush emits the pending run, if any.
func (e *uintOptRLEEncoder) flush() {
	if e.count == 0 {
		return
	}

	if e.count == 1 {
		// single value: positive VarInt. Pass the magnitude as uint64 so the
		// full [0, 2^64) range survives (an int64 cast would corrupt s >= 2^63).
		e.buf = appendVarIntMag(e.buf, e.s, false)
	} else {
		// run: negated value (negative zero when s == 0) then count-2.
		e.buf = appendVarIntMag(e.buf, e.s, true)
		e.buf = appendVarUint(e.buf, uint64(e.count-2))
	}
}

// writeValue appends a value to the stream.
func (e *uintOptRLEEncoder) writeValue(v uint64) {
	if e.count > 0 && e.s == v {
		e.count++
		return
	}
	e.flush()
	e.count = 1
	e.s = v
}

// bytes flushes and returns the encoded bytes. Call once.
func (e *uintOptRLEEncoder) bytes() []uint8 {
	e.flush()
	if e.buf == nil {
		return []uint8{}
	}
	return e.buf
}

// uintOptRLEDecoder is the optimized unsigned-int RLE decoder.
type uintOptRLEDecoder struct {
	buf   *bytes.Buffer
	s     uint64
	count uint
}

// newUintOptRLEDecoder creates a decoder over data.
func newUintOptRLEDecoder(data []uint8) *uintOptRLEDecoder {
	return &uintOptRLEDecoder{buf: bytes.NewBuffer(data)}
}

// remaining reports the number of undecoded bytes left in the column buffer. It
// lets callers (UpdateDecoderV2.RemainingLen) measure progress without reaching
// into the unexported buf field.
func (d *uintOptRLEDecoder) remaining() int {
	return d.buf.Len()
}

// readValue returns the next value.
func (d *uintOptRLEDecoder) readValue() (uint64, error) {
	if d.count == 0 {
		mag, negative, err := readVarIntSigned(d.buf)
		if err != nil {
			return 0, err
		}
		d.s = mag
		d.count = 1
		if negative {
			// sign bit set (incl. negative zero) => a run; read count-2.
			n, err := binaryReadUvarint(d.buf)
			if err != nil {
				return 0, err
			}
			// Bound the *actual* count (n+2), not the encoded n, in uint64 before
			// the uint cast. n is the VarUint count-2, so the run is n+2; bounding
			// n alone would admit a run of maxRleRunCount+2, which overflows the
			// cast on 32-bit. Reject when n+2 > maxRleRunCount, i.e.
			// n > maxRleRunCount-2 (maxRleRunCount is well above 2, so the
			// subtraction cannot underflow).
			if n > maxRleRunCount-2 {
				return 0, fmt.Errorf("uint opt rle: run length (encoded %d, actual %d) exceeds bound %d", n, n+2, maxRleRunCount)
			}
			d.count = uint(n) + 2
		}
	}
	d.count--
	return d.s, nil
}
