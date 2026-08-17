package crdt

import (
	"bytes"
	"reflect"
	"testing"
)

func TestBatchedArrayCleanupPreservesObserverItemBoundaries(t *testing.T) {
	t.Parallel()

	doc := newDoc("array-batch-observer", false, nil, nil, false, WithClientID(17))
	arr := doc.GetArray("a")
	values := ArrayAny{1, 2, 3, 4}
	callbackCount := 0
	arr.Observe(func(value interface{}, _ interface{}) {
		event := value.(*YArrayEvent)
		changes := event.GetChanges()
		if added := changes.GetOr("added").(Set); len(added) != len(values) {
			t.Errorf("observer saw %d added items, want the %d pre-cleanup boundaries", len(added), len(values))
		}
		delta := event.GetDelta()
		if len(delta) != 1 || !delta[0].IsInsert() || !reflect.DeepEqual(delta[0].InsertValue(), values) {
			t.Errorf("observer delta = %#v, want one insert of %#v", delta, values)
		}
		if structs := doc.store.structsForClient(doc.ClientID); len(structs) != len(values) {
			t.Errorf("observer ran with %d stored structs, want %d before cleanup", len(structs), len(values))
		}
		callbackCount++
	})

	Transact(doc, func(*Transaction) {
		for _, value := range values {
			arr.Insert(arr.GetLength(), ArrayAny{value})
		}
	}, nil, true)

	if callbackCount != 1 {
		t.Fatalf("observer called %d times, want 1", callbackCount)
	}
	if structs := doc.store.structsForClient(doc.ClientID); len(structs) != 1 {
		t.Fatalf("cleanup left %d stored structs, want one merged item", len(structs))
	}
}

func TestBatchedArrayObserverAddedMidTransactionSeesEarlierItems(t *testing.T) {
	t.Parallel()

	doc := newDoc("array-batch-mid-observer", false, nil, nil, false, WithClientID(18))
	arr := doc.GetArray("a")
	addedCount := 0
	Transact(doc, func(*Transaction) {
		arr.Insert(0, ArrayAny{"a"})
		arr.Insert(1, ArrayAny{"b"})
		arr.Observe(func(value interface{}, _ interface{}) {
			addedCount = len(value.(*YArrayEvent).GetChanges().GetOr("added").(Set))
		})
		arr.Insert(2, ArrayAny{"c"})
		arr.Insert(3, ArrayAny{"d"})
	}, nil, true)

	if addedCount != 4 {
		t.Fatalf("mid-transaction observer saw %d added items, want 4", addedCount)
	}
}

func TestBatchedArrayCleanupPreservesNestedDeepObserver(t *testing.T) {
	t.Parallel()

	doc := newDoc("array-batch-deep-observer", false, nil, nil, false, WithClientID(20))
	root := doc.GetArray("root")
	child := NewYArray()
	root.Push(ArrayAny{child})
	addedCount := 0
	root.ObserveDeep(func(value interface{}, _ interface{}) {
		for _, event := range value.([]IEventType) {
			if event.GetTarget() == child {
				addedCount = len(event.(*YArrayEvent).GetChanges().GetOr("added").(Set))
			}
		}
	})

	Transact(doc, func(*Transaction) {
		for i := 0; i < 4; i++ {
			child.Insert(child.GetLength(), ArrayAny{i})
		}
	}, nil, true)

	if addedCount != 4 {
		t.Fatalf("nested deep observer saw %d added items, want 4", addedCount)
	}
}

func TestBatchedArrayCleanupMatchesPerOperationWire(t *testing.T) {
	t.Parallel()

	build := func(guid string, batched bool) *Doc {
		doc := newDoc(guid, false, nil, nil, false, WithClientID(19))
		arr := doc.GetArray("a")
		insert := func() {
			for i := 0; i < 200; i++ {
				arr.Insert(arr.GetLength(), ArrayAny{i})
			}
		}
		if batched {
			Transact(doc, func(*Transaction) { insert() }, nil, true)
		} else {
			insert()
		}
		return doc
	}

	perOperation := build("array-per-operation", false)
	batched := build("array-batched", true)
	for _, version := range []struct {
		name   string
		encode func(*Doc, []byte) ([]byte, error)
	}{
		{name: "v1", encode: EncodeStateAsUpdate},
		{name: "v2", encode: EncodeStateAsUpdateV2},
	} {
		want, err := version.encode(perOperation, nil)
		if err != nil {
			t.Fatalf("encode per-operation %s: %v", version.name, err)
		}
		got, err := version.encode(batched, nil)
		if err != nil {
			t.Fatalf("encode batched %s: %v", version.name, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("batched %s update differs from equivalent per-operation update", version.name)
		}
	}
}
