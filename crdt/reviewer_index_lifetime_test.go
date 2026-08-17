package crdt

import "testing"

// Side-table lifetime: repeated create/destroy must not accumulate entries.
//
// doc.positionIndexes is keyed by *AbstractType, so every entry is a strong reference. An entry
// that survives its type pins that type and its whole Item list for the lifetime of the Doc, and
// nothing goes wrong when it happens — no wrong answer, no race, no failing gate, just memory that
// never comes back.
//
// Two of the three checks originally written here were folded into list_position_index_test.go and
// have been removed rather than left as duplicates: entry-dies-with-its-type is covered there
// parameterised over both gc settings, and unreachable-key detection is covered by
// validateDocPositionIndexEntries, which walks live ContentType edges only.
//
// This one is kept because it is not subsumed. A single create/destroy cycle passes on an
// implementation that leaks exactly one entry per type — which is the shape a long-lived process
// actually suffers, and the shape a test written once per bug does not reach for.

func indexTableSize(doc *Doc) int { return len(doc.positionIndexes) }

// growPastActivation builds a nested array large enough to activate the index, then forces the
// activation by performing a positioned mutation.
func growPastActivation(t *testing.T, arr *YArray) {
	t.Helper()
	rng := markerLCG(3)
	for i := 0; i < buildListPositionIndexItems+2_000; i++ {
		arr.Insert(Number(rng(arr.GetLength()+1)), ArrayAny{i})
	}
	if listItemCount(arr) < buildListPositionIndexItems {
		t.Fatalf("fixture has %d items, need >= %d", listItemCount(arr), buildListPositionIndexItems)
	}
}

// Repeated create/destroy must not accumulate. A single-cycle test passes on an implementation that
// leaks one entry per type, which is exactly the shape a long-lived process suffers from.
func TestPositionIndexTableDoesNotAccumulate(t *testing.T) {
	doc := newDoc("g", true, defaultGCFilter, nil, false, WithClientID(1))
	outer := doc.GetArray("a")
	peak := 0
	for cycle := 0; cycle < 4; cycle++ {
		inner := NewYArray()
		outer.Insert(0, ArrayAny{inner})
		growPastActivation(t, inner)
		if s := indexTableSize(doc); s > peak {
			peak = s
		}
		outer.Delete(0, 1)
		if s := indexTableSize(doc); s > 1 {
			t.Fatalf("cycle %d: side table holds %d entries after the type was deleted; "+
				"entries are accumulating across create/destroy cycles", cycle, s)
		}
	}
	t.Logf("POSITION_INDEX_LIFETIME peak=%d final=%d after 4 create/destroy cycles",
		peak, indexTableSize(doc))
}

// The formatted activation path is a second route into the same side table, and it matters more
// than the plain one for lifetime: it activates at 512 physical Items rather than 16,000, so almost
// every non-trivial rich-text node in a document holds an entry. The population being large is what
// turns a one-entry-per-type leak from a curiosity into a bounded-memory problem.
//
// Cheaper to exercise than the plain path for exactly the same reason.
func TestPositionIndexTableDoesNotAccumulateForFormattedText(t *testing.T) {
	doc := newDoc("g", true, defaultGCFilter, nil, false, WithClientID(1))
	outer := doc.GetArray("a")
	rng := markerLCG(0xF0F0)
	peak := 0
	for cycle := 0; cycle < 6; cycle++ {
		inner := NewYText("")
		outer.Insert(0, ArrayAny{inner})
		bold := newObject()
		bold.Set("bold", true)
		inner.Insert(0, "x", bold)
		for i := 1; i < buildFormattedListPositionIndexItems+200; i++ {
			inner.Insert(Number(1+rng(int(inner.Length()))), "x", Object{})
		}
		// Force activation: a positioned edit inside the inherited run.
		inner.Insert(Number(1+rng(int(inner.Length()))), "y", Object{})
		if s := indexTableSize(doc); s > peak {
			peak = s
		}
		outer.Delete(0, 1)
		if s := indexTableSize(doc); s > 1 {
			t.Fatalf("cycle %d: side table holds %d entries after the formatted type was deleted",
				cycle, s)
		}
	}
	if peak == 0 {
		t.Fatal("no formatted index ever activated; this test observes nothing")
	}
	t.Logf("POSITION_INDEX_LIFETIME_FORMATTED peak=%d final=%d after 6 cycles",
		peak, indexTableSize(doc))
}
