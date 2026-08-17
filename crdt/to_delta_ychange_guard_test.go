package crdt

import "testing"

// The cold ToDelta scan caches "is ychange present in currentAttributes" in a boolean. That flag is
// only safe while it stays in sync with the map, and there is exactly one path where the map can
// change out from under it: a ContentFormat whose key IS "ychange", which a user can create with a
// plain Format call. The scan re-syncs the flag there; this exercises that branch, which the
// snapshot-driven tests do not reach because they never format that key.
func TestToDeltaYChangeFlagResyncGuard(t *testing.T) {
	mk := func() (*Doc, *YText) {
		doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
		txt := doc.GetText("t")
		txt.Insert(0, "abcdefghij", Object{})
		return doc, txt
	}

	// A user-authored "ychange" attribute, set and then cleared mid-run.
	_, txt := mk()
	on := newObject()
	on.Set("ychange", MakeObject("user", "set"))
	txt.Format(2, 3, on)
	off := newObject()
	off.Set("ychange", Null)
	txt.Format(6, 2, off)

	plain := txt.ToDelta(nil, nil, nil)
	if len(plain) == 0 {
		t.Fatal("empty delta")
	}

	// The same document read through a snapshot, which is the path that drives the ychange logic
	// proper. Both must render the text identically; only attribution may differ.
	snapDoc := newDoc("g", true, defaultGCFilter, nil, false, WithClientID(1))
	st := snapDoc.GetText("t")
	st.Insert(0, "abcdefghij", Object{})
	snap := NewSnapshotByDoc(snapDoc)
	st.Insert(5, "XYZ", Object{})
	st.Delete(0, 2)

	withSnap := st.ToDelta(snap, nil, nil)
	withCompute := st.ToDelta(snap, nil, func(kind string, id *ID) Object {
		o := newObject()
		o.Set("type", kind)
		return o
	})
	if len(withSnap) == 0 || len(withCompute) == 0 {
		t.Fatal("snapshot delta empty")
	}

	// Text content must match between the two snapshot renderings: computeYChange changes only the
	// attribute attached to a run, never which characters appear.
	textOf := func(ops []EventOperator) string {
		out := ""
		for _, op := range ops {
			if s, ok := op.InsertValue().(string); ok {
				out += s
			}
		}
		return out
	}
	if a, b := textOf(withSnap), textOf(withCompute); a != b {
		t.Fatalf("computeYChange altered rendered text\n want %q\n got  %q", a, b)
	}

	// Repeated reads must be stable: the flag is per-scan state and must not leak between calls.
	for i := 0; i < 5; i++ {
		if got := textOf(st.ToDelta(snap, nil, nil)); got != textOf(withSnap) {
			t.Fatalf("repeat %d: snapshot delta text changed across reads", i)
		}
		if got := textOf(txt.ToDelta(nil, nil, nil)); got != textOf(plain) {
			t.Fatalf("repeat %d: plain delta text changed across reads", i)
		}
	}
}
