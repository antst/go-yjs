package crdt

import "testing"

// Nil-safety and laziness of the event handlers.
//
// EH and DEH stay unallocated until Observe/ObserveDeep, which removes two allocations from every
// type nobody observes -- the common case. The PUBLIC accessors GetEH/GetDEH deliberately
// materialise on demand so they never return nil, keeping the exported surface stable; the
// laziness is preserved internally by hasTypeObservers, which consults the fields without
// materialising them.
//
// That split is the thing to guard. A reader who reaches for GetEH() to ask "are there observers"
// gets a correct answer and silently allocates, defeating the optimization with nothing failing.
// Equally, a future call site that assumes a raw field is non-nil dereferences nil on a document
// that simply never attached an observer -- invisible to any test that observes, which is nearly
// all of them.
//
// So this exercises every observer entry point against a NEVER-OBSERVED type, drives mutation and
// remote integration so the dispatch paths run with unallocated handlers, and asserts the laziness
// by reading the FIELDS rather than through the materialising accessors.

func TestObserverEntryPointsNilSafe(t *testing.T) {
	noop := func(interface{}, interface{}) {}

	t.Run("unobserve without observe", func(t *testing.T) {
		doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
		txt := doc.GetText("t")
		arr := doc.GetArray("a")
		m := doc.GetMap("m")
		f := doc.GetXmlFragment("x")

		// Every removal path on a type whose handler was never allocated.
		txt.Unobserve(noop)
		txt.UnobserveDeep(noop)
		arr.Unobserve(noop)
		arr.UnobserveDeep(noop)
		m.Unobserve(noop)
		m.UnobserveDeep(noop)
		f.Unobserve(noop)
		f.UnobserveDeep(noop)
	})

	t.Run("mutate and commit with no observers", func(t *testing.T) {
		doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
		txt := doc.GetText("t")
		arr := doc.GetArray("a")
		m := doc.GetMap("m")

		// Mutation drives the dispatch paths that read EH/DEH. A nested type is included because
		// deep dispatch walks PARENTS, so it reaches a handler the child never allocated.
		txt.Insert(0, "hello", Object{})
		attr := newObject()
		attr.Set("bold", true)
		txt.Format(0, 3, attr)
		txt.Delete(1, 1)

		nested := NewYMap(nil)
		nested.Set("k", 1)
		arr.Insert(0, ArrayAny{nested})
		nested.Set("k", 2) // mutate the CHILD: deep dispatch climbs to the unobserved parent
		arr.Delete(0, 1)

		m.Set("a", 1)
		m.Delete("a")
	})

	t.Run("remote update into an unobserved document", func(t *testing.T) {
		src := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
		src.GetText("t").Insert(0, "remote", Object{})
		src.GetArray("a").Insert(0, ArrayAny{1, 2, 3})
		enc, err := EncodeStateAsUpdateV2(src, nil)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}

		// The receiver has never observed anything, and integration dispatches change events.
		dst := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(2))
		_ = ApplyUpdateV2(dst, enc, nil)
		if got := dst.GetText("t").ToString(); got != "remote" {
			t.Fatalf("remote text = %q, want %q", got, "remote")
		}
	})

	t.Run("observe then unobserve then mutate", func(t *testing.T) {
		doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
		txt := doc.GetText("t")
		fired := 0
		cb := func(interface{}, interface{}) { fired++ }

		txt.Observe(cb)
		txt.Insert(0, "a", Object{})
		if fired == 0 {
			t.Fatal("observer never fired; the fixture is not exercising dispatch")
		}
		before := fired

		txt.Unobserve(cb)
		txt.Insert(0, "b", Object{})
		if fired != before {
			t.Fatalf("observer fired %d times after Unobserve", fired-before)
		}

		// Removing again, and removing all from a now-empty handler, must both be inert.
		txt.Unobserve(cb)
		removeAllEventHandlerListeners(txt.getEventHandler())
		txt.Insert(0, "c", Object{})
	})

	t.Run("handlers stay unallocated until observed", func(t *testing.T) {
		doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
		txt := doc.GetText("t")
		txt.Insert(0, "x", Object{})

		// Read the FIELDS, not GetEH()/GetDEH(). The accessors deliberately materialise a handler
		// on demand so the public API never returns nil -- calling them here would create the very
		// allocation this asserts is absent. hasTypeObservers exists precisely to consult the
		// handlers without materialising them, which is the internal contract being pinned.
		if txt.eventHandler != nil {
			t.Error("EH allocated on a type that was never observed")
		}
		if txt.deepEventHandler != nil {
			t.Error("DEH allocated on a type that was never observed")
		}
		if hasTypeObservers(txt) {
			t.Error("hasTypeObservers reported observers on an unobserved type")
		}
		if txt.eventHandler != nil || txt.deepEventHandler != nil {
			t.Error("hasTypeObservers materialised a handler; it must only read")
		}

		txt.Observe(func(interface{}, interface{}) {})
		if txt.eventHandler == nil {
			t.Error("EH still unallocated after Observe")
		}
		if !hasTypeObservers(txt) {
			t.Error("hasTypeObservers missed a registered observer")
		}
	})
}
