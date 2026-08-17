package crdt

import (
	"fmt"
	"testing"
)

// freshApplyDeltaCounts predicts how many structs and ContentFormat items a fresh all-insert
// ApplyDelta will create, so the arena can hand out one exactly-sized block. The prediction is a
// second implementation of the insertion path's shape, written separately from the path itself —
// which is the same count-versus-emit hazard as the delete-set writer and the JSON length prefix.
//
// Being wrong here is not a correctness bug: an undersized reservation falls back to ordinary
// growth and an oversized one wastes memory. But it silently defeats the optimisation, and a
// prediction that drifts from the path it models is exactly what nobody notices — the tests that
// pin "40/40" agree with whatever the predictor currently says.
//
// So this compares the prediction against what the document ACTUALLY contains afterwards, over
// randomized deltas rather than chosen ones.

func countFormatItems(t *testing.T, txt *YText) (structs int, formats int) {
	t.Helper()
	for item := txt.startItem(); item != nil; item = item.right {
		structs++
		if _, ok := item.content.(*contentFormat); ok {
			formats++
		}
	}
	return structs, formats
}

func TestFreshApplyDeltaCountsMatchWhatIsCreated(t *testing.T) {
	attrShapes := func(rng func(int) int) Object {
		switch rng(5) {
		case 0:
			return Object{} // no attributes
		case 1:
			a := newObject()
			a.Set("bold", true)
			return a
		case 2:
			a := newObject()
			a.Set("bold", true)
			a.Set("italic", false)
			return a
		case 3:
			a := newObject()
			a.Set("bold", Null) // null attributes must NOT be counted
			a.Set("italic", true)
			return a
		default:
			a := newObject()
			a.Set("bold", Null)
			return a // every attribute null
		}
	}

	for seed := 0; seed < 600; seed++ {
		rng := markerLCG(uint32(seed*2654435761 + 7))
		for _, sanitize := range []bool{true, false} {
			var delta []EventOperator
			n := 1 + rng(6)
			for i := 0; i < n; i++ {
				text := ""
				switch rng(6) {
				case 0:
					text = "" // empty insert: sanitized away, must not be counted
				case 1:
					text = "line\n" // trailing newline interacts with the sanitize rule
				default:
					text = fmt.Sprintf("s%d", rng(100))
				}
				delta = append(delta, NewTextDeltaOp(text, attrShapes(rng)))
			}

			wantStructs, wantFormats := freshApplyDeltaCounts(delta, sanitize)

			doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
			txt := doc.GetText("t")
			txt.ApplyDelta(delta, sanitize)
			gotStructs, gotFormats := countFormatItems(t, txt)

			// A zero prediction means "not a fresh all-insert delta, do not reserve" and is always
			// safe; only a non-zero prediction claims exactness.
			if wantStructs == 0 && wantFormats == 0 {
				continue
			}
			// Only the FORMAT count is compared. The struct count cannot be checked this way:
			// adjacent same-client Items merge at transaction cleanup, so the document afterwards
			// holds fewer structs than were created, and the predictor is counting creations.
			// ContentFormat Items never merge, so their count survives cleanup intact — which is
			// also why the format count is the one the arena reservation depends on being exact.
			_ = gotStructs
			_ = wantStructs
			if gotFormats != wantFormats {
				t.Fatalf("seed %d sanitize=%v: predicted %d ContentFormat items, document has %d\n"+
					" delta %v", seed, sanitize, wantFormats, gotFormats, delta)
			}
		}
	}
}

// Arena replacement lifetime: reserving a new exactly-sized block replaces the Doc's active block
// while Items created from the previous one are still live. Those Items hold pointers INTO the old
// array, so it must stay reachable and unmodified — the same property the position-index node
// blocks rely on, and the same one a "reuse the block" optimisation would quietly destroy.
func TestFormatArenaReplacementKeepsEarlierItemsValid(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))

	type snapshot struct {
		name  string
		delta string
		text  string
	}
	var taken []snapshot

	for round := 0; round < 30; round++ {
		name := fmt.Sprintf("t%d", round)
		txt := doc.GetText(name)
		var delta []EventOperator
		// Vary the attribute count so each round reserves a differently sized block, forcing
		// replacement rather than reuse of the remaining capacity.
		for i := 0; i < 1+round%7; i++ {
			a := newObject()
			a.Set("bold", i%2 == 0)
			if i%3 == 0 {
				a.Set("italic", true)
			}
			delta = append(delta, NewTextDeltaOp(fmt.Sprintf("r%di%d", round, i), a))
		}
		txt.ApplyDelta(delta, true)
		taken = append(taken, snapshot{
			name:  name,
			delta: deltaSemantic(txt.ToDelta(nil, nil, nil)),
			text:  txt.ToString(),
		})
	}

	// Every earlier text must still read exactly as it did when its block was the active one.
	for _, s := range taken {
		txt := doc.GetText(s.name)
		if got := txt.ToString(); got != s.text {
			t.Fatalf("%s: text changed after later reservations replaced the arena block\n"+
				" want %q\n got  %q", s.name, s.text, got)
		}
		if got := deltaSemantic(txt.ToDelta(nil, nil, nil)); got != s.delta {
			t.Fatalf("%s: delta changed after later reservations\n want %s\n got  %s",
				s.name, s.delta, got)
		}
	}

	// And the whole document must survive its own encoding.
	enc, err := EncodeStateAsUpdateV2(doc, nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	fresh := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(2))
	_ = ApplyUpdateV2(fresh, enc, nil)
	for _, s := range taken {
		if got := deltaSemantic(fresh.GetText(s.name).ToDelta(nil, nil, nil)); got != s.delta {
			t.Fatalf("%s: round-trip differs\n want %s\n got  %s", s.name, s.delta, got)
		}
	}
}
