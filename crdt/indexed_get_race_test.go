package crdt

import (
	"reflect"
	"sync"
	"testing"
)

// Indexed reads must not write to shared state. Before the immutable read index, typeListGet
// called findMarker, which refreshes timestamps, overwrites marker entries, and flips Item marker
// bits. Get(i) for i>0 therefore raced with another reader of the same quiescent document.
func TestIndexedGetIsRaceFreeOnQuiescentArray(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	arr := doc.GetArray("a")
	const n = 5000
	for i := 0; i < n; i++ {
		arr.Insert(i, ArrayAny{i})
	}

	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for r := 0; r < 500; r++ {
				idx := 1 + (w*997+r*31)%(n-1)
				if got, ok := arr.Get(idx).(int); !ok || got != idx {
					t.Errorf("worker %d: Get(%d) = %v, want %d", w, idx, got, idx)
					return
				}
			}
		}(w)
	}
	wg.Wait()
}

// A freshly decoded document has no mutable write markers. Simply making Get read-only without a
// replacement cache made this canonical load-then-read shape roughly 100x slower at 100k items.
// Require both properties at once: concurrent safety and publication of the immutable read index.
func TestIndexedGetIsRaceFreeAndIndexedAfterDecode(t *testing.T) {
	src := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	srcArr := src.GetArray("a")
	const n = 5000
	rng := markerLCG(0xD00D)
	for i := 0; i < n; i++ {
		srcArr.Insert(rng(srcArr.GetLength()+1), ArrayAny{i})
	}
	enc, err := EncodeStateAsUpdateV2(src, nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(2))
	_ = ApplyUpdateV2(doc, enc, nil)
	arr := doc.GetArray("a")
	if arr.GetLength() != Number(n) {
		t.Fatalf("decoded length = %d, want %d", arr.GetLength(), n)
	}

	want := make([]interface{}, n)
	for i := 0; i < n; i++ {
		want[i] = arr.Get(i)
	}
	index := arr.readIndex.Load()
	if index == nil || index == buildingListReadIndex || len(index.positions) < maxSearchMarker/2 {
		t.Fatalf("decoded reads did not publish a useful immutable index: %#v", index)
	}

	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for r := 0; r < 500; r++ {
				idx := 1 + (w*997+r*31)%(n-1)
				if got := arr.Get(idx); got != want[idx] {
					t.Errorf("worker %d: Get(%d) = %v, want %v", w, idx, got, want[idx])
					return
				}
			}
		}(w)
	}
	wg.Wait()
}

func TestIndexedGetInvalidatesAfterMutation(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	arr := doc.GetArray("a")
	rng := markerLCG(0xCAFE)
	model := make([]int, 0, 5000)
	for i := 0; i < 5000; i++ {
		at := rng(len(model) + 1)
		arr.Insert(at, ArrayAny{i})
		model = append(model, 0)
		copy(model[at+1:], model[at:])
		model[at] = i
	}
	_ = arr.Get(2500)
	before := arr.readIndex.Load()
	if before == nil || before == buildingListReadIndex {
		t.Fatal("read index was not published")
	}

	arr.Insert(1234, ArrayAny{"new"})
	if arr.readIndex.Load() != nil {
		t.Fatal("insert did not invalidate read index")
	}
	model = append(model, 0)
	copy(model[1235:], model[1234:])
	model[1234] = -1
	arr.Delete(3456, 1)
	model = append(model[:3456], model[3457:]...)

	for i := range model {
		got := arr.Get(i)
		if i == 1234 {
			if got != "new" {
				t.Fatalf("Get(%d) = %v, want new", i, got)
			}
		} else if got != model[i] {
			t.Fatalf("Get(%d) = %v, want %d", i, got, model[i])
		}
	}
	after := arr.readIndex.Load()
	if after == nil || after == buildingListReadIndex || after == before {
		t.Fatal("reads did not rebuild a fresh index after mutation")
	}
}

func TestIndexedGetInvalidatesAfterRemoteMutation(t *testing.T) {
	source := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	arr := source.GetArray("a")
	rng := markerLCG(0xF00D)
	for i := 0; i < 5000; i++ {
		arr.Insert(rng(arr.GetLength()+1), ArrayAny{i})
	}
	base, err := EncodeStateAsUpdateV2(source, nil)
	if err != nil {
		t.Fatal(err)
	}
	replica := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(2))
	_ = ApplyUpdateV2(replica, base, nil)
	replicaArr := replica.GetArray("a")
	_ = replicaArr.Get(2500)
	before := replicaArr.readIndex.Load()
	if before == nil || before == buildingListReadIndex {
		t.Fatal("read index was not published")
	}

	arr.Insert(1234, ArrayAny{"remote"})
	arr.Delete(3456, 1)
	update, err := EncodeStateAsUpdateV2(source, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = ApplyUpdateV2(replica, update, nil)
	if replicaArr.readIndex.Load() != nil {
		t.Fatal("remote apply did not invalidate read index")
	}
	if got, want := replicaArr.ToArray(), arr.ToArray(); !reflect.DeepEqual(got, want) {
		t.Fatalf("remote result differs after invalidation: got %v want %v", got, want)
	}
	_ = replicaArr.Get(2500)
	after := replicaArr.readIndex.Load()
	if after == nil || after == buildingListReadIndex || after == before {
		t.Fatal("remote result did not rebuild a fresh read index")
	}
}

func TestIndexedGetHonorsReadCacheOption(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1), WithReadCache(false))
	arr := doc.GetArray("a")
	for i := 0; i < 200; i++ {
		arr.Insert(0, ArrayAny{i})
	}
	if got := arr.Get(100); got == nil {
		t.Fatal("indexed read returned nil")
	}
	if arr.readIndex.Load() != nil {
		t.Fatal("WithReadCache(false) retained an indexed-read cache")
	}
}
