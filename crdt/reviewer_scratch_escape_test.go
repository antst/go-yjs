package crdt

import (
	"fmt"
	"testing"
)

// The mutation scratch is one reused ordered-Object per Doc, reset on every acquire. That makes it
// the same class of construction as the append buffer behind ContentString: correct only while
// nothing retains a reference past the operation that produced it. A leak here does not crash and
// does not fail the differential — the document renders correctly right up until a LATER formatted
// mutation resets the storage underneath a value the document is still holding.
//
// So the checks are retention-shaped rather than value-shaped: produce a value, keep it, perform
// more work that reuses the scratch, and require the kept value to be unchanged.

func deltaAttrs(t *testing.T, txt *YText) string {
	t.Helper()
	return deltaSemantic(txt.ToDelta(nil, nil, nil))
}

// TestScratchReuseElsewhereDoesNotDisturbAnotherText is the escape check, shaped so it can
// actually fail.
//
// The obvious version — record renderings after each step, then compare them at the end — cannot
// detect anything: those renderings are Go strings, immutable snapshots that a later corruption
// leaves untouched. My first attempt did exactly that and would have passed against any leak.
//
// What bites instead: build one text, record it, then drive many formatted mutations on a DIFFERENT
// text in the SAME Doc, which is the only thing that reuses the shared scratch. The first text is
// never touched, so re-reading it must return exactly what was recorded. If scratch storage ever
// escaped into stored attribute state, mutating the second text would reset it underneath the
// first, and the re-read diverges.
func TestScratchReuseElsewhereDoesNotDisturbAnotherText(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	stable := doc.GetText("stable")
	churn := doc.GetText("churn")

	for i := 0; i < 40; i++ {
		a := newObject()
		a.Set("bold", i%2 == 0)
		a.Set("tag", fmt.Sprintf("s%d", i))
		if i%3 == 0 {
			a.Set("color", fmt.Sprintf("#%03d", i))
		}
		stable.Insert(stable.Length(), fmt.Sprintf("%d", i%10), a)
	}
	recorded := deltaAttrs(t, stable)
	recordedText := stable.ToString()

	// Every operation below reuses the per-Doc scratch, with deliberately different attribute
	// shapes and sizes so a reset would write recognisably different state.
	for i := 0; i < 200; i++ {
		a := newObject()
		a.Set("bold", true)
		a.Set("tag", "CHURN")
		a.Set("color", "#fff")
		a.Set("extra", i)
		churn.Insert(i%(churn.Length()+1), "z", a)
		if churn.Length() > 4 {
			cleared := newObject()
			cleared.Set("bold", Null)
			cleared.Set("color", Null)
			churn.Format(i%(churn.Length()-3), 3, cleared)
		}
		if got := deltaAttrs(t, stable); got != recorded {
			t.Fatalf("step %d: the untouched text changed while another text reused the scratch\n"+
				" recorded %s\n now      %s", i, recorded, got)
		}
	}
	if got := stable.ToString(); got != recordedText {
		t.Fatalf("untouched text content changed\n want %q\n got  %q", recordedText, got)
	}

	// And it must survive its own encoding, since a corrupted attribute can render correctly in
	// memory while encoding something else.
	enc, err := EncodeStateAsUpdateV2(doc, nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	fresh := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(2))
	_ = ApplyUpdateV2(fresh, enc, nil)
	if got := deltaAttrs(t, fresh.GetText("stable")); got != recorded {
		t.Fatalf("round-trip of the untouched text diverged\n want %s\n got  %s", recorded, got)
	}
}

// Reentrancy: the scratch is one per Doc, so a mutation triggered from inside another mutation's
// transaction would reset storage the outer one may still be using. Observers fire at cleanup
// rather than mid-call, which should make this safe — "should" being why it is tested.
func TestScratchSurvivesMutationFromObserver(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	txt := doc.GetText("t")
	other := doc.GetText("o")

	depth := 0
	txt.Observe(func(interface{}, interface{}) {
		if depth > 0 {
			return
		}
		depth++
		defer func() { depth-- }()
		a := newObject()
		a.Set("bold", true)
		a.Set("from", "observer")
		other.Insert(other.Length(), "o", a)
	})

	want := ""
	for i := 0; i < 30; i++ {
		a := newObject()
		a.Set("bold", i%2 == 0)
		a.Set("step", fmt.Sprintf("s%d", i))
		txt.Insert(txt.Length(), "x", a)
		want += "x"
	}
	if got := txt.ToString(); got != want {
		t.Fatalf("text diverged under observer-triggered mutation\n want %q\n got  %q", want, got)
	}
	if other.Length() == 0 {
		t.Fatal("the observer never mutated; this test exercises nothing")
	}
	// Round-trip: a corrupted attribute would survive in memory but re-decode differently.
	enc, err := EncodeStateAsUpdateV2(doc, nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	fresh := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(2))
	_ = ApplyUpdateV2(fresh, enc, nil)
	if a, b := deltaAttrs(t, txt), deltaAttrs(t, fresh.GetText("t")); a != b {
		t.Fatalf("round-trip differs\n live %s\n decoded %s", a, b)
	}
}
