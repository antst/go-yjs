package crdt

import (
	"bytes"
	"testing"
)

// Reference vectors from lib0 StringEncoder (yjs@13.6.31).
func TestStringEncoderReference(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []uint8
	}{
		// "abcdefgh" var-string (len 8) then UintOptRle lengths [3,2,3].
		{"multiple", []string{"abc", "de", "fgh"}, []uint8{8, 97, 98, 99, 100, 101, 102, 103, 104, 3, 2, 3}},
		// empty concat ("") -> varString [0]; lengths UintOptRle([0,0]) -> [64,0]
		// (a run of two zeros flushes negative-zero marker 64, then VarUint(count-2)
		// = VarUint(0) = 0).
		{"empty_strings", []string{"", ""}, []uint8{0, 64, 0}},
		// unicode: "世界" is 6 UTF-8 bytes, 2 UTF-16 code units.
		{"unicode", []string{"世界"}, []uint8{6, 228, 184, 150, 231, 149, 140, 2}},
		// no writes: sarr=[""], varString("")=[0], empty lengths -> [0].
		{"none", nil, []uint8{0}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newDefaultStringEncoder()
			for _, s := range c.in {
				e.writeValue(s)
			}
			got := e.bytes()
			if !bytes.Equal(got, c.want) {
				t.Fatalf("encode %v: want %v got %v", c.in, c.want, got)
			}
		})
	}
}

func TestStringRoundTrip(t *testing.T) {
	cases := [][]string{
		{"hello"},
		{"abc", "de", "fgh"},
		{"", ""},
		{"世界", "🌍", "αβγ"},
		{"a", "a", "a", "a"},
		// long strings to cross the internal 19-char buffering boundary
		{"this is a fairly long string fragment", "and another long one here too"},
		nil,
	}

	for _, in := range cases {
		e := newDefaultStringEncoder()
		for _, s := range in {
			e.writeValue(s)
		}
		data := e.bytes()

		d := newStringDecoder(data)
		for i, want := range in {
			got, err := d.readValue()
			if err != nil {
				t.Fatalf("in=%v idx=%d read err: %v", in, i, err)
			}
			if got != want {
				t.Fatalf("in=%v idx=%d: want %q got %q", in, i, want, got)
			}
		}
	}
}
