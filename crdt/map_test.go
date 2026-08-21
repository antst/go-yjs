package crdt

import (
	"bytes"
	"reflect"
	"testing"
)

// ---------------------------------------------------------------- from map_attribute_cache_test.go
func TestRootAttributeChangesPreserveIndependentReadCaches(t *testing.T) {
	doc := newDoc("attribute-cache-independence", false, nil, nil, false)
	text := doc.GetText("text")
	text.Insert(0, "abc", Object{})
	wantDelta := text.ToDelta(nil, nil, nil)
	_ = text.ToDelta(nil, nil, nil)
	deltaCache := text.deltaCache.Load()
	if deltaCache == nil {
		t.Fatal("fixture did not prime the text delta cache")
	}
	text.SetAttribute("lang", "en")
	if text.deltaCache.Load() != deltaCache {
		t.Fatal("root attribute change invalidated a delta cache that does not project root attributes")
	}
	if got := text.ToDelta(nil, nil, nil); !reflect.DeepEqual(got, wantDelta) {
		t.Fatalf("root attribute changed content delta: got=%v want=%v", got, wantDelta)
	}
	if got := text.GetAttribute("lang"); got != "en" {
		t.Fatalf("root attribute = %v, want en", got)
	}

	fragment := doc.GetXMLFragment("xml")
	element := NewYXmlElement("p")
	element.Insert(0, ArrayAny{"child"})
	fragment.Insert(0, ArrayAny{element})
	wantSlice := element.Slice(0, element.GetLength())
	_ = element.Slice(0, element.GetLength())
	sliceCache := element.sliceCache.Load()
	if sliceCache == nil {
		t.Fatal("fixture did not prime the XML child-slice cache")
	}
	element.SetAttribute("id", "x")
	if element.sliceCache.Load() != sliceCache {
		t.Fatal("attribute change invalidated an XML cache that projects only children")
	}
	if got := element.Slice(0, element.GetLength()); !reflect.DeepEqual(got, wantSlice) {
		t.Fatalf("attribute change altered child slice: got=%v want=%v", got, wantSlice)
	}
}

// ---------------------------------------------------------------- from map_fresh_key_fastpath_test.go
func TestFreshMapKeyFastPathPreservesItemShape(t *testing.T) {
	t.Parallel()

	doc := newDoc("fresh-map-fast-path", false, nil, nil, false, WithClientID(7))
	m := doc.GetMap("m")
	values := []any{1, "two", nil, true}
	Transact(doc, func(*Transaction) {
		for i, value := range values {
			m.Set(string(rune('a'+i)), value)
		}
	}, nil, true)

	structs := doc.store.structsForClient(doc.ClientID)
	if len(structs) != len(values) {
		t.Fatalf("stored %d structs, want %d", len(structs), len(values))
	}
	for i, raw := range structs {
		item, ok := raw.(*itemStruct)
		if !ok {
			t.Fatalf("struct %d has type %T, want *Item", i, raw)
		}
		if item.id.Clock != i || item.length != 1 || item.left != nil || item.right != nil || item.origin != nil {
			t.Fatalf("struct %d has unexpected fresh-key shape: id=%v len=%d left=%p right=%p origin=%v",
				i, item.id, item.length, item.left, item.right, item.origin)
		}
		if item.parent != m || item.parentSub != string(rune('a'+i)) || !item.countable() || item.isDeleted() {
			t.Fatalf("struct %d lost map metadata: parent=%T key=%q info=%08b", i, item.parent, item.parentSub, item.info)
		}
	}

	update, err := EncodeStateAsUpdate(doc, nil)
	if err != nil {
		t.Fatalf("encode fresh map: %v", err)
	}
	clone := newDoc("fresh-map-fast-path-clone", false, nil, nil, false, WithClientID(8))
	_ = ApplyUpdate(clone, update, nil)
	got := clone.GetMap("m")
	for i, want := range values {
		key := string(rune('a' + i))
		value := got.Get(key)
		if want == nil && isUndefined(value) {
			continue
		}
		if value != want {
			t.Fatalf("round-trip key %q = %#v, want %#v", key, value, want)
		}
	}
}

