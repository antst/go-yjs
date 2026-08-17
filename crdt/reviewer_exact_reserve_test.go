package crdt

import (
	"fmt"
	"testing"
	"unicode/utf16"
)

// The exact string/origin reservation rests on a proof: when EVERY op in a fresh delta is a
// non-empty formatted insertText, the resulting ContentStrings are physically separated by format
// markers and therefore cannot merge. If they could merge, the surviving Item would retain the
// whole exact block — a leak of the entire reservation rather than of one Item.
//
// The obvious way for the proof to fail is two consecutive ops carrying the SAME attributes.
// minimizeAttributeChanges exists to drop redundant format markers, so if it drops them there, the
// two strings become adjacent and mergeable while still satisfying the "every op is formatted"
// precondition the reservation checks.

func countContentStrings(txt *YText) (strings int, formats int, total int) {
	for item := txt.startItem(); item != nil; item = item.right {
		total++
		switch item.content.(type) {
		case *contentString:
			strings++
		case *contentFormat:
			formats++
		}
	}
	return
}

// The attack: consecutive ops with IDENTICAL attributes.
func TestExactReserveHoldsWhenAdjacentOpsShareAttributes(t *testing.T) {
	shapes := []struct {
		name  string
		attrs func(i int) Object
	}{
		{"identical every op", func(int) Object { a := newObject(); a.Set("bold", true); return a }},
		{"identical pairs", func(i int) Object {
			a := newObject()
			a.Set("bold", i/2%2 == 0)
			return a
		}},
		{"same key alternating value", func(i int) Object {
			a := newObject()
			a.Set("bold", i%2 == 0)
			return a
		}},
		{"multi-key identical", func(int) Object {
			a := newObject()
			a.Set("bold", true)
			a.Set("italic", true)
			a.Set("color", "#123")
			return a
		}},
	}

	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			for ops := 2; ops <= 8; ops++ {
				doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
				txt := doc.GetText("t")
				var delta []EventOperator
				want := ""
				for i := 0; i < ops; i++ {
					s := fmt.Sprintf("p%d", i)
					delta = append(delta, NewTextDeltaOp(s, shape.attrs(i)))
					want += s
				}
				txt.ApplyDelta(delta, true)

				if got := txt.ToString(); got != want {
					t.Fatalf("ops=%d: content %q, want %q", ops, got, want)
				}
				strs, _, _ := countContentStrings(txt)
				if strs != ops {
					t.Fatalf("ops=%d: %d ContentString Items survive for %d formatted ops — the "+
						"strings MERGED, so the reservation's exact block is retained by the "+
						"survivor instead of being freed with its Items", ops, strs, ops)
				}
			}
		})
	}
}

// One origin per string that spans more than one UTF-16 clock unit. Wide runes and surrogate pairs
// are where a byte-length assumption diverges from the clock the CRDT actually uses.
func TestExactReserveOriginCountForWideStrings(t *testing.T) {
	// Valid UTF-8 only. Invalid input does not survive the wire format — a lone 0xff decodes as
	// U+FFFD — but that is pre-existing, unrelated to this reservation, and reproduces on a plain
	// Insert at mainline, so it is reported separately rather than folded into a test about
	// origin counting.
	cases := []string{
		"a", "ab", "café", "日本語", "\U0001F600", "a\U0001F600b",
		"\U0001F600\U0001F600", "é",
	}
	for _, s := range cases {
		doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
		txt := doc.GetText("t")
		a := newObject()
		a.Set("bold", true)
		txt.ApplyDelta([]EventOperator{NewTextDeltaOp(s, a)}, true)

		if got := txt.ToString(); got != s {
			t.Fatalf("%q round-tripped as %q", s, got)
		}
		// The CRDT length is in UTF-16 code units, which is what "spans more than one clock" means.
		want := len(utf16.Encode([]rune(s)))
		if got := int(txt.Length()); got != want {
			t.Fatalf("%q: Length()=%d, want %d UTF-16 units", s, got, want)
		}
		// Must survive its own encoding: a wrong origin count shows up as a decode divergence.
		enc, err := EncodeStateAsUpdateV2(doc, nil)
		if err != nil {
			t.Fatalf("%q encode: %v", s, err)
		}
		fresh := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(2))
		_ = ApplyUpdateV2(fresh, enc, nil)
		if got := fresh.GetText("t").ToString(); got != s {
			t.Fatalf("%q decoded as %q", s, got)
		}
	}
}

// The sanitize boundary decides which deltas qualify, and an op that is empty AFTER sanitizing must
// not be counted — otherwise the reservation is oversized and, worse, the "every op is formatted"
// precondition is evaluated against ops that never materialise.
func TestExactReserveSanitizeBoundary(t *testing.T) {
	for _, sanitize := range []bool{true, false} {
		for _, trailing := range []string{"x", "x\n", "\n"} {
			doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
			txt := doc.GetText("t")
			a := newObject()
			a.Set("bold", true)
			delta := []EventOperator{
				NewTextDeltaOp("head", a),
				NewTextDeltaOp(trailing, a),
			}
			txt.ApplyDelta(delta, sanitize)

			enc, err := EncodeStateAsUpdateV2(doc, nil)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			fresh := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(2))
			_ = ApplyUpdateV2(fresh, enc, nil)
			if a, b := txt.ToString(), fresh.GetText("t").ToString(); a != b {
				t.Fatalf("sanitize=%v trailing=%q: round-trip differs %q vs %q", sanitize, trailing, a, b)
			}
			if a, b := deltaSemantic(txt.ToDelta(nil, nil, nil)),
				deltaSemantic(fresh.GetText("t").ToDelta(nil, nil, nil)); a != b {
				t.Fatalf("sanitize=%v trailing=%q: delta differs\n %s\n %s", sanitize, trailing, a, b)
			}
		}
	}
}
