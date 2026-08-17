package crdt

import (
	"bytes"
	"testing"
)

// Reference byte vectors in these tests were produced by lib0's RleEncoder
// (yjs@13.6.31) via node, e.g.:
//
//	const e = new enc.RleEncoder(enc.writeUint8)
//	for (const v of [1,1,1,7]) e.write(v)
//	enc.toUint8Array(e) // => [1,2,7]
func TestRleEncoderReference(t *testing.T) {
	cases := []struct {
		name string
		in   []uint8
		want []uint8
	}{
		{"single", []uint8{5}, []uint8{5}},
		{"repeated", []uint8{3, 3, 3, 3}, []uint8{3}},
		{"alternating", []uint8{1, 1, 1, 7}, []uint8{1, 2, 7}},
		{"empty", nil, []uint8{}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newRLEEncoder(writeByte)
			for _, v := range c.in {
				e.writeValue(v)
			}
			got := e.bytes()
			if !bytes.Equal(got, c.want) {
				t.Fatalf("encode %v: want %v got %v", c.in, c.want, got)
			}
		})
	}
}

func TestRleRoundTrip(t *testing.T) {
	cases := [][]uint8{
		{5},
		{3, 3, 3, 3},
		{1, 1, 1, 7},
		{0, 1, 0, 1, 0},
		{255, 255, 0, 0, 0, 42},
		nil,
	}

	for _, in := range cases {
		e := newRLEEncoder(writeByte)
		for _, v := range in {
			e.writeValue(v)
		}
		data := e.bytes()

		d := newRLEDecoder(data, readUint8)
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

// A single value with no trailing count must be readable an unbounded number of
// times (RLE "read forever" branch), matching lib0's count = -1 semantics.
func TestRleReadForever(t *testing.T) {
	e := newRLEEncoder(writeByte)
	e.writeValue(42)
	d := newRLEDecoder(e.bytes(), readUint8)
	for i := 0; i < 100; i++ {
		got, err := d.readValue()
		if err != nil {
			t.Fatalf("read %d err: %v", i, err)
		}
		if got != 42 {
			t.Fatalf("read %d: want 42 got %d", i, got)
		}
	}
}
