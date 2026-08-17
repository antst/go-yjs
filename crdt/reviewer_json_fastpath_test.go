package crdt

import "testing"

// The fast JSON path writes wire bytes directly, so its two halves must agree EXACTLY: the
// var-length prefix comes from jsonStringJSEncodedLen and the payload from appendJSONStringJS. If
// they disagree by one byte for any input, the prefix lies about the payload and every subsequent
// field in the update decodes at the wrong offset. That is not a rendering bug, it is a corrupt
// stream, and the corruption is caused by whichever byte value the two functions classify
// differently.
//
// A hand-chosen list of escape classes cannot establish agreement — it establishes agreement on the
// bytes someone thought of. The relationship is cheap to check exhaustively, so it is checked
// exhaustively: every single byte, and every two-byte sequence.

func TestJSONFastPathLengthMatchesPayloadExhaustively(t *testing.T) {
	check := func(s string) {
		t.Helper()
		payload := appendJSONStringJS(nil, s)
		if got, want := jsonStringJSEncodedLen(s), uint64(len(payload)); got != want {
			t.Fatalf("length disagrees with payload for %q: encodedLen=%d actual=%d\n"+
				"the var-string prefix would misdescribe the payload and corrupt the stream",
				s, got, want)
		}
	}
	// Every byte value, alone.
	for b := 0; b < 256; b++ {
		check(string([]byte{byte(b)}))
	}
	// Every ordered pair. Catches any state carried between iterations of the escape loop —
	// notably the `start` cursor, which is the one piece of state appendJSONStringJS keeps.
	for a := 0; a < 256; a++ {
		for b := 0; b < 256; b++ {
			check(string([]byte{byte(a), byte(b)}))
		}
	}
	// Longer strings mixing passthrough, two-byte escapes and six-byte escapes, including a
	// payload past 127 bytes so the prefix itself needs multiple varint bytes.
	rng := markerLCG(0x5150)
	alphabet := []byte{'a', '"', '\\', '\b', '\t', '\n', '\f', '\r', 0x00, 0x0b, 0x1f, 0x20, 0x7f, 0xc3, 0xa9, 0xe2, 0x80, 0xa8}
	for i := 0; i < 4000; i++ {
		n := 1 + rng(200)
		buf := make([]byte, n)
		for j := range buf {
			buf[j] = alphabet[rng(len(alphabet))]
		}
		check(string(buf))
	}
}

// The fast path must also produce the SAME BYTES as the encoder it is bypassing. Agreeing with
// itself is necessary but not sufficient: both halves could be consistently wrong and would then
// encode a document the reference decodes differently.
func TestJSONFastPathBytesMatchOrderedMarshal(t *testing.T) {
	mismatch := 0
	check := func(s string) {
		t.Helper()
		fast := string(appendJSONStringJS(nil, s))
		want, err := marshalJSONOrdered(s)
		if err != nil {
			t.Fatalf("marshalJSONOrdered(%q): %v", s, err)
		}
		if fast != string(want) {
			mismatch++
			if mismatch <= 5 {
				t.Errorf("fast path differs from marshalJSONOrdered for %q (% x)\n fast %q\n want %q",
					s, []byte(s), fast, string(want))
			}
		}
	}
	for b := 0; b < 256; b++ {
		check(string([]byte{byte(b)}))
	}
	for _, s := range []string{
		"", "plain", `quote"inside`, `back\slash`, "tab\there", "nl\nhere",
		" line-sep", " para-sep", "café", "emoji \U0001F600",
		"\x00\x01\x02\x1f", "\x7f", "mixed \"\\\n  \U0001F600 end",
		string([]byte{0xff}), string([]byte{0xc3}), string([]byte{0xed, 0xa0, 0x80}), // invalid UTF-8
	} {
		check(s)
	}
	if mismatch > 0 {
		t.Fatalf("%d inputs encoded differently from the marshaller the fast path replaces", mismatch)
	}
}
