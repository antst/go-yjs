package crdt

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"
)

// These tests guard the run-length overflow bound (maxRleRunCount) added to the
// RLE / Opt-RLE column decoders. A run count from corrupt/hostile input that
// exceeds the bound is rejected with an error instead of being cast to int/uint
// (which would wrap negative on 32-bit platforms or collide with RleDecoder's -1
// "repeat forever" sentinel). Each test crafts a minimal column buffer whose run
// count is MaxInt32+1 and asserts Read errors.

func appendUvarint(b []byte, v uint64) []byte {
	var tmp [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(tmp[:], v)
	return append(b, tmp[:n]...)
}

func TestRleDecoderRejectsOverflowRunLength(t *testing.T) {
	// RleDecoder.Read: first byte is the value, then the run-count varint.
	buf := []byte{7}                                  // state value
	buf = appendUvarint(buf, uint64(math.MaxInt32)+1) // run count > bound
	d := newRLEDecoder(buf, readUint8)
	if _, err := d.readValue(); err == nil {
		t.Fatalf("expected error on overflowing RLE run length, got nil")
	}
}

func TestUintOptRleDecoderRejectsOverflowRunLength(t *testing.T) {
	// UintOptRleDecoder.Read: a negative-zero signed varint (0x40 = sign bit set,
	// magnitude 0) signals "a run follows", then the run-count varint.
	buf := []byte{0x40}
	buf = appendUvarint(buf, uint64(math.MaxInt32)+1)
	d := newUintOptRLEDecoder(buf)
	if _, err := d.readValue(); err == nil {
		t.Fatalf("expected error on overflowing UintOptRle run length, got nil")
	}
}

func TestIntDiffOptRleDecoderRejectsOverflowRunLength(t *testing.T) {
	// IntDiffOptRleDecoder.Read: a signed varint whose encodedDiff has the LSB set
	// (hasCount) signals "a run follows". encodedDiff = 1 -> magnitude 1, positive
	// (single byte 0x01), then the run-count varint.
	buf := new(bytes.Buffer)
	writeVarIntSigned(buf, 1) // encodedDiff = 1 => hasCount set
	data := appendUvarint(buf.Bytes(), uint64(math.MaxInt32)+1)
	d := newIntDiffOptRLEDecoder(data)
	if _, err := d.readValue(); err == nil {
		t.Fatalf("expected error on overflowing IntDiffOptRle run length, got nil")
	}
}

// --- Off-by-one boundary regressions -----------------------------------------
//
// The guards above bound the run length, but originally bounded the *encoded*
// value (RleDecoder: runLen-1; UintOptRle/IntDiffOptRle: count-2) rather than
// the actual run length. That admitted runs of maxRleRunCount+1 / +2, which
// overflow the int/uint cast on 32-bit. These tests pin the exact boundary: a
// run length AT maxRleRunCount must decode, one OVER it must error. A single
// Read() sets up the run counter and returns one element without materializing
// the (multi-billion-entry) run, so it is enough to assert on the first Read.

func TestRleDecoderAcceptsMaximalRunRejectsOverBound(t *testing.T) {
	// RleDecoder encodes runLen as VarUint(runLen-1). The actual run is encoded+1.
	// At-bound: runLen == maxRleRunCount -> encoded = maxRleRunCount-1 (must pass).
	// Over-bound: runLen == maxRleRunCount+1 -> encoded = maxRleRunCount (must err).
	atBound := []byte{7}
	atBound = appendUvarint(atBound, maxRleRunCount-1)
	d := newRLEDecoder(atBound, readUint8)
	if v, err := d.readValue(); err != nil {
		t.Fatalf("maximal RLE run (length %d) should decode, got err: %v", maxRleRunCount, err)
	} else if v != 7 {
		t.Fatalf("maximal RLE run: want value 7, got %d", v)
	}

	overBound := []byte{7}
	overBound = appendUvarint(overBound, maxRleRunCount) // runLen = maxRleRunCount+1
	d = newRLEDecoder(overBound, readUint8)
	if _, err := d.readValue(); err == nil {
		t.Fatalf("RLE run length %d (one over bound) should error, got nil", maxRleRunCount+1)
	}
}

func TestUintOptRleDecoderAcceptsMaximalRunRejectsOverBound(t *testing.T) {
	// UintOptRleDecoder encodes count as VarUint(count-2). The actual run is enc+2.
	// At-bound: count == maxRleRunCount -> encoded = maxRleRunCount-2 (must pass).
	// Over-bound: count == maxRleRunCount+1 -> encoded = maxRleRunCount-1 (must err).
	atBound := []byte{0x40} // negative-zero marker => "a run follows"
	atBound = appendUvarint(atBound, maxRleRunCount-2)
	d := newUintOptRLEDecoder(atBound)
	if v, err := d.readValue(); err != nil {
		t.Fatalf("maximal UintOptRle run (count %d) should decode, got err: %v", maxRleRunCount, err)
	} else if v != 0 {
		t.Fatalf("maximal UintOptRle run: want value 0, got %d", v)
	}

	overBound := []byte{0x40}
	overBound = appendUvarint(overBound, maxRleRunCount-1) // count = maxRleRunCount+1
	d = newUintOptRLEDecoder(overBound)
	if _, err := d.readValue(); err == nil {
		t.Fatalf("UintOptRle run count %d (one over bound) should error, got nil", maxRleRunCount+1)
	}
}

func TestIntDiffOptRleDecoderAcceptsMaximalRunRejectsOverBound(t *testing.T) {
	// IntDiffOptRleDecoder encodes count as VarUint(count-2). The actual run is enc+2.
	// At-bound: count == maxRleRunCount -> encoded = maxRleRunCount-2 (must pass).
	// Over-bound: count == maxRleRunCount+1 -> encoded = maxRleRunCount-1 (must err).
	atBuf := new(bytes.Buffer)
	writeVarIntSigned(atBuf, 1) // encodedDiff = 1 => hasCount set
	atBound := appendUvarint(atBuf.Bytes(), maxRleRunCount-2)
	d := newIntDiffOptRLEDecoder(atBound)
	if _, err := d.readValue(); err != nil {
		t.Fatalf("maximal IntDiffOptRle run (count %d) should decode, got err: %v", maxRleRunCount, err)
	}

	overBuf := new(bytes.Buffer)
	writeVarIntSigned(overBuf, 1)
	overBound := appendUvarint(overBuf.Bytes(), maxRleRunCount-1) // count = maxRleRunCount+1
	d = newIntDiffOptRLEDecoder(overBound)
	if _, err := d.readValue(); err == nil {
		t.Fatalf("IntDiffOptRle run count %d (one over bound) should error, got nil", maxRleRunCount+1)
	}
}
