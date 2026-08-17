package crdt

import (
	"fmt"
	"strings"
	"testing"
)

// String immutability across the amortized tail-append buffer.
//
// WHY THIS NEEDS ITS OWN TEST. The sole-run append fast path builds ContentString.Str with
// unsafe.String over a []byte it keeps and appends to. Go strings are immutable BY CONTRACT --
// every consumer, the standard library, and map keying all assume it -- and this construction makes
// that contract conditional on an invariant maintained by hand: appends must only ever write PAST
// the length of every string previously exposed.
//
// The reasoning is sound. append writes at [len, newlen) so bytes [0, len) are untouched; when
// capacity is exceeded append allocates a fresh array and any retained string keeps the old one
// alive; and the buffer is never rewound to [:0], only extended or replaced by a new make(). The
// three-part validity check even rejects a caller reassigning the exported Str field directly.
//
// But "sound by reasoning" is exactly what this file exists to convert into "checked". A violation
// would be catastrophic and silent: a string handed to a consumer, or used as a map key, or written
// to the wire, would change value underneath them. The differential oracle cannot see it, because
// it reads each document once and never retains a string across subsequent appends.

// TestAppendBufferRetainedStringsNeverMutate retains the value after EVERY append and re-checks all
// of them at the end, including across the reallocations that buffer growth forces.
func TestAppendBufferRetainedStringsNeverMutate(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	txt := doc.GetText("t")

	const appends = 4000
	retained := make([]string, 0, appends)
	expected := make([]string, 0, appends)

	var sb strings.Builder
	for i := 0; i < appends; i++ {
		piece := string(rune('a' + i%26))
		txt.Insert(txt.Length(), piece, Object{})
		sb.WriteString(piece)

		// Retain the live value. On the fast path this string is a view over the shared buffer, so
		// if a later append ever wrote below its length this copy would change.
		retained = append(retained, txt.ToString())
		expected = append(expected, sb.String())
	}

	for i := range retained {
		if retained[i] != expected[i] {
			t.Fatalf("string retained after append %d MUTATED\n want %q\n got  %q",
				i, trunc(expected[i]), trunc(retained[i]))
		}
	}
	if got := txt.ToString(); got != expected[len(expected)-1] {
		t.Fatalf("final text differs\n want %q\n got %q", trunc(expected[len(expected)-1]), trunc(got))
	}
	t.Logf("APPEND_IMMUTABILITY appends=%d retained=%d all stable", appends, len(retained))
}

// TestAppendBufferSurvivesReassignmentPaths drives the operations that replace ContentString.Str
// out from under the buffer -- merge, split by mid-insert, and delete. Each must invalidate the
// buffer rather than append into a stale one; the validity check is what makes that safe, and it
// has three conditions, so it deserves to be exercised rather than read.
func TestAppendBufferSurvivesReassignmentPaths(t *testing.T) {
	for round := 0; round < 200; round++ {
		doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
		txt := doc.GetText("t")
		var model strings.Builder
		var retained []string
		var want []string

		for step := 0; step < 40; step++ {
			switch (round + step) % 4 {
			case 0, 1: // tail append: the fast path
				p := fmt.Sprintf("%d", (round+step)%10)
				txt.Insert(txt.Length(), p, Object{})
				model.WriteString(p)
			case 2: // MID insert: splits the run, reassigning Str
				cur := model.String()
				if len(cur) > 2 {
					at := len(cur) / 2
					txt.Insert(at, "M", Object{})
					model.Reset()
					model.WriteString(cur[:at] + "M" + cur[at:])
				}
			default: // delete: reassigns Str and drops content
				cur := model.String()
				if len(cur) > 3 {
					txt.Delete(1, 1)
					model.Reset()
					model.WriteString(cur[:1] + cur[2:])
				}
			}
			retained = append(retained, txt.ToString())
			want = append(want, model.String())
		}

		for i := range retained {
			if retained[i] != want[i] {
				t.Fatalf("round %d step %d: retained string mutated or diverged\n want %q\n got  %q",
					round, i, trunc(want[i]), trunc(retained[i]))
			}
		}
	}
	t.Logf("APPEND_REASSIGN rounds=200 all stable")
}

// TestAppendBufferEncodedBytesStable checks the consequence that would actually reach a peer: a
// retained ENCODING must not change when the document is appended to afterwards. If a wire buffer
// aliased the append buffer, a message already handed to a transport would rewrite itself.
func TestAppendBufferEncodedBytesStable(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	txt := doc.GetText("t")
	txt.Insert(0, "seed", Object{})

	snapshots := make([][]byte, 0, 64)
	shapes := make([]string, 0, 64)
	for i := 0; i < 64; i++ {
		txt.Insert(txt.Length(), string(rune('a'+i%26)), Object{})
		enc, err := EncodeStateAsUpdateV2(doc, nil)
		if err != nil {
			t.Fatalf("encode %d: %v", i, err)
		}
		snapshots = append(snapshots, enc)
		shapes = append(shapes, txt.ToString())
	}

	// Keep appending well past every retained encoding, forcing buffer growth.
	for i := 0; i < 4000; i++ {
		txt.Insert(txt.Length(), "z", Object{})
	}

	for i, enc := range snapshots {
		fresh := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(2))
		_ = ApplyUpdateV2(fresh, enc, nil)
		if got := fresh.GetText("t").ToString(); got != shapes[i] {
			t.Fatalf("encoding retained at step %d decodes differently after later appends\n"+
				" want %q\n got  %q", i, trunc(shapes[i]), trunc(got))
		}
	}
	t.Logf("APPEND_WIRE_STABLE snapshots=%d", len(snapshots))
}

func trunc(s string) string {
	if len(s) <= 80 {
		return s
	}
	return s[:40] + "…" + s[len(s)-40:]
}
