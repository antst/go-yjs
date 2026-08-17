package crdt

import (
	"bytes"
	"fmt"
	"math"
)

// maxIntDiffOptRleDiff bounds the per-step diff this codec can encode without
// the diff*2 framing overflowing int64. Yjs only ever feeds clock/keyClock
// deltas here, which are tiny; the bound exists purely to turn a corrupt-but-
// silent int64 wrap into a loud failure on pathological input.
const maxIntDiffOptRleDiff = int64(1) << 61

// int_diff_opt_rle_codec.go ports lib0's IntDiffOptRleEncoder /
// IntDiffOptRleDecoder used by Yjs UpdateEncoderV2 for the keyClock, leftClock
// and rightClock columns.
//
// Values are diffed from the previous value, then runs of identical diffs are
// RLE'd. The encoded diff packs the run flag into the LSB:
//
//	encodedDiff = diff*2 + (count == 1 ? 0 : 1)
//
// written as a signed VarInt; a run additionally writes VarUint(count-2). The
// decoder recovers diff via floor(encodedDiff/2) (arithmetic shift), so this
// codec supports only 31-bit diffs — which is exactly how Yjs uses it.

// intDiffOptRLEEncoder is the diff + optimized-RLE integer encoder.
type intDiffOptRLEEncoder struct {
	buf   []byte
	s     int64
	count uint
	diff  int64
}

// newDefaultIntDiffOptRLEEncoder creates an empty encoder.
func newDefaultIntDiffOptRLEEncoder() *intDiffOptRLEEncoder {
	return &intDiffOptRLEEncoder{}
}

func newIntDiffOptRleEncoder(capacity int) *intDiffOptRLEEncoder {
	return &intDiffOptRLEEncoder{buf: make([]byte, 0, capacity)}
}

// flush emits the pending run, if any. It returns an error (instead of
// panicking) when the pending diff is out of encodable range, so the V2 encode
// path can fail gracefully on a hostile clock delta rather than crash. The error
// is threaded up through Write / ToUint8Array and into the encode entry points
// (this rides the content-encode error threading); success-path bytes are
// unchanged.
func (e *intDiffOptRLEEncoder) flush() error {
	if e.count == 0 {
		return nil
	}

	var hasCount int64
	if e.count != 1 {
		hasCount = 1
	}
	// Guard the diff*2 framing against int64 overflow. A diff at/above 2^61 would
	// wrap silently and emit a corrupt stream; reject it loudly instead. This is
	// unreachable for real documents (clocks are small monotonic integers), but a
	// hostile V1 update can carry a >2^61 clock diff that reaches here through
	// ConvertUpdateFormatV1ToV2 (a legal V1 VarUint).
	if e.diff > maxIntDiffOptRleDiff || e.diff < -maxIntDiffOptRleDiff {
		return fmt.Errorf("IntDiffOptRleEncoder: diff %d out of encodable range", e.diff)
	}
	encodedDiff := e.diff*2 + hasCount
	negative := encodedDiff < 0
	magnitude := uint64(encodedDiff)
	if negative {
		magnitude = uint64(-encodedDiff)
	}
	e.buf = appendVarIntMag(e.buf, magnitude, negative)
	if e.count > 1 {
		e.buf = appendVarUint(e.buf, uint64(e.count-2))
	}
	return nil
}

// writeValue appends a value to the stream. It returns the deferred flush error of
// the just-finished run, if the previous run's diff was out of range.
func (e *intDiffOptRLEEncoder) writeValue(v int64) error {
	if e.count > 0 && e.diff == v-e.s {
		e.s = v
		e.count++
		return nil
	}
	if err := e.flush(); err != nil {
		return err
	}
	e.count = 1
	e.diff = v - e.s
	e.s = v
	return nil
}

