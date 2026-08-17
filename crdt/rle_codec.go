package crdt

import (
	"bytes"
	"fmt"
)

// rle_codec.go ports lib0's RleEncoder / RleDecoder (the uint8-valued variant
// used by Yjs UpdateEncoderV2 for the info and parentInfo columns).
//
// Basic run-length encoding: a value followed, when it changes, by the
// VarUint-encoded count-1 of the run that just ended. See
// lib0/encoding.js#RleEncoder and lib0/decoding.js#RleDecoder.

// rleEncoder is the uint8-valued run-length encoder.
type rleEncoder struct {
	buf     *bytes.Buffer
	w       func(*bytes.Buffer, uint8)
	state   uint8
	count   uint
	started bool
}

// newRLEEncoder creates an RleEncoder that writes values using w.
func newRLEEncoder(w func(*bytes.Buffer, uint8)) *rleEncoder {
	return &rleEncoder{buf: new(bytes.Buffer), w: w}
}

func newRleEncoderWithCapacity(w func(*bytes.Buffer, uint8), capacity int) *rleEncoder {
	return &rleEncoder{buf: bytes.NewBuffer(make([]byte, 0, capacity)), w: w}
}

// writeValue appends a value to the run-length stream.
func (e *rleEncoder) writeValue(v uint8) {
	if e.started && e.state == v {
		e.count++
		return
	}

	if e.count > 0 {
		// flush counter, unless this is the first value (count = 0)
		writeVarUint(e.buf, uint64(e.count-1))
	}
	e.count = 1
	e.w(e.buf, v)
	e.state = v
	e.started = true
}

// bytes returns the encoded bytes.
func (e *rleEncoder) bytes() []uint8 {
	out := e.buf.Bytes()
	if out == nil {
		return []uint8{}
	}
	return out
}

// rleDecoder is the uint8-valued run-length decoder.
type rleDecoder struct {
	buf   *bytes.Buffer
	r     func(*bytes.Buffer) (uint8, error)
	state uint8
	count int
}

// newRLEDecoder creates an RleDecoder over data, reading values using r.
func newRLEDecoder(data []uint8, r func(*bytes.Buffer) (uint8, error)) *rleDecoder {
	return &rleDecoder{buf: bytes.NewBuffer(data), r: r}
}

// remaining reports the number of undecoded bytes left in the column buffer. It
// lets callers (UpdateDecoderV2.RemainingLen) measure progress without reaching
// into the unexported buf field.
func (d *rleDecoder) remaining() int {
	return d.buf.Len()
}

// readValue returns the next value in the run-length stream.
func (d *rleDecoder) readValue() (uint8, error) {
	if d.count == 0 {
		s, err := d.r(d.buf)
		if err != nil {
			return 0, err
		}
		d.state = s

		if d.buf.Len() > 0 {
			n, err := binaryReadUvarint(d.buf)
			if err != nil {
				return 0, err
			}
			// Bound the *actual* run length (n+1), not the encoded n, in uint64
			// before casting to int. n is the VarUint count-1, so the run is n+1;
			// bounding n alone would admit a run of maxRleRunCount+1, which overflows
			// the int cast on 32-bit and can collide with the -1 "repeat forever"
			// sentinel. Reject when n+1 > maxRleRunCount, i.e. n >= maxRleRunCount.
			if n >= maxRleRunCount {
				return 0, fmt.Errorf("rle: run length (encoded %d, actual %d) exceeds bound %d", n, n+1, maxRleRunCount)
			}
			d.count = int(n) + 1
		} else {
			d.count = -1 // read the current value forever
		}
	}
	d.count--
	return d.state, nil
}
