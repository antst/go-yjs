package crdt

import (
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

// ---------------------------------------------------------------- from event_delta_concat_test.go
func TestTextEventDeltaCoalescesBatchedStringItems(t *testing.T) {
	doc := newDoc("event-delta-concat", false, defaultGCFilter, nil, false, WithClientID(1))
	text := doc.GetText("t")
	var got []EventOperator
	text.Observe(func(value interface{}, _ interface{}) {
		got = value.(*YTextEvent).GetDelta()
	})

	const count = 512
	Transact(doc, func(*Transaction) {
		for i := 0; i < count; i++ {
			text.Insert(text.Length(), "x", Object{})
		}
	}, nil, true)

	if len(got) != 1 || got[0].Kind != EventOperatorInsertText || got[0].InsertText != strings.Repeat("x", count) {
		t.Fatalf("batched text event delta = %#v", got)
	}
}

// ---------------------------------------------------------------- from event_operator_union_test.go
func TestEventOperatorTaggedUnionLayoutAndZeroValue(t *testing.T) {
	t.Parallel()

	wantSize := uintptr(56)
	if unsafe.Sizeof(uintptr(0)) == 4 {
		wantSize = 28
	}
	if got := unsafe.Sizeof(EventOperator{}); got != wantSize {
		t.Fatalf("EventOperator size = %d, want %d bytes", got, wantSize)
	}
	var zero EventOperator
	if zero.GetKind() != EventOperatorNone || zero.IsInsert() || zero.InsertValue() != nil || zero.OpLength() != 0 {
		t.Fatalf("zero EventOperator is not a no-op: %#v", zero)
	}

	emptyAttributes := newObject()
	if NewTextDeltaOp("", Object{}).HasAttributes() {
		t.Fatal("nil Object unexpectedly marks attributes present")
	}
	if !NewTextDeltaOp("", emptyAttributes).HasAttributes() {
		t.Fatal("explicit empty Object did not mark attributes present")
	}
	insertNil := NewValueDeltaOp(nil, Object{})
	if !insertNil.IsInsert() || insertNil.GetKind() != EventOperatorInsertValue || insertNil.InsertValue() != nil {
		t.Fatalf("inserted nil lost its value-arm tag: %#v", insertNil)
	}
}

func TestEventOperatorRichDeltaRoundTrip(t *testing.T) {
	t.Parallel()

	doc := newDoc("delta-union-source", false, defaultGCFilter, nil, false, WithClientID(1))
	text := doc.GetText("t")
	text.Insert(0, "abcd", Object{})
	bold := MakeObject("bold", true)
	italic := MakeObject("italic", true)
	embed := MakeObject("image", "cat.png")
	text.ApplyDelta([]EventOperator{
		NewRetainDeltaOp(1, bold),
		NewDeleteDeltaOp(1),
		NewTextDeltaOp("XY", italic),
		NewValueDeltaOp(embed, Object{}),
		NewRetainDeltaOp(2, Object{}),
	}, true)

	first := text.ToDelta(nil, nil, nil)
	replica := newDoc("delta-union-replica", false, defaultGCFilter, nil, false, WithClientID(2))
	replica.GetText("t").ApplyDelta(first, true)
	second := replica.GetText("t").ToDelta(nil, nil, nil)
	if !reflect.DeepEqual(deltaReadShape(first), deltaReadShape(second)) {
		t.Fatalf("rich delta changed across ApplyDelta/ToDelta round trip:\nfirst:  %#v\nsecond: %#v", first, second)
	}
}

// ---------------------------------------------------------------- from event_projection_regression_test.go
// TestEventKeyProjectionMatchesReference pins yjs@13.6.31 YEvent.keys:
// the projection is computed lazily from the transaction and is reused by
// changes.keys. In particular, constructing an event must not seed the cache
// with an empty-but-non-nil map, because that makes every key change disappear.
func TestEventKeyProjectionMatchesReference(t *testing.T) {
	doc := newDoc("event-keys", false, defaultGCFilter, nil, false)
	ym := doc.GetMap("m")

	type observed struct {
		keys       map[string]EventAction
		changeKeys map[string]EventAction
	}
	var events []observed
	ym.Observe(func(value, _ interface{}) {
		event := value.(*YMapEvent)
		changes := event.GetChanges()
		if event.Keys == nil {
			t.Fatal("YMapEvent.GetChanges did not compute the key projection")
		}
		fromChanges, ok := changes.GetOr("keys").(map[string]EventAction)
		if !ok {
			t.Fatalf("changes.keys has type %T, want map[string]EventAction", changes.GetOr("keys"))
		}
		// Read changes FIRST: it has the same lazy-computation obligation as
		// GetKeys and must not merely capture the cache's current nil value.
		keys := event.GetKeys()
		events = append(events, observed{keys: keys, changeKeys: fromChanges})
	})

	ym.Set("k", Number(1))
	ym.Set("k", Number(2))
	ym.Delete("k")
	Transact(doc, func(_ *Transaction) {
		ym.Set("ephemeral", Number(1))
		ym.Delete("ephemeral")
		ym.Set("kept", Number(7))
	}, nil, true)

	if len(events) != 4 {
		t.Fatalf("observed %d events, want 4", len(events))
	}
	wants := []EventAction{
		{Action: ActionAdd},
		{Action: ActionUpdate, OldValue: Number(1)},
		{Action: ActionDelete, OldValue: Number(2)},
	}
	for i, want := range wants {
		got, ok := events[i].keys["k"]
		if !ok {
			t.Errorf("event %d keys=%v, want key k", i, events[i].keys)
			continue
		}
		if got.Action != want.Action || !reflect.DeepEqual(got.OldValue, want.OldValue) {
			t.Errorf("event %d action=%q old=%v, want action=%q old=%v", i, got.Action, got.OldValue, want.Action, want.OldValue)
		}
		if !reflect.DeepEqual(events[i].changeKeys, events[i].keys) {
			t.Errorf("event %d changes.keys=%v, direct keys=%v", i, events[i].changeKeys, events[i].keys)
		}
	}
	if got := events[3].keys; len(got) != 1 || got["kept"].Action != ActionAdd {
		t.Errorf("mixed no-op event keys=%v, want only kept:add", got)
	}
	if !reflect.DeepEqual(events[3].changeKeys, events[3].keys) {
		t.Errorf("mixed no-op changes.keys=%v, direct keys=%v", events[3].changeKeys, events[3].keys)
	}
}

// TestGenericEventChangesComputesKeyProjection exercises YEvent.GetChanges
// directly. YMapEvent has its own override, so the map observer above cannot
// establish that the embedded generic implementation calls GetKeys rather than
// returning the cache's current value.
func TestGenericEventChangesComputesKeyProjection(t *testing.T) {
	doc := newDoc("generic-event-keys", false, defaultGCFilter, nil, false)
	ym := doc.GetMap("m")
	observed := false
	ym.Observe(func(value, _ interface{}) {
		event := value.(*YMapEvent)
		changes := event.YEvent.GetChanges()
		if event.Keys == nil {
			t.Fatal("YEvent.GetChanges did not compute the key projection")
		}
		keys, ok := changes.GetOr("keys").(map[string]EventAction)
		if !ok || keys["k"].Action != ActionAdd {
			t.Fatalf("generic changes.keys=%v (%T), want k:add", changes.GetOr("keys"), changes.GetOr("keys"))
		}
		observed = true
	})

	ym.Set("k", Number(1))
	if !observed {
		t.Fatal("map observer did not run")
	}
}

// TestEventPathUsesVisibleLengthsAndStaysFlat pins yjs@13.6.31 getPathTo.
// ContentAny coalesces adjacent primitive values into one Item whose Length is
// greater than one, while a multi-level path must still be a flat sequence.
func TestEventPathUsesVisibleLengthsAndStaysFlat(t *testing.T) {
	doc := newDoc("event-path", false, defaultGCFilter, nil, false)
	root := doc.GetArray("root")
	level1 := NewYArray()
	level2 := NewYArray()
	target := NewYArray()

	root.Insert(0, ArrayAny{Number(1), Number(2), Number(3), level1})
	level1.Insert(0, ArrayAny{Number(10), Number(11), level2})
	level2.Insert(0, ArrayAny{target})

	want := []interface{}{3, 2, 0}
	if got := getPathTo(root, target); !reflect.DeepEqual(got, want) {
		t.Fatalf("getPathTo(root, target)=%v, want flat path %v", got, want)
	}
}