// writeTrusted appends a clock from a live in-memory document. Number-sized live
// clocks cannot reach the hostile-update overflow case guarded by Write, so the
// full-state encoder can avoid an error check and sticky-error branch per field.
func (e *intDiffOptRLEEncoder) writeTrusted(v int64) {
	diff := v - e.s
	if e.count > 0 && e.diff == diff {
		e.s = v
		e.count++
		return
	}
	if e.count > 0 {
		encodedDiff := e.diff * 2
		if e.count != 1 {
			encodedDiff++
		}
		negative := encodedDiff < 0
		magnitude := uint64(encodedDiff)
		if negative {
			magnitude = uint64(-encodedDiff)
		}
		e.buf = appendVarIntMag(e.buf, magnitude, negative)
		if e.count > 1 {
			e.buf = appendVarUint(e.buf, uint64(e.count-2))
		}
	}
	e.count = 1
	e.diff = diff
	e.s = v
}

// bytes flushes the final run and returns the encoded bytes. Call once.
// It returns an error if the final pending diff is out of encodable range.
func (e *intDiffOptRLEEncoder) bytes() ([]uint8, error) {
	if err := e.flush(); err != nil {
		return nil, err
	}
	if e.buf == nil {
		return []uint8{}, nil
	}
	return e.buf, nil
}

// intDiffOptRLEDecoder is the diff + optimized-RLE integer decoder.
type intDiffOptRLEDecoder struct {
	buf   *bytes.Buffer
	s     int64
	count uint
	diff  int64
}

// newIntDiffOptRLEDecoder creates a decoder over data.
func newIntDiffOptRLEDecoder(data []uint8) *intDiffOptRLEDecoder {
	return &intDiffOptRLEDecoder{buf: bytes.NewBuffer(data)}
}

// remaining reports the number of undecoded bytes left in the column buffer. It
// lets callers (UpdateDecoderV2.RemainingLen) measure progress without reaching
// into the unexported buf field.
func (d *intDiffOptRLEDecoder) remaining() int {
	return d.buf.Len()
}

// readValue returns the next value.
func (d *intDiffOptRLEDecoder) readValue() (int64, error) {
	if d.count == 0 {
		mag, negative, err := readVarIntSigned(d.buf)
		if err != nil {
			return 0, err
		}
		// readVarIntSigned returns the magnitude as a uint64; a corrupt/hostile
		// varint can encode mag > MaxInt64, which would wrap to a bogus negative
		// value on the int64 cast below and silently desync the column. The Yjs
		// encoder only ever emits tiny diffs (bounded by maxIntDiffOptRleDiff), so
		// any magnitude this large is malformed — reject it loudly instead of
		// wrapping (matching the encoder-side overflow guard).
		if mag > math.MaxInt64 {
			return 0, fmt.Errorf("int diff opt rle: magnitude %d overflows int64", mag)
		}
		encodedDiff := int64(mag)
		if negative {
			encodedDiff = -encodedDiff
		}

		hasCount := encodedDiff & 1
		// arithmetic shift = floor division by 2, matching JS Math.floor(diff/2)
		d.diff = encodedDiff >> 1
		d.count = 1
		if hasCount != 0 {
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
				return 0, fmt.Errorf("int diff opt rle: run length (encoded %d, actual n+2) exceeds bound %d", n, maxRleRunCount)
			}
			d.count = uint(n) + 2
		}
	}
	// Detect int64 overflow of the running accumulator before it wraps. A hostile
	// stream can drive d.s past MaxInt64 (or below MinInt64) via a large diff and
	// a long run, silently producing a bogus clock that desyncs the document.
	// Reject it loudly instead of wrapping.
	if (d.diff > 0 && d.s > math.MaxInt64-d.diff) || (d.diff < 0 && d.s < math.MinInt64-d.diff) {
		return 0, fmt.Errorf("int diff opt rle: clock accumulator overflows int64 (s=%d diff=%d)", d.s, d.diff)
	}
	d.s += d.diff
	d.count--
	return d.s, nil
}
