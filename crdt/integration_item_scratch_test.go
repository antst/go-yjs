package crdt

import "testing"

func TestIntegrationItemSetReusableOverflowClearsBetweenUses(t *testing.T) {
	items := make([]itemStruct, 7)
	pointers := make([]*itemStruct, len(items))
	for i := range items {
		pointers[i] = &items[i]
	}

	var reusable map[*itemStruct]struct{}
	set := integrationItemSet{reusable: reusable}
	for _, item := range pointers[:6] {
		set.Add(item)
	}
	if set.overflow == nil {
		t.Fatal("adding more than the inline capacity did not activate reusable overflow storage")
	}
	for _, item := range pointers[:6] {
		if !set.Has(item) {
			t.Fatalf("promoted set lost item %p", item)
		}
	}

	set.Reset()
	for _, item := range pointers[:6] {
		if set.Has(item) {
			t.Fatalf("reset set retained stale item %p", item)
		}
	}
	set.Add(pointers[6])
	if !set.Has(pointers[6]) {
		t.Fatal("reset set did not accept a new item")
	}
	reusable = set.Release()
	if len(reusable) != 0 {
		t.Fatalf("released reusable overflow retains %d items", len(reusable))
	}

	// Warm the reusable map to the exact shape below, then require subsequent
	// overflow lifetimes to reuse its buckets. This is the allocation property
	// the remote-integration scratch relies on; rebuilding the map on each Item
	// turns a two-peer merge back into hundreds of allocations.
	warm := integrationItemSet{reusable: reusable}
	for _, item := range pointers[:6] {
		warm.Add(item)
	}
	reusable = warm.Release()
	if got := testing.AllocsPerRun(100, func() {
		s := integrationItemSet{reusable: reusable}
		for _, item := range pointers[:6] {
			s.Add(item)
		}
		reusable = s.Release()
	}); got != 0 {
		t.Fatalf("reusing a warmed integration overflow map allocates %.2f times/run, want 0", got)
	}
}

func TestConflictHeavyRemoteIntegrationAllocationBudget(t *testing.T) {
	buildUpdate := func(client Number) []uint8 {
		doc := newDoc("conflict-scratch", false, defaultGCFilter, nil, false, WithClientID(client))
		text := doc.GetText("t")
		rng := newPerfLCG()
		for i := 0; i < perfSmall; i++ {
			index := 0
			if length := text.Length(); length > 0 {
				index = rng.intn(length + 1)
			}
			text.Insert(index, "x", Object{})
		}
		update, err := EncodeStateAsUpdate(doc, nil)
		if err != nil {
			t.Fatal(err)
		}
		return update
	}

	first := buildUpdate(1)
	second := buildUpdate(2)
	apply := func() *Doc {
		doc := newDoc("conflict-scratch", false, defaultGCFilter, nil, false, WithClientID(999))
		_ = ApplyUpdate(doc, first, nil)
		_ = ApplyUpdate(doc, second, nil)
		return doc
	}

	doc := apply()
	text := doc.GetText("t")
	if got, want := text.Length(), Number(2*perfSmall); got != want {
		t.Fatalf("merged length = %d, want %d", got, want)
	}
	if got := listItemCount(text); got < Number(2*perfSmall-10) {
		t.Fatalf("fixture coalesced to %d physical Items; need roughly %d to exercise conflict overflow", got, 2*perfSmall)
	}

	// This ceiling states the production-path property, not a microbenchmark:
	// the two overflow maps are allocated at most once per integrateStructs call
	// and reused across the conflict-heavy remote Items. Reverting the scratch
	// raises this fixture from about 213 allocations to about 366.
	if got := testing.AllocsPerRun(10, func() { _ = apply() }); got > 250 {
		t.Fatalf("conflict-heavy two-peer merge allocates %.0f times/run, want <= 250", got)
	}
}
