package crdt

import (
	"reflect"
	"sort"
	"testing"
)

// ---------------------------------------------------------------- from transaction_accessors_test.go
func TestTransactionPublicAccessorsPreserveOwnershipContracts(t *testing.T) {
	doc := newDoc("transaction-accessors", false, defaultGCFilter, nil, false, WithClientID(7))
	text := doc.GetText("text")
	origin := &struct{ name string }{"origin"}
	trans := newTransaction(doc, origin, true, true)

	trans.beforeState[1] = 2
	trans.afterState[1] = 3
	changed := newChangedSubs()
	changed.Add("key")
	trans.changedTypesInternal()[asAbstractType(text)] = changed
	trans.subdocsAdded.Add("added")
	trans.subdocsRemoved.Add("removed")
	trans.subdocsLoaded.Add("loaded")

	if trans.Document() != doc || trans.Origin() != origin || !trans.IsLocal() {
		t.Fatalf("scalar accessors returned doc=%p origin=%v local=%v", trans.Document(), trans.Origin(), trans.IsLocal())
	}

	before := trans.BeforeState()
	after := trans.AfterState()
	before[1] = 99
	after[1] = 99
	if trans.beforeState[1] != 2 || trans.afterState[1] != 3 {
		t.Fatal("state-vector accessors exposed transaction-owned maps")
	}

	projection := trans.ChangedTypes()
	projection[text].Add("caller")
	delete(projection, text)
	if !trans.changedTypes[asAbstractType(text)].Has("key") || trans.changedTypes[asAbstractType(text)].Has("caller") {
		t.Fatal("ChangedTypes exposed its map or a nested ChangedSubs set")
	}

	added := trans.SubdocsAdded()
	removed := trans.SubdocsRemoved()
	loaded := trans.SubdocsLoaded()
	added.Delete("added")
	removed.Delete("removed")
	loaded.Delete("loaded")
	if !trans.subdocsAdded.Has("added") || !trans.subdocsRemoved.Has("removed") || !trans.subdocsLoaded.Has("loaded") {
		t.Fatal("a subdocument accessor exposed a transaction-owned set")
	}

	meta := trans.Meta()
	meta["consumer"] = NewSet()
	meta["consumer"].Add("visible-to-later-observers")
	if !trans.meta["consumer"].Has("visible-to-later-observers") {
		t.Fatal("Meta returned a snapshot; transaction metadata must remain live and writable")
	}
}

// ---------------------------------------------------------------- from transaction_changed_journal_test.go
type changedMaterializationTrigger uint8

const (
	materializeDuringTransaction changedMaterializationTrigger = iota
	materializeFromObserver
	materializeForRemoteCleanup
	materializeAfterCommit
)

func canonicalChangedTypes(trans *Transaction, names map[abstractType]string) map[string][]string {
	result := make(map[string][]string)
	for changedType, subs := range trans.changedTypesInternal() {
		name := names[changedType]
		for sub := range subs {
			result[name] = append(result[name], sub)
		}
		sort.Strings(result[name])
	}
	return result
}

func runChangedMaterializationTrigger(t *testing.T, trigger changedMaterializationTrigger) map[string][]string {
	t.Helper()
	doc := newDoc("changed-materialization", false, nil, nil, false, WithClientID(1))
	left := doc.GetMap("left")
	right := doc.GetMap("right")
	text := doc.GetText("text")
	names := map[abstractType]string{left: "left", right: "right", text: "text"}

	var retained *Transaction
	var observed map[string][]string
	if trigger == materializeFromObserver {
		right.Observe(func(_ interface{}, transValue interface{}) {
			observed = canonicalChangedTypes(transValue.(*Transaction), names)
		})
	}
	mutate := func(trans *Transaction) {
		retained = trans
		left.Set("a", 1)
		text.Insert(0, "x", Object{})
		if trigger == materializeDuringTransaction {
			_ = trans.changedTypesInternal()
		}
		right.Set("b", 2)
		left.Set("a", 3) // duplicate key: materialization must deduplicate it.
		text.Insert(1, "y", Object{})
	}

	if trigger == materializeForRemoteCleanup {
		trans, initialCall := beginTransact(doc, nil, false, false)
		mutate(trans)
		finishTransact(doc, initialCall)
	} else {
		Transact(doc, mutate, nil, true)
	}

	if trigger == materializeFromObserver {
		if observed == nil {
			t.Fatal("observer did not materialize changed types")
		}
		return observed
	}
	return canonicalChangedTypes(retained, names)
}

func TestChangedTypesMaterializationIsTriggerIndependent(t *testing.T) {
	want := runChangedMaterializationTrigger(t, materializeDuringTransaction)
	for _, trigger := range []changedMaterializationTrigger{
		materializeFromObserver,
		materializeForRemoteCleanup,
		materializeAfterCommit,
	} {
		if got := runChangedMaterializationTrigger(t, trigger); !reflect.DeepEqual(got, want) {
			t.Fatalf("trigger %d changed map = %v, want %v", trigger, got, want)
		}
	}
}

