package crdt

import (
	"bytes"
	"testing"
)

// Reference vectors from lib0 UintOptRleEncoder (yjs@13.6.31).
func TestUintOptRleEncoderReference(t *testing.T) {
	cases := []struct {
		name string
		in   []uint64
		want []uint8
	}{
		{"single", []uint64{7}, []uint8{7}},
		{"mixed_with_run", []uint64{1, 2, 3, 3, 3}, []uint8{1, 2, 67, 1}},
		{"zero_single", []uint64{0}, []uint8{0}},
		{"zero_run", []uint64{0, 0, 0}, []uint8{64, 1}},
		{"large_run", []uint64{300, 300}, []uint8{236, 4, 0}},
		{"empty", nil, []uint8{}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newDefaultUintOptRLEEncoder()
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

func TestUintOptRleRoundTrip(t *testing.T) {
	cases := [][]uint64{
		{7},
		{1, 2, 3, 3, 3},
		{0},
		{0, 0, 0},
		{300, 300},
		{1, 1, 1, 1, 2, 2, 3},
		{0, 5, 0, 5, 0},
		{1<<32 + 7, 1<<32 + 7},
		nil,
	}

	for _, in := range cases {
		e := newDefaultUintOptRLEEncoder()
		for _, v := range in {
			e.writeValue(v)
		}
		data := e.bytes()

		d := newUintOptRLEDecoder(data)
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
