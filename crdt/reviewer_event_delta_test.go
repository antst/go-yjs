package crdt

import (
	"fmt"
	"strings"
	"testing"
)

// Event deltas now come from the shared append-only accumulator, whose Take returns an
// unsafe.String view over a buffer that later segments keep appending to. That is the same
// construction as ContentString's tail-append buffer, and it is correct for the same reason:
// appends only ever write past the length of every string already handed out, and a reallocation
// leaves the old array alive through the returned string.
//
// "Correct for the same reason" is not the same as "checked", and the failure mode is silent: an
// observer that keeps a delta would see its text change underneath it later, with nothing failing
// at the time. Observers are exactly the callers most likely to retain — that is what an observer
// is for.

// TestEventDeltaArenaSegmentsAreCorrect checks the delta strings the arena produces against an
// INDEPENDENTLY KNOWN expectation, not against a copy of themselves.
//
// That distinction is the whole test. The arena's Take returns an unsafe.String view over a shared
// append buffer, and if a later segment ever wrote below a previous segment's end, the earlier
// string would change — but the corruption happens inside GetDelta, before an observer sees
// anything. A test that snapshots each string as it arrives and re-compares later is therefore
// comparing a corrupted value to a copy of the same corrupted value, and passes. The first version
// of this test did exactly that and passed against an injected buffer-rewind bug.
//
// The fixture is deterministic, so each coalesced op must be exactly thirteen copies of one letter.
// Anything else — a truncated segment, a doubled one, bytes from a neighbour — fails.
func TestEventDeltaArenaSegmentsAreCorrect(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	txt := doc.GetText("t")

	const segments, runLen = 4, 13
	var got []string
	txt.Observe(func(e interface{}, _ interface{}) {
		ev, ok := e.(*YTextEvent)
		if !ok {
			return
		}
		for _, op := range ev.GetDelta() {
			if op.IsInsert() {
				if s, ok := op.InsertValue().(string); ok {
					got = append(got, s)
				}
			}
		}
	})

	// Each segment: one attributed character opening a distinct run, then twelve inheriting
	// characters. Multiple ContentStrings per op is what forces Take onto the shared buffer rather
	// than its single-value fast field — verified by instrumenting the two paths, which reports
	// four buffered takes and zero scalar ones for this shape.
	Transact(doc, func(*Transaction) {
		for seg := 0; seg < segments; seg++ {
			attr := newObject()
			attr.Set("tag", fmt.Sprintf("s%d", seg))
			txt.Insert(txt.Length(), string(rune('a'+seg)), attr)
			for k := 1; k < runLen; k++ {
				txt.Insert(txt.Length(), string(rune('a'+seg)), Object{})
			}
		}
	}, nil, true)

	if len(got) != segments {
		t.Fatalf("delta has %d insert ops %q, want %d coalesced segments", len(got), got, segments)
	}
	for seg, s := range got {
		want := strings.Repeat(string(rune('a'+seg)), runLen)
		if s != want {
			t.Fatalf("segment %d = %q, want %q — the shared arena produced the wrong bytes for a "+
				"previously taken segment", seg, s, want)
		}
	}
	t.Logf("EVENT_DELTA_ARENA %d coalesced segments of %d, all exact", len(got), runLen)
}

// The transitions they asked about. Coalescing must stop at anything that is not an adjacent
// string insert: a format between two runs, and an embed between two runs. Merging across either
// would produce a delta that renders the same text with the wrong attribution or the wrong op
// count, which no text comparison would notice.
func TestEventDeltaCoalescingStopsAtNonStringBoundaries(t *testing.T) {
	run := func(name string, build func(txt *YText), wantInserts []string) {
		t.Run(name, func(t *testing.T) {
			doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
			txt := doc.GetText("t")
			var got []string
			txt.Observe(func(e interface{}, _ interface{}) {
				ev, ok := e.(*YTextEvent)
				if !ok {
					return
				}
				for _, op := range ev.GetDelta() {
					if op.IsInsert() {
						if s, ok := op.InsertValue().(string); ok {
							got = append(got, s)
						} else {
							got = append(got, "<non-string>")
						}
					}
				}
			})
			Transact(doc, func(*Transaction) { build(txt) }, nil, true)
			if len(got) != len(wantInserts) {
				t.Fatalf("delta has %d insert ops %q, want %d %q", len(got), got, len(wantInserts), wantInserts)
			}
			for i := range got {
				if got[i] != wantInserts[i] {
					t.Fatalf("insert op %d = %q, want %q (full: %q)", i, got[i], wantInserts[i], got)
				}
			}
		})
	}

	// Plain adjacency SHOULD coalesce — the whole point of the change.
	run("adjacent strings coalesce", func(txt *YText) {
		for i := 0; i < 512; i++ {
			txt.Insert(txt.Length(), "x", Object{})
		}
	}, []string{strings.Repeat("x", 512)})

	// string -> format -> string must not merge across a genuine attribute boundary.
	//
	// The trailing insert must carry EXPLICIT attributes. Passing Object{} means "inherit": the
	// zero Object is nil, and Insert substitutes the attributes active at the cursor, so a plain
	// third insert after a bold run really is bold and really should coalesce with it. The first
	// version of this case did that and read the correct answer as a coalescing bug — checking the
	// parent commit, which behaved identically, is what caught it.
	run("attribute change breaks the run", func(txt *YText) {
		bold := newObject()
		bold.Set("bold", true)
		plain := newObject()
		plain.Set("bold", Null)
		txt.Insert(0, "aaa", Object{})
		txt.Insert(3, "bbb", bold)
		txt.Insert(6, "ccc", plain)
	}, []string{"aaa", "bbb", "ccc"})

	// string <-> embed must not merge either, and the embed must stay its own op.
	run("embed breaks the run", func(txt *YText) {
		txt.Insert(0, "aaa", Object{})
		em := newObject()
		em.Set("img", "x")
		txt.InsertEmbed(3, em, Object{})
		txt.Insert(4, "bbb", Object{})
	}, []string{"aaa", "<non-string>", "bbb"})
}
