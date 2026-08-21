package crdt

import (
	"slices"
	"testing"
)

const appendKeysLargeBufferCapacity = 64

func TestYMapAppendKeysCachedResultIsCallerOwned(t *testing.T) {
	doc := newDoc("map-append-keys-cache", false, defaultGCFilter, nil, false, WithClientID(1))
	ym := doc.GetMap("m")
	ym.Set("a", 1)
	ym.Set("b", 2)
	_, _ = ym.Keys(), ym.Keys()
	cached := ym.keysCache.Load()
	if cached == nil {
		t.Fatal("Keys reads did not populate the cache")
	}
	want := slices.Clone(cached.keys)

	// Model the dangerous reuse shape explicitly: a large caller buffer against a
	// small cached map. Its capacity greatly exceeds the number of appended keys.
	dst := make([]string, 1, appendKeysLargeBufferCapacity)
	dst[0] = "prefix"
	got := ym.AppendKeys(dst)
	if got[0] != "prefix" || !slices.Equal(got[1:], want) {
		t.Fatalf("AppendKeys result = %q, want prefix followed by %q", got, want)
	}
	got[1] = "caller mutation"
	if !slices.Equal(cached.keys, want) {
		t.Fatalf("caller mutation changed cached keys: got %q, want %q", cached.keys, want)
	}
	if next := ym.AppendKeys(nil); !slices.Equal(next, want) {
		t.Fatalf("caller mutation changed later result: got %q, want %q", next, want)
	}
}

func TestYMapAppendKeysNilDestinationFromKeysCacheIsCallerOwned(t *testing.T) {
	doc := newDoc("map-append-keys-cache-nil", false, defaultGCFilter, nil, false, WithClientID(1))
	ym := doc.GetMap("m")
	ym.Set("a", 1)
	ym.Set("b", 2)
	_, _ = ym.Keys(), ym.Keys()
	cached := ym.keysCache.Load()
	if cached == nil {
		t.Fatal("Keys reads did not populate the cache")
	}
	want := slices.Clone(cached.keys)

	got := ym.AppendKeys(nil)
	if !slices.Equal(got, want) {
		t.Fatalf("AppendKeys(nil) = %q, want %q", got, want)
	}
	got[0] = "caller mutation"
	if !slices.Equal(cached.keys, want) {
		t.Fatalf("AppendKeys(nil) result aliases cached keys: got %q, want %q", cached.keys, want)
	}
	if next := ym.AppendKeys(nil); !slices.Equal(next, want) {
		t.Fatalf("caller mutation changed later result: got %q, want %q", next, want)
	}
}

func TestYMapAppendKeysCacheNeverAliasesDestination(t *testing.T) {
	doc := newDoc("map-append-keys-prime", false, defaultGCFilter, nil, false, WithClientID(1))
	ym := doc.GetMap("m")
	ym.Set("a", 1)
	ym.Set("b", 2)

	// Cache priming must clone the appended suffix even when the caller's spare
	// capacity is much larger than the map.
	dst := make([]string, 0, appendKeysLargeBufferCapacity)
	dst = ym.AppendKeys(dst)
	if ym.keysCache.Load() != nil {
		t.Fatal("first read unexpectedly populated the keys cache")
	}
	dst = ym.AppendKeys(dst[:0])
	cached := ym.keysCache.Load()
	if cached == nil {
		t.Fatal("second read did not populate the keys cache")
	}
	want := slices.Clone(cached.keys)
	dst[0] = "caller mutation"
	if !slices.Equal(cached.keys, want) {
		t.Fatalf("cache aliases caller destination: got %q, want %q", cached.keys, want)
	}

	// A populated cache cannot be stale after a supported write: same-document
	// concurrent mutation is outside the concurrency contract, while serialized
	// Set/Delete/Clear and remote integration synchronously invalidate read caches
	// before returning or dispatching observers.
	ym.Set("c", 3)
	if ym.keysCache.Load() != nil {
		t.Fatal("serialized write left a stale keys cache reachable")
	}
}

func TestYMapAppendKeysFromJSONCacheIsCallerOwned(t *testing.T) {
	doc := newDoc("map-append-keys-json", false, defaultGCFilter, nil, false, WithClientID(1))
	ym := doc.GetMap("m")
	ym.Set("a", 1)
	ym.Set("b", 2)
	for i := 0; i < yMapEntriesCacheThreshold; i++ {
		_ = ym.ToJSON()
	}
	cached := ym.jsonCache.Load()
	if cached == nil {
		t.Fatal("ToJSON reads did not populate the JSON cache")
	}
	want := cached.value.Keys()

	dst := make([]string, 1, 1+len(want))
	dst[0] = "prefix"
	got := ym.AppendKeys(dst)
	if got[0] != "prefix" || !slices.Equal(got[1:], want) {
		t.Fatalf("AppendKeys result = %q, want prefix followed by %q", got, want)
	}
	got[1] = "caller mutation"
	if next := ym.AppendKeys(nil); !slices.Equal(next, want) {
		t.Fatalf("caller mutation changed JSON-cached keys: got %q, want %q", next, want)
	}
}

func TestYMapAppendKeysNilDestinationFromJSONCacheIsCallerOwned(t *testing.T) {
	doc := newDoc("map-append-keys-json-nil", false, defaultGCFilter, nil, false, WithClientID(1))
	ym := doc.GetMap("m")
	ym.Set("a", 1)
	ym.Set("b", 2)
	ym.Set("c", 3)
	for i := 0; i < yMapEntriesCacheThreshold; i++ {
		_ = ym.ToJSON()
	}
	cached := ym.jsonCache.Load()
	if cached == nil {
		t.Fatal("ToJSON reads did not populate the JSON cache")
	}
	if cached.value.d == nil || cached.value.d.large == nil {
		t.Fatal("test fixture did not exercise the large JSON-cache key slice")
	}
	want := cached.value.Keys()

	got := ym.AppendKeys(nil)
	if !slices.Equal(got, want) {
		t.Fatalf("AppendKeys(nil) = %q, want %q", got, want)
	}
	got[0] = "caller mutation"
	if cachedKeys := cached.value.Keys(); !slices.Equal(cachedKeys, want) {
		t.Fatalf("AppendKeys(nil) result aliases JSON-cached keys: got %q, want %q", cachedKeys, want)
	}
	if next := ym.AppendKeys(nil); !slices.Equal(next, want) {
		t.Fatalf("caller mutation changed later result: got %q, want %q", next, want)
	}
}

func TestYMapAppendKeysEmptyPreservesPrefix(t *testing.T) {
	ym := NewYMap(nil)
	dst := []string{"prefix"}
	got := ym.AppendKeys(dst)
	if !slices.Equal(got, dst) {
		t.Fatalf("AppendKeys on empty map = %q, want %q", got, dst)
	}
}
