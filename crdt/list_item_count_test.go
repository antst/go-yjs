package crdt

import "testing"

func assertLinkedItemCount(t *testing.T, typ abstractType) Number {
	t.Helper()
	want := Number(0)
	for item := typ.startItem(); item != nil; item = item.right {
		want++
	}
	if got := listItemCount(typ); got != want {
		t.Fatalf("linked Item count = %d, walked %d", got, want)
	}
	return want
}

func TestLinkedItemCountTracksListSurgery(t *testing.T) {
	doc := newDoc("count", false, defaultGCFilter, nil, false, WithClientID(1))
	text := doc.GetText("t")
	text.Insert(0, "abcdefghijklmnopqrstuvwxyz", Object{})
	if got := assertLinkedItemCount(t, text); got != 1 {
		t.Fatalf("one ContentString produced %d Items, want 1", got)
	}

	// Random edits exercise integration, splitting, deletion, and cleanup merging. Check the
	// maintained counter after every public transaction rather than only at the final shape.
	rng := markerLCG(0x1CEB00DA)
	for i := 0; i < 2_000; i++ {
		if text.Length() > 0 && rng(4) == 0 {
			text.Delete(Number(rng(text.Length())), 1)
		} else {
			text.Insert(Number(rng(text.Length()+1)), "x", Object{})
		}
		assertLinkedItemCount(t, text)
	}

	update, err := EncodeStateAsUpdateV2(doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	replica := newDoc("count", false, defaultGCFilter, nil, false, WithClientID(2))
	replicaText := replica.GetText("t")
	_ = ApplyUpdateV2(replica, update, nil)
	if got, want := assertLinkedItemCount(t, replicaText), assertLinkedItemCount(t, text); got != want {
		t.Fatalf("decoded linked Item count = %d, source %d", got, want)
	}

	// A root decoded before its concrete accessor is requested is first held by AbstractType and
	// later adopted by YText in Doc.Get. The adoption must carry the physical-list count too.
	lazy := newDoc("count", false, defaultGCFilter, nil, false, WithClientID(3))
	_ = ApplyUpdateV2(lazy, update, nil)
	lazyText := lazy.GetText("t")
	assertLinkedItemCount(t, lazyText)

	// Inside a long transaction the list has not been cleaned up yet. Integration and splitting
	// must update the counter immediately rather than relying on a commit-time recount.
	array := doc.GetArray("a")
	array.Insert(0, ArrayAny{0, 1, 2, 3})
	Transact(doc, func(*Transaction) {
		for i := 0; i < 1_000; i++ {
			if array.GetLength() > 0 && rng(4) == 0 {
				array.Delete(Number(rng(array.GetLength())), 1)
			} else {
				array.Insert(Number(rng(array.GetLength()+1)), ArrayAny{i + 10})
			}
			assertLinkedItemCount(t, array)
		}
	}, nil, true)
	assertLinkedItemCount(t, array)

	// Map history is stored in per-key chains and is never walked by list position lookup.
	keyed := doc.GetMap("m")
	for i := 0; i < 100; i++ {
		keyed.Set("k", i)
	}
	if got := assertLinkedItemCount(t, keyed); got != 0 {
		t.Fatalf("map history contributed %d list Items, want 0", got)
	}
}

func TestLinkedItemCountResetsWithNestedTypeGC(t *testing.T) {
	doc := newDoc("gc", true, defaultGCFilter, nil, false, WithClientID(1))
	outer := doc.GetArray("outer")
	inner := NewYArray()
	outer.Insert(0, ArrayAny{inner})
	for i := 0; i < 100; i++ {
		inner.Insert(Number(i), ArrayAny{i})
	}
	if got := assertLinkedItemCount(t, inner); got == 0 {
		t.Fatal("nested type never acquired linked Items")
	}
	outer.Delete(0, 1)
	if inner.startItem() != nil {
		t.Fatal("nested type GC retained its linked-list head")
	}
	if got := listItemCount(inner); got != 0 {
		t.Fatalf("nested type GC retained linked Item count %d", got)
	}
}

func TestLinkedItemCountUsesPhysicalItemsNotVisibleLength(t *testing.T) {
	doc := newDoc("density", false, defaultGCFilter, nil, false, WithClientID(1))
	text := doc.GetText("t")
	text.Insert(0, string(make([]byte, 100_000)), Object{})
	if got := assertLinkedItemCount(t, text); got != 1 {
		t.Fatalf("100k-character paste produced %d Items, want 1", got)
	}

	rng := markerLCG(0x51A7E)
	for i := 0; i < 1_000; i++ {
		text.Insert(Number(rng(text.Length()+1)), "x", Object{})
	}
	items := assertLinkedItemCount(t, text)
	if limit := searchMarkerLimit(items); limit != maxSearchMarker {
		t.Fatalf("sparse paste/edit shape has %d Items and limit %d, want %d", items, limit, maxSearchMarker)
	}
}
