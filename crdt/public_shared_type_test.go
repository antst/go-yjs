package crdt_test

import (
	"testing"

	"github.com/antst/go-yjs/crdt"
)

// This test deliberately lives outside package y_crdt. It pins that the public
// shared-type boundary is usable without importing any of the private object
// graph types that back it.
func TestPublicSharedTypeBoundaryIsUsableExternally(t *testing.T) {
	doc := crdt.NewDoc("public-boundary", crdt.WithGC(false), crdt.WithClientID(1))
	text := doc.GetText("text")
	var shared crdt.SharedType = text

	pos := crdt.NewRelativePositionFromTypeIndex(shared, 0, 0)
	if pos == nil {
		t.Fatal("relative-position constructor rejected a public shared type")
	}

	constructed, err := doc.Get("generic-map", func() crdt.SharedType {
		return crdt.NewYMap(nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := constructed.(*crdt.YMap); !ok {
		t.Fatalf("Doc.Get returned %T, want *YMap", constructed)
	}

	fragment := doc.GetXmlFragment("xml")
	first := crdt.NewYXmlElement("first")
	second := crdt.NewYXmlText()
	fragment.Insert(0, crdt.ArrayAny{first})
	fragment.InsertAfter(first, crdt.ArrayAny{second})
	if got := fragment.GetFirstChild(); got != first {
		t.Fatalf("GetFirstChild returned %T, want the inserted *YXmlElement", got)
	}

	acceptText := func(*crdt.YText) {}
	acceptFragment := func(*crdt.YXmlFragment) {}
	acceptText(text.Clone())
	acceptFragment(fragment.Clone())

	array := doc.GetArray("array")
	array.Push(crdt.ArrayAny{"before"})
	snapshot := crdt.NewSnapshotByDoc(doc)
	// Use a distinct content kind so cleanup cannot merge the post-snapshot
	// value into the pre-snapshot ContentAny item.
	array.Push(crdt.ArrayAny{[]byte("after")})
	if got := crdt.TypeListToArraySnapshot(array, snapshot); len(got) != 1 || got[0] != "before" {
		t.Fatalf("TypeListToArraySnapshot = %#v, want [before]", got)
	}

	sharedMap := doc.GetMap("snapshot-map")
	sharedMap.Set("key", "before")
	mapSnapshot := crdt.NewSnapshotByDoc(doc)
	sharedMap.Set("key", "after")
	if got := crdt.TypeMapGetSnapshot(sharedMap, "key", mapSnapshot); got != "before" {
		t.Fatalf("TypeMapGetSnapshot = %#v, want before", got)
	}
	if got, _ := crdt.TypeMapGetAllSnapshot(sharedMap, mapSnapshot).Get("key"); got != "before" {
		t.Fatalf("TypeMapGetAllSnapshot[key] = %#v, want before", got)
	}

	text.Insert(0, "public codec surface", crdt.Object{})
	if cleaned := crdt.CleanupYTextFormatting(text); cleaned != 0 {
		t.Fatalf("CleanupYTextFormatting(clean text) = %d, want 0", cleaned)
	}
	updateV1, err := crdt.EncodeStateAsUpdate(doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	from, to, err := crdt.ParseUpdateMeta(updateV1)
	if err != nil || len(from) == 0 || len(to) == 0 {
		t.Fatalf("ParseUpdateMeta = (%v, %v, %v), want non-empty bounds", from, to, err)
	}
	stateVector, err := crdt.EncodeStateVectorFromUpdate(updateV1)
	if err != nil {
		t.Fatal(err)
	}
	if decoded, err := crdt.DecodeStateVector(stateVector); err != nil || len(decoded) == 0 {
		t.Fatalf("DecodeStateVector = (%v, %v), want a non-empty state vector", decoded, err)
	}
	updateV2, err := crdt.EncodeStateAsUpdateV2(doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := crdt.EncodeStateVectorFromUpdateV2(updateV2); err != nil {
		t.Fatal(err)
	}
	if obfuscated, err := crdt.ObfuscateUpdate(updateV1); err != nil || len(obfuscated) == 0 {
		t.Fatalf("ObfuscateUpdate = (%x, %v), want a non-empty update", obfuscated, err)
	}

	undo := crdt.NewUndoManager([]crdt.SharedType{text}, 0, nil)
	undo.Destroy()
}
