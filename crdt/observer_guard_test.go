package crdt

import "testing"

func TestObserverHandlersStayLazyBehindPublicAccessors(t *testing.T) {
	types := []abstractType{
		NewYArray(), NewYMap(nil), NewYText(""), newYString(""),
		NewYXmlFragment(), NewYXmlElement("div"), NewYXmlText(), newYXmlHook("hook"),
	}
	for _, value := range types {
		if hasTypeObservers(value) {
			t.Fatalf("new %T unexpectedly reports an observer", value)
		}
		if eh, deh := existingTypeEventHandlers(value); eh != nil || deh != nil {
			t.Fatalf("observer probe materialized handlers for %T", value)
		}
	}

	array := types[0].(*YArray)
	eh, deh := array.getEventHandler(), array.getDeepEventHandler()
	if eh == nil || deh == nil {
		t.Fatal("public observer-handler accessors returned nil")
	}
	if array.getEventHandler() != eh || array.getDeepEventHandler() != deh {
		t.Fatal("public observer-handler accessors did not return stable handlers")
	}
}

func TestObserverAddedDuringTransactionReceivesEvent(t *testing.T) {
	t.Parallel()

	doc := newDoc("observer-mid-transaction", false, nil, nil, false, WithClientID(1))
	text := doc.GetText("text")
	calls := 0

	Transact(doc, func(_ *Transaction) {
		text.Insert(0, "x", Object{})
		text.Observe(func(event interface{}, _ interface{}) {
			if _, ok := event.(*YTextEvent); !ok {
				t.Errorf("observer event has type %T, want *YTextEvent", event)
			}
			calls++
		})
	}, nil, true)

	if calls != 1 {
		t.Fatalf("observer added during transaction called %d times, want 1", calls)
	}
}

func TestAncestorDeepObserverReceivesChildEvent(t *testing.T) {
	t.Parallel()

	doc := newDoc("observer-deep", false, nil, nil, false, WithClientID(1))
	root := doc.GetMap("root")
	child := NewYText("")
	root.Set("child", child)

	var events []IEventType
	root.ObserveDeep(func(value interface{}, _ interface{}) {
		var ok bool
		events, ok = value.([]IEventType)
		if !ok {
			t.Errorf("deep observer value has type %T, want []IEventType", value)
			return
		}
		for _, event := range events {
			if event.GetCurrentTarget() != root {
				t.Errorf("event current target during callback = %T, want root YMap", event.GetCurrentTarget())
			}
		}
	})
	child.Insert(0, "x", Object{})

	if len(events) != 1 {
		t.Fatalf("deep observer received %d events, want 1", len(events))
	}
	if events[0].GetTarget() != child {
		t.Fatalf("event target = %T, want child YText", events[0].GetTarget())
	}
}

func TestDirectObserverOnNestedTypeReceivesEvent(t *testing.T) {
	t.Parallel()

	doc := newDoc("observer-nested-direct", false, nil, nil, false, WithClientID(1))
	root := doc.GetMap("root")
	child := NewYText("")
	root.Set("child", child)

	calls := 0
	child.Observe(func(value interface{}, transValue interface{}) {
		event, ok := value.(*YTextEvent)
		if !ok || event.GetTarget() != child {
			t.Errorf("nested observer event = %T/%v, want child YText event", value, ok)
		}
		trans, ok := transValue.(*Transaction)
		if !ok {
			t.Errorf("nested observer transaction has type %T", transValue)
			return
		}
		if trans.meta == nil || trans.subdocsAdded == nil || trans.deleteSet.clients == nil {
			t.Fatal("nested observer saw a partially materialized transaction")
		}
		calls++
	})

	child.Insert(0, "x", Object{})
	if calls != 1 {
		t.Fatalf("nested direct observer called %d times, want 1", calls)
	}
}

func TestPublicTransactKeepsWritableEmptyFields(t *testing.T) {
	t.Parallel()

	doc := newDoc("public-transaction-fields", false, nil, nil, false, WithClientID(1))
	Transact(doc, func(trans *Transaction) {
		if trans.afterState == nil || trans.changedParentTypes == nil || trans.meta == nil ||
			trans.subdocsAdded == nil || trans.subdocsRemoved == nil || trans.subdocsLoaded == nil ||
			trans.deleteSet == nil || trans.deleteSet.clients == nil {
			t.Fatal("exported Transact exposed nil transaction fields")
		}
		if trans.changedTypesInternal() == nil {
			t.Fatal("ChangedTypes returned a nil writable map")
		}
		trans.meta["test"] = NewSet()
	}, nil, true)
}

func TestDocumentObserverRetainsChangedParentTypes(t *testing.T) {
	t.Parallel()

	doc := newDoc("observer-transaction", false, nil, nil, false, WithClientID(1))
	text := doc.GetText("text")
	sawEvent := false
	doc.On("afterTransaction", NewObserverHandler(func(values ...interface{}) {
		trans := values[0].(*Transaction)
		events, ok := trans.changedParentTypes[text]
		if ok && len(events) == 1 && events[0].GetTarget() == text {
			sawEvent = true
		}
	}))

	text.Insert(0, "x", Object{})
	if !sawEvent {
		t.Fatal("document observer did not receive the type event graph")
	}
}

func TestObserverGuardPreservesUndoScopeTracking(t *testing.T) {
	t.Parallel()

	doc := newDoc("observer-undo", false, nil, nil, false, WithClientID(1))
	value := doc.GetMap("value")
	manager := newUndoManager(value, 500, nil, nil)

	value.Set("key", "value")
	if !manager.CanUndo() {
		t.Fatal("UndoManager did not track a change without type observers")
	}
	manager.Undo()
	if value.Has("key") {
		t.Fatal("undo did not revert the tracked map change")
	}
}

func TestDirectObserverDispatchUsesChangedSnapshot(t *testing.T) {
	t.Parallel()

	doc := newDoc("observer-changed-snapshot", false, nil, nil, false, WithClientID(1))
	a := doc.GetText("a")
	b := doc.GetText("b")
	bCalls := 0
	b.Observe(func(interface{}, interface{}) { bCalls++ })
	a.Observe(func(_ interface{}, transValue interface{}) {
		trans := transValue.(*Transaction)
		trans.changedTypesInternal()[b] = newChangedSubs()
	})

	a.Insert(0, "x", Object{})
	if bCalls != 0 {
		t.Fatalf("observer added through Changed mutation fired %d times in the current snapshot, want 0", bCalls)
	}
}

func TestDeepObserverDispatchUsesParentEventSnapshot(t *testing.T) {
	t.Parallel()

	doc := newDoc("observer-parent-snapshot", false, nil, nil, false, WithClientID(1))
	a := doc.GetText("a")
	b := doc.GetText("b")
	bCalls := 0
	b.ObserveDeep(func(interface{}, interface{}) { bCalls++ })
	a.ObserveDeep(func(value interface{}, transValue interface{}) {
		trans := transValue.(*Transaction)
		trans.changedParentTypes[b] = value.([]IEventType)
	})

	a.Insert(0, "x", Object{})
	if bCalls != 0 {
		t.Fatalf("deep observer added through parent-event mutation fired %d times in the current snapshot, want 0", bCalls)
	}
}
