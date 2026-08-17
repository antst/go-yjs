package crdt

import (
	"reflect"
	"testing"
)

func TestArrayEventDeltaFlattensAdjacentItemContent(t *testing.T) {
	t.Parallel()

	doc := newDoc("array-event-flat-delta", false, nil, nil, false, WithClientID(16))
	arr := doc.GetArray("a")
	want := ArrayAny{1, 2, 3}
	var got interface{}
	arr.Observe(func(value interface{}, _ interface{}) {
		delta := value.(*YArrayEvent).GetDelta()
		if len(delta) == 1 && delta[0].IsInsert() {
			got = delta[0].InsertValue()
		}
	})

	Transact(doc, func(*Transaction) {
		for _, value := range want {
			arr.Insert(arr.GetLength(), ArrayAny{value})
		}
	}, nil, true)

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("array event insert = %#v, want flat %#v", got, want)
	}
}