func TestChangedTypesMaterializesDuringTransaction(t *testing.T) {
	doc := newDoc("changed-types-during", false, nil, nil, false, WithClientID(1))
	a := doc.GetMap("a")
	b := doc.GetMap("b")

	var retained *Transaction
	Transact(doc, func(trans *Transaction) {
		retained = trans
		a.Set("first", 1)

		changed := trans.changedTypesInternal()
		if !changed[a].Has("first") {
			t.Fatalf("first materialization = %v, want key first", changed[a])
		}
		// The returned map remains the live writable transaction state. This also
		// verifies that subsequent real changes do not replace it.
		changed[b] = ChangedSubs{"synthetic": {}}
		a.Set("second", 2)
		if !changed[a].Has("second") || !changed[b].Has("synthetic") {
			t.Fatalf("live changed map lost later/synthetic keys: a=%v b=%v", changed[a], changed[b])
		}
	}, nil, true)

	changed := retained.changedTypesInternal()
	if !changed[a].Has("first") || !changed[a].Has("second") || !changed[b].Has("synthetic") {
		t.Fatalf("retained transaction changed map = %v", changed)
	}
}

func TestChangedTypesMaterializesAfterTransaction(t *testing.T) {
	doc := newDoc("changed-types-after", false, nil, nil, false, WithClientID(1))
	a := doc.GetMap("a")
	b := doc.GetMap("b")

	var retained *Transaction
	Transact(doc, func(trans *Transaction) {
		retained = trans
		a.Set("one", 1)
		b.Set("two", 2)
		a.Set("three", 3) // Exercise non-consecutive groups for the same type.
	}, nil, true)
	if retained.changedTypes != nil {
		t.Fatal("unobserved transaction eagerly materialized changed types")
	}
	if len(retained.changedJournal) == 0 {
		t.Fatal("unobserved transaction did not retain its compact changed journal")
	}

	changed := retained.changedTypesInternal()
	if len(changed) != 2 || !changed[a].Has("one") || !changed[a].Has("three") || !changed[b].Has("two") {
		t.Fatalf("post-commit changed map = %v", changed)
	}
}

func TestChangedTypesObserverSeesExactKeysAfterLazyRecording(t *testing.T) {
	doc := newDoc("changed-types-observer", false, nil, nil, false, WithClientID(1))
	m := doc.GetMap("m")

	var observed ChangedSubs
	Transact(doc, func(_ *Transaction) {
		m.Set("before", 1)
		m.Observe(func(_ interface{}, transValue interface{}) {
			observed = transValue.(*Transaction).changedTypesInternal()[m]
		})
		m.Set("after", 2)
	}, nil, true)

	if observed == nil || len(observed) != 2 || !observed.Has("before") || !observed.Has("after") {
		t.Fatalf("observer changed keys = %v, want before+after", observed)
	}
}

func TestChangedTypesDropsNestedTypeDeletedInSameTransaction(t *testing.T) {
	doc := newDoc("changed-types-delete", false, nil, nil, false, WithClientID(1))
	root := doc.GetMap("root")
	child := NewYMap(nil)
	root.Set("child", child)

	var retained *Transaction
	Transact(doc, func(trans *Transaction) {
		retained = trans
		child.Set("nested", 1)
		root.Delete("child")
	}, nil, true)

	changed := retained.changedTypesInternal()
	if _, exists := changed[child]; exists {
		t.Fatalf("deleted nested type remained in changed map: %v", changed[child])
	}
	if !changed[root].Has("child") {
		t.Fatalf("parent deletion missing from changed map: %v", changed[root])
	}
}

// ---------------------------------------------------------------- from transaction_delete_pointer_test.go
func TestCompactDeleteSetPointerStoragePromotesWithoutLosingRanges(t *testing.T) {
	doc := newDoc("compact-delete-pointers", false, nil, nil, false)
	trans := newTransaction(doc, nil, true, false)
	client := doc.ClientID

	for i := 0; i < 3; i++ {
		trans.addToDeleteSet(client, i*2, 1)
	}

	deletes := trans.deleteSet.clients[client]
	if len(deletes) != 3 {
		t.Fatalf("delete ranges = %d, want 3", len(deletes))
	}
	for i, item := range deletes {
		if item.clock != i*2 || item.length != 1 {
			t.Fatalf("delete range %d = {%d,%d}, want {%d,1}", i, item.clock, item.length, i*2)
		}
	}
	if deletes[0] != &trans.deleteItemStorage[0] || deletes[1] != &trans.deleteItemStorage[1] {
		t.Fatal("promotion did not preserve the inline DeleteItems")
	}
	if &deletes[0] == &trans.deletePointerStorage[0] {
		t.Fatal("third range did not promote the pointer slice out of inline storage")
	}
}

func TestCompactDeleteSetPointerStorageHonorsReservation(t *testing.T) {
	doc := newDoc("reserved-delete-pointers", false, nil, nil, false)
	trans := newTransaction(doc, nil, true, false)
	client := doc.ClientID
	trans.reserveDeleteSetClient(client, 4)
	reserved := trans.deleteSet.clients[client][:cap(trans.deleteSet.clients[client])]

	trans.addToDeleteSet(client, 0, 1)
	deletes := trans.deleteSet.clients[client]
	if len(deletes) != 1 || &deletes[0] != &reserved[0] {
		t.Fatal("compact pointer storage replaced the explicitly reserved delete slice")
	}
}
