package crdt

import "testing"

func TestYMapSizeTracksLocalRemoteAndConflictingEntries(t *testing.T) {
	prelim := NewYMap(map[string]interface{}{"a": 1, "b": 2})
	if got := prelim.GetSize(); got != 2 {
		t.Fatalf("preliminary size = %d, want 2", got)
	}

	left := newDoc("map-size", false, defaultGCFilter, nil, false, WithClientID(1))
	lm := left.GetMap("m")
	if got := lm.GetSize(); got != 0 {
		t.Fatalf("empty size = %d, want 0", got)
	}
	lm.Set("a", 1)
	lm.Set("b", 2)
	lm.Set("a", 3) // replacing a live value must not change the count
	if got := lm.GetSize(); got != 2 {
		t.Fatalf("size after overwrite = %d, want 2", got)
	}
	lm.Delete("b")
	lm.Delete("missing")
	if got := lm.GetSize(); got != 1 {
		t.Fatalf("size after delete = %d, want 1", got)
	}

	base, err := EncodeStateAsUpdateV2(left, nil)
	if err != nil {
		t.Fatal(err)
	}
	right := newDoc("map-size", false, defaultGCFilter, nil, false, WithClientID(2))
	_ = ApplyUpdateV2(right, base, nil)
	rm := right.GetMap("m")
	if got := rm.GetSize(); got != 1 {
		t.Fatalf("remote initial size = %d, want 1", got)
	}

	// Both peers replace the same live key concurrently; convergence keeps one
	// visible key, not one entry per conflicting item.
	lm.Set("a", "left")
	rm.Set("a", "right")
	lu, err := EncodeStateAsUpdateV2(left, nil)
	if err != nil {
		t.Fatal(err)
	}
	ru, err := EncodeStateAsUpdateV2(right, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = ApplyUpdateV2(left, ru, nil)
	_ = ApplyUpdateV2(right, lu, nil)
	if got := lm.GetSize(); got != 1 {
		t.Fatalf("left size after conflict = %d, want 1", got)
	}
	if got := rm.GetSize(); got != 1 {
		t.Fatalf("right size after conflict = %d, want 1", got)
	}
}

func TestYMapKeysCacheIsolatedAndInvalidated(t *testing.T) {
	doc := newDoc("map-keys-cache", false, defaultGCFilter, nil, false, WithClientID(1))
	ym := doc.GetMap("m")
	ym.Set("a", 1)
	ym.Set("b", 2)
	_ = ym.Keys()
	keys := ym.Keys()
	if ym.keysCache.Load() == nil {
		t.Fatal("second unchanged Keys read did not populate the cache")
	}
	keys[0] = "caller mutation"
	for _, key := range ym.Keys() {
		if key == "caller mutation" {
			t.Fatal("caller mutation changed cached keys")
		}
	}

	ym.Set("c", 3)
	if ym.keysCache.Load() != nil {
		t.Fatal("local set did not invalidate keys cache")
	}
	if got := len(ym.Keys()); got != 3 {
		t.Fatalf("keys after local set = %d, want 3", got)
	}

	replica := newDoc("map-keys-cache", false, defaultGCFilter, nil, false, WithClientID(2))
	update, err := EncodeStateAsUpdateV2(doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = ApplyUpdateV2(replica, update, nil)
	rm := replica.GetMap("m")
	_, _ = rm.Keys(), rm.Keys()
	ym.Set("d", 4)
	update, err = EncodeStateAsUpdateV2(doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = ApplyUpdateV2(replica, update, nil)
	if rm.keysCache.Load() != nil {
		t.Fatal("remote set did not invalidate keys cache")
	}
	if got := len(rm.Keys()); got != 4 {
		t.Fatalf("keys after remote set = %d, want 4", got)
	}
}

func TestYMapEntriesCacheIsolatedAndInvalidated(t *testing.T) {
	doc := newDoc("map-entries-cache", false, defaultGCFilter, nil, false, WithClientID(1))
	ym := doc.GetMap("m")
	ym.Set("a", 1)
	ym.Set("b", 2)
	var entries map[string]interface{}
	for i := 0; i < yMapEntriesCacheThreshold; i++ {
		entries = ym.Entries()
	}
	if ym.entriesCache.Load() == nil {
		t.Fatal("sustained Entries reads did not populate the cache")
	}
	entries["a"] = "caller mutation"
	entries["injected"] = true
	next := ym.Entries()
	if next["a"] != 1 {
		t.Fatalf("caller mutation changed cached value: %#v", next["a"])
	}
	if _, exists := next["injected"]; exists {
		t.Fatal("caller insertion changed cached entries")
	}

	ym.Set("c", 3)
	if ym.entriesCache.Load() != nil || ym.entriesReads.Load() != 0 {
		t.Fatal("map mutation did not reset the entries cache")
	}
	if got := len(ym.Entries()); got != 3 {
		t.Fatalf("entries after mutation = %d, want 3", got)
	}
}

func TestYMapJSONCacheIsolatedBoundedAndInvalidated(t *testing.T) {
	doc := newDoc("map-json-cache", false, defaultGCFilter, nil, false, WithClientID(1))
	ym := doc.GetMap("m")
	ym.Set("a", 1)
	ym.Set("b", 2)
	var rendered Object
	for i := 0; i < yMapEntriesCacheThreshold; i++ {
		rendered = ym.ToJSON().(Object)
	}
	if ym.jsonCache.Load() == nil {
		t.Fatal("sustained ToJSON reads did not populate the cache")
	}
	if ym.entriesCache.Load() != nil || ym.keysCache.Load() != nil {
		t.Fatal("JSON projection retained duplicate map read caches")
	}
	rendered.Set("a", "caller mutation")
	rendered.Set("injected", true)
	next := ym.ToJSON().(Object)
	if got := next.GetOr("a"); got != 1 {
		t.Fatalf("caller mutation changed cached JSON value: %#v", got)
	}
	if next.Has("injected") {
		t.Fatal("caller insertion changed cached JSON entries")
	}
	if got := ym.Entries()["b"]; got != 2 {
		t.Fatalf("Entries did not reuse the JSON projection safely: %#v", got)
	}
	if got := len(ym.Keys()); got != 2 {
		t.Fatalf("Keys did not reuse the JSON projection safely: %d", got)
	}

	ym.Set("c", 3)
	if ym.jsonCache.Load() != nil || ym.jsonReads.Load() != 0 {
		t.Fatal("local map mutation did not reset the JSON cache")
	}
	if got := ym.ToJSON().(Object).Len(); got != 3 {
		t.Fatalf("JSON after mutation has %d entries, want 3", got)
	}
	replica := newDoc("map-json-cache", false, defaultGCFilter, nil, false, WithClientID(2))
	update, err := EncodeStateAsUpdateV2(doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = ApplyUpdateV2(replica, update, nil)
	remote := replica.GetMap("m")
	for i := 0; i < yMapEntriesCacheThreshold; i++ {
		_ = remote.ToJSON()
	}
	if remote.jsonCache.Load() == nil {
		t.Fatal("remote fixture did not populate the JSON cache")
	}
	ym.Set("d", 4)
	update, err = EncodeStateAsUpdateV2(doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = ApplyUpdateV2(replica, update, nil)
	if remote.jsonCache.Load() != nil || remote.jsonReads.Load() != 0 {
		t.Fatal("remote map mutation did not reset the JSON cache")
	}
	if got := remote.ToJSON().(Object).Len(); got != 4 {
		t.Fatalf("remote JSON after mutation has %d entries, want 4", got)
	}

	// Nested shared types render newly-created mutable values. Do not cache them shallowly: a
	// caller mutating a nested rendering must not change what a later ToJSON observes.
	nested := NewYMap(map[string]interface{}{"n": 1})
	ym.Set("nested", nested)
	for i := 0; i < yMapEntriesCacheThreshold+2; i++ {
		_ = ym.ToJSON()
	}
	if ym.jsonCache.Load() != nil {
		t.Fatal("map with a nested shared type populated the shallow JSON cache")
	}
	withUndefined := doc.GetMap("undefined")
	withUndefined.Set("u", Undefined)
	for i := 0; i < yMapEntriesCacheThreshold+2; i++ {
		_ = withUndefined.ToJSON()
	}
	if withUndefined.jsonCache.Load() != nil {
		t.Fatal("undefined-valued map populated a projection that would omit a live key")
	}
}

func TestYMapClearMoreThanUint8DeleteItemsRoundTrips(t *testing.T) {
	source := newDoc("map-clear-large", false, defaultGCFilter, nil, false, WithClientID(1))
	sm := source.GetMap("m")
	for i := 0; i < 300; i++ {
		sm.Set(mapKey(i), i)
	}
	beforeClear, err := EncodeStateAsUpdateV2(source, nil)
	if err != nil {
		t.Fatal(err)
	}
	replica := newDoc("map-clear-large", false, defaultGCFilter, nil, false, WithClientID(2))
	_ = ApplyUpdateV2(replica, beforeClear, nil)

	sm.Clear()
	if got := sm.GetSize(); got != 0 {
		t.Fatalf("source size after clear = %d, want 0", got)
	}
	afterClear, err := EncodeStateAsUpdateV2(source, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = ApplyUpdateV2(replica, afterClear, nil)
	rm := replica.GetMap("m")
	if got := rm.GetSize(); got != 0 {
		t.Fatalf("replica size after clear = %d, want 0", got)
	}
	if got := len(rm.Entries()); got != 0 {
		t.Fatalf("replica entries after clear = %d, want 0", got)
	}
}

func TestYMapLargeObservedClearReportsEveryKey(t *testing.T) {
	doc := newDoc("map-clear-observed", false, defaultGCFilter, nil, false, WithClientID(1))
	ym := doc.GetMap("m")
	for i := 0; i < 300; i++ {
		ym.Set(mapKey(i), i)
	}
	calls := 0
	changedKeys := 0
	ym.Observe(func(value interface{}, _ interface{}) {
		calls++
		event, ok := value.(*YMapEvent)
		if !ok {
			t.Fatalf("observer event type = %T, want *YMapEvent", value)
		}
		changedKeys = len(event.KeysChanged)
	})
	ym.Clear()
	if calls != 1 {
		t.Fatalf("observer calls = %d, want 1", calls)
	}
	if changedKeys != 300 {
		t.Fatalf("changed keys = %d, want 300", changedKeys)
	}
}
