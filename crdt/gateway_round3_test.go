package crdt

import "testing"

// MAX-gateway finding: Doc.GetSubdocGuids cast each SubDocs entry to string, but
// SubDocs holds *Doc (added in cleanupTransactions, read as *Doc in Destroy/GetSubdocs).
// yjs getSubdocGuids maps each subdoc to its .guid. Teeth: pre-fix this panics
// ("interface conversion: ... is *Doc, not string") on any doc that has a subdoc.
func TestGetSubdocGuidsReturnsGuids(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	m := doc.GetMap("m")
	m.Set("a", newDoc("guid-a", false, defaultGCFilter, nil, false))
	m.Set("b", newDoc("guid-b", false, defaultGCFilter, nil, false))

	guids := doc.GetSubdocGuids() // pre-fix: panics here
	if len(guids) != 2 {
		t.Fatalf("GetSubdocGuids returned %d guids, want 2: %v", len(guids), guids)
	}
	if !guids.Has("guid-a") || !guids.Has("guid-b") {
		t.Errorf("GetSubdocGuids = %v, want {guid-a, guid-b}", guids)
	}
}

// MAX-gateway finding: the 'subdocs' event must carry (changes, doc, transaction)
// like yjs (doc.emit('subdocs', [{...}, doc, transaction])) and every sibling event.
// Teeth: pre-fix only the changes object was emitted, so v[1]/v[2] were absent.
func TestSubdocsEventCarriesDocAndTransaction(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	var gotDoc bool
	var gotTrans bool
	var argc int
	doc.On("subdocs", NewObserverHandler(func(v ...interface{}) {
		argc = len(v)
		if len(v) >= 2 {
			d, ok := v[1].(*Doc)
			gotDoc = ok && d == doc
		}
		if len(v) >= 3 {
			_, gotTrans = v[2].(*Transaction)
		}
	}))

	doc.GetMap("m").Set("s", newDoc("sub", false, defaultGCFilter, nil, false))

	if argc < 3 {
		t.Fatalf("subdocs event emitted %d args, want 3 (changes, doc, transaction)", argc)
	}
	if !gotDoc {
		t.Error("subdocs event arg[1] is not the emitting *Doc")
	}
	if !gotTrans {
		t.Error("subdocs event arg[2] is not the *Transaction")
	}
}