func TestFreshMapKeyFastPathDeletesWithDeletedParent(t *testing.T) {
	t.Parallel()

	doc := newDoc("fresh-map-deleted-parent", false, nil, nil, false, WithClientID(9))
	root := doc.GetMap("root")
	child := NewYMap(nil)
	root.Set("child", child)
	root.Delete("child")

	child.Set("key", "value")
	item := child.getMap()["key"]
	if item == nil || !item.isDeleted() {
		t.Fatalf("item added beneath deleted parent = %v, want deleted item", item)
	}
	if child.Has("key") {
		t.Fatal("item added beneath deleted parent remained visible")
	}
}

// ---------------------------------------------------------------- from map_item_arena_test.go
func TestMapItemArenaKeepsStablePointersAndBoundedTail(t *testing.T) {
	t.Parallel()

	doc := newDoc("map-item-arena", false, nil, nil, false)
	const count = 100
	items := make([]*itemWithSingleAny, count)
	for i := range items {
		items[i] = doc.allocateMapItemStorage()
		items[i].item.id.Clock = i
	}

	for i, storage := range items {
		if storage.item.id.Clock != i {
			t.Fatalf("storage %d moved or was reused: clock=%d", i, storage.item.id.Clock)
		}
	}
	if len(doc.mapItemBlock) > 32 || len(doc.mapItemBlock)-doc.mapItemBlockUsed > 31 {
		t.Fatalf("final block len/used = %d/%d, want bounded tail", len(doc.mapItemBlock), doc.mapItemBlockUsed)
	}
}

// ---------------------------------------------------------------- from map_overwrite_fastpath_test.go
func setPrimitiveMapThroughGenericIntegrate(t *testing.T, doc *Doc, parent abstractType, key string, value any) {
	t.Helper()
	Transact(doc, func(trans *Transaction) {
		left := parent.getMap()[key]
		if left == nil {
			t.Fatal("generic overwrite fixture has no current value")
		}
		clock := getState(doc.store, doc.ClientID)
		storage := doc.allocateMapItemStorage()
		storage.value[0] = value
		storage.content.arr = storage.value[:]
		item := initItemWithLength(&storage.item, GenID(doc.ClientID, clock), left,
			getItemLastID(left), nil, nil, parent, key, &storage.content, 1)
		_ = item.integrateStruct(trans, 0)
	}, nil, true)
}

func TestPrimitiveMapOverwriteFastPathMatchesGenericEncoding(t *testing.T) {
	fastDoc := newDoc("fast-map-overwrite", false, nil, nil, false)
	genericDoc := newDoc("generic-map-overwrite", false, nil, nil, false)
	genericDoc.ClientID = fastDoc.ClientID
	fast := fastDoc.GetMap("m")
	generic := genericDoc.GetMap("m")

	fast.Set("k", "initial")
	generic.Set("k", "initial")
	fast.Set("k", "live-overwrite")
	setPrimitiveMapThroughGenericIntegrate(t, genericDoc, generic, "k", "live-overwrite")
	fast.Delete("k")
	generic.Delete("k")
	fast.Set("k", "after-delete")
	setPrimitiveMapThroughGenericIntegrate(t, genericDoc, generic, "k", "after-delete")

	if !reflect.DeepEqual(fast.ToJSON(), generic.ToJSON()) {
		t.Fatalf("map state differs: fast=%v generic=%v", fast.ToJSON(), generic.ToJSON())
	}
	fastV1, err := EncodeStateAsUpdate(fastDoc, nil)
	if err != nil {
		t.Fatal(err)
	}
	genericV1, err := EncodeStateAsUpdate(genericDoc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fastV1, genericV1) {
		t.Fatalf("V1 encoding differs:\nfast    %x\ngeneric %x", fastV1, genericV1)
	}
	fastV2, err := EncodeStateAsUpdateV2(fastDoc, nil)
	if err != nil {
		t.Fatal(err)
	}
	genericV2, err := EncodeStateAsUpdateV2(genericDoc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fastV2, genericV2) {
		t.Fatalf("V2 encoding differs:\nfast    %x\ngeneric %x", fastV2, genericV2)
	}
}
