package crdt

import (
	"bytes"
	"math"
	"testing"
)

// TestIntDiffOptRleReadRejectsOverflowMagnitude guards that the decoder rejects a
// signed-varint magnitude that exceeds MaxInt64 instead of casting uint64 ->
// int64 (which would wrap to a bogus negative diff and silently desync the
// column). A magnitude this large can only come from corrupt/hostile input — the
// encoder bounds diffs to maxIntDiffOptRleDiff (2^61).
func TestIntDiffOptRleReadRejectsOverflowMagnitude(t *testing.T) {
	buf := new(bytes.Buffer)
	// writeVarIntMag takes a full-range uint64 magnitude; MaxInt64+1 overflows the
	// int64 cast in Read.
	writeVarIntMag(buf, uint64(math.MaxInt64)+1, false)

	d := newIntDiffOptRLEDecoder(buf.Bytes())
	if _, err := d.readValue(); err == nil {
		t.Fatalf("expected error on overflowing magnitude, got nil")
	}
}

// Reference vectors from lib0 IntDiffOptRleEncoder (yjs@13.6.31).
func TestIntDiffOptRleEncoderReference(t *testing.T) {
	cases := []struct {
		name string
		in   []int64
		want []uint8
	}{
		{"sequential", []int64{1, 2, 3, 4}, []uint8{3, 2}},
		{"constant", []int64{5, 5, 5}, []uint8{10, 1, 0}},
		{"decreasing", []int64{10, 7, 4}, []uint8{20, 69, 0}},
		{"zero", []int64{0}, []uint8{0}},
		{"varied", []int64{1, 2, 3, 2, 1}, []uint8{3, 1, 65, 0}},
		{"empty", nil, []uint8{}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newDefaultIntDiffOptRLEEncoder()
			for _, v := range c.in {
				if err := e.writeValue(v); err != nil {
					t.Fatalf("write %d: %v", v, err)
				}
			}
			got, err := e.bytes()
			if err != nil {
				t.Fatalf("ToUint8Array: %v", err)
			}
			if !bytes.Equal(got, c.want) {
				t.Fatalf("encode %v: want %v got %v", c.in, c.want, got)
			}
		})
	}
}

func TestIntDiffOptRleRoundTrip(t *testing.T) {
	cases := [][]int64{
		{1, 2, 3, 4},       // diff = 1 (constant diff)
		{5, 5, 5},          // diff = 0 (constant value)
		{10, 7, 4},         // diff = -3
		{0},                // single zero
		{1, 2, 3, 2, 1},    // varied diffs
		{-5, -3, -1, 1, 3}, // negatives, diff = 2
		{100, 100, 50, 0},  // mixed
		{0, 0, 0, 0, 0},    // all-zero run
		nil,
	}

	for _, in := range cases {
		e := newDefaultIntDiffOptRLEEncoder()
		for _, v := range in {
			if err := e.writeValue(v); err != nil {
				t.Fatalf("in=%v write %d: %v", in, v, err)
			}
		}
		data, err := e.bytes()
		if err != nil {
			t.Fatalf("in=%v ToUint8Array: %v", in, err)
		}

		d := newIntDiffOptRLEDecoder(data)
		for i, want := range in {
			got, err := d.readValue()
			if err != nil {
				t.Fatalf("in=%v idx=%d read err: %v", in, i, err)
			}
			if got != want {
				t.Fatalf("in=%v idx=%d: want %d got %d", in, i, want, got)
			}
		}
	}
}
