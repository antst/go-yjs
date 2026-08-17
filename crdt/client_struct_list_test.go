package crdt

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"
)

// flatClientStructOracle deliberately knows nothing about production cursors.
// Every mutation derives its position again from the struct's exact start clock,
// so a wrong cursor in clientStructList cannot be mirrored into this model.
type flatClientStructOracle struct {
	items []abstractStruct
}

func (o *flatClientStructOracle) insertionIndex(clock Number) (int, bool) {
	index := sort.Search(len(o.items), func(i int) bool {
		return o.items[i].getID().Clock >= clock
	})
	return index, index < len(o.items) && o.items[index].getID().Clock == clock
}

func (o *flatClientStructOracle) insert(value abstractStruct) error {
	index, duplicate := o.insertionIndex(value.getID().Clock)
	if duplicate {
		return fmt.Errorf("duplicate struct clock %d", value.getID().Clock)
	}
	o.items = append(o.items, nil)
	copy(o.items[index+1:], o.items[index:])
	o.items[index] = value
	return nil
}

func (o *flatClientStructOracle) remove(value abstractStruct) error {
	index, found := o.insertionIndex(value.getID().Clock)
	if !found || o.items[index] != value {
		return fmt.Errorf("remove missing struct %v", value.getID())
	}
	copy(o.items[index:], o.items[index+1:])
	o.items[len(o.items)-1] = nil
	o.items = o.items[:len(o.items)-1]
	return nil
}

func (o *flatClientStructOracle) replace(value abstractStruct) error {
	index, found := o.insertionIndex(value.getID().Clock)
	if !found {
		return fmt.Errorf("replace missing struct %v", value.getID())
	}
	o.items[index] = value
	return nil
}

func (o *flatClientStructOracle) find(clock Number) (abstractStruct, bool) {
	// Find the last exact start not greater than clock, independently of
	// FindIndexSS and its interpolation/binary-search implementation.
	index := sort.Search(len(o.items), func(i int) bool {
		return o.items[i].getID().Clock > clock
	}) - 1
	if index < 0 {
		return nil, false
	}
	value := o.items[index]
	start := value.getID().Clock
	return value, clock < start+value.structLength()
}

func compareClientStructList(list *clientStructList, oracle *flatClientStructOracle) error {
	got := list.Snapshot(nil)
	if len(got) != len(oracle.items) {
		return fmt.Errorf("length=%d, want %d", len(got), len(oracle.items))
	}
	for i := range got {
		if got[i] != oracle.items[i] {
			return fmt.Errorf("entry %d=%p (%v), want %p (%v)",
				i, got[i], got[i].getID(), oracle.items[i], oracle.items[i].getID())
		}
	}
	return nil
}

func requireClientStructListMatches(t *testing.T, list *clientStructList, oracle *flatClientStructOracle) {
	t.Helper()
	if err := compareClientStructList(list, oracle); err != nil {
		t.Fatal(err)
	}
	if list.Len() != len(oracle.items) {
		t.Fatalf("Len=%d, want %d", list.Len(), len(oracle.items))
	}

	walked := make([]abstractStruct, 0, list.Len())
	for cursor, ok := list.First(); ok; cursor, ok = cursor.Next() {
		walked = append(walked, cursor.Value())
	}
	if len(walked) != len(oracle.items) {
		t.Fatalf("forward cursor count=%d, want %d", len(walked), len(oracle.items))
	}
	for i := range walked {
		if walked[i] != oracle.items[i] {
			t.Fatalf("forward cursor %d=%p, want %p", i, walked[i], oracle.items[i])
		}
	}

	reverse := len(oracle.items) - 1
	for cursor, ok := list.Last(); ok; cursor, ok = cursor.Prev() {
		if cursor.Value() != oracle.items[reverse] {
			t.Fatalf("reverse cursor %d=%p, want %p", reverse, cursor.Value(), oracle.items[reverse])
		}
		reverse--
	}
	if reverse != -1 {
		t.Fatalf("reverse cursor stopped at %d", reverse)
	}
}

func TestClientStructListMatchesIndependentFlatOracleAfterEveryPrimitive(t *testing.T) {
	const (
		clients = 40
		steps   = 500
	)
	var exercised [7]int
	for seed := int64(0); seed < clients; seed++ {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			list := newClientStructList(1)
			oracle := &flatClientStructOracle{}
			first := &abstractStructBase{id: GenID(Number(seed+1), 0), length: 8}
			list.Append(first)
			if err := oracle.insert(first); err != nil {
				t.Fatal(err)
			}
			requireClientStructListMatches(t, list, oracle)

			for step := 0; step < steps; step++ {
				switch rng.Intn(len(exercised)) {
				case 0: // append at the semantic clock end
					last := oracle.items[len(oracle.items)-1]
					value := &abstractStructBase{
						id:     GenID(first.id.Client, last.getID().Clock+last.structLength()),
						length: Number(1 + rng.Intn(8)),
					}
					list.Append(value)
					if err := oracle.insert(value); err != nil {
						t.Fatal(err)
					}
					exercised[0]++

				case 1: // split one struct, inserting the right half after its cursor
					candidates := make([]abstractStruct, 0, len(oracle.items))
					for _, value := range oracle.items {
						if value.structLength() > 1 {
							candidates = append(candidates, value)
						}
					}
					if len(candidates) == 0 {
						continue
					}
					left := candidates[rng.Intn(len(candidates))]
					cursor, err := list.Find(left.getID().Clock)
					if err != nil {
						t.Fatal(err)
					}
					oldLength := left.structLength()
					offset := Number(1 + rng.Intn(int(oldLength-1)))
					left.setStructLength(offset)
					right := &abstractStructBase{
						id:     GenID(left.getID().Client, left.getID().Clock+offset),
						length: oldLength - offset,
					}
					list.InsertAfter(cursor, right)
					if err := oracle.insert(right); err != nil {
						t.Fatal(err)
					}
					exercised[1]++

				case 2: // merge neighbours and remove the absorbed right struct
					if len(oracle.items) < 2 {
						continue
					}
					index := rng.Intn(len(oracle.items) - 1)
					left, right := oracle.items[index], oracle.items[index+1]
					rightCursor, err := list.Find(right.getID().Clock)
					if err != nil {
						t.Fatal(err)
					}
					left.setStructLength(left.structLength() + right.structLength())
					list.Remove(rightCursor, rightCursor)
					if err := oracle.remove(right); err != nil {
						t.Fatal(err)
					}
					exercised[2]++

				case 3: // replace without changing semantic position
					old := oracle.items[rng.Intn(len(oracle.items))]
					cursor, err := list.Find(old.getID().Clock)
					if err != nil {
						t.Fatal(err)
					}
					replacement := &abstractStructBase{id: *old.getID(), length: old.structLength()}
					list.Replace(cursor, replacement)
					if err := oracle.replace(replacement); err != nil {
						t.Fatal(err)
					}
					exercised[3]++

				case 4: // capacity is representation-only
					list.Reserve(list.Len() + rng.Intn(64))
					exercised[4]++

				case 5: // compare point lookup against an independently derived answer
					last := oracle.items[len(oracle.items)-1]
					end := last.getID().Clock + last.structLength()
					clock := Number(rng.Intn(int(end)))
					want, found := oracle.find(clock)
					cursor, err := list.Find(clock)
					if found != (err == nil) {
						t.Fatalf("Find(%d) found=%v err=%v", clock, found, err)
					}
					if found && cursor.Value() != want {
						t.Fatalf("Find(%d)=%p, want %p", clock, cursor.Value(), want)
					}
					exercised[5]++

				case 6: // merge a run and remove its inclusive cursor range
					if len(oracle.items) < 3 {
						continue
					}
					leftIndex := rng.Intn(len(oracle.items) - 2)
					removeCount := 2 + rng.Intn(minNumber(4, len(oracle.items)-leftIndex-1)-1)
					left := oracle.items[leftIndex]
					removed := append([]abstractStruct(nil), oracle.items[leftIndex+1:leftIndex+1+removeCount]...)
					firstCursor, err := list.Find(removed[0].getID().Clock)
					if err != nil {
						t.Fatal(err)
					}
					lastCursor, err := list.Find(removed[len(removed)-1].getID().Clock)
					if err != nil {
						t.Fatal(err)
					}
					for _, value := range removed {
						left.setStructLength(left.structLength() + value.structLength())
					}
					list.Remove(firstCursor, lastCursor)
					for _, value := range removed {
						if err := oracle.remove(value); err != nil {
							t.Fatal(err)
						}
					}
					exercised[6]++
				}
				requireClientStructListMatches(t, list, oracle)
			}
		})
	}
	for operation, count := range exercised {
		if count < 100 {
			t.Fatalf("primitive %d exercised only %d times", operation, count)
		}
	}
}

func TestClientStructListOracleRejectsProductionCursorAsPositionAuthority(t *testing.T) {
	list := newClientStructList(4)
	oracle := &flatClientStructOracle{}
	for clock := Number(0); clock < 3; clock++ {
		value := &abstractStructBase{id: GenID(1, clock), length: 1}
		list.Append(value)
		if err := oracle.insert(value); err != nil {
			t.Fatal(err)
		}
	}
	wrong, _ := list.First()
	value := &abstractStructBase{id: GenID(1, 3), length: 1}
	if clientStructOperationPanicked(func() { list.InsertAfter(wrong, value) }) {
		return // the build-tagged store oracle rejected the wrong cursor immediately
	}
	if err := oracle.insert(value); err != nil { // independently derives position 3
		t.Fatal(err)
	}
	if err := compareClientStructList(list, oracle); err == nil {
		t.Fatal("independent oracle accepted a production cursor as insertion authority")
	}
}

func TestClientStructListSnapshotAndCursorBoundaries(t *testing.T) {
	list := newClientStructList(0)
	if list.Len() != 0 {
		t.Fatalf("empty Len=%d", list.Len())
	}
	if _, ok := list.First(); ok {
		t.Fatal("empty list has a first cursor")
	}
	if _, ok := list.Last(); ok {
		t.Fatal("empty list has a last cursor")
	}
	var zero clientStructCursor
	if next, ok := zero.Next(); ok || next.Valid() {
		t.Fatalf("zero cursor has next=%#v ok=%v", next, ok)
	}
	if previous, ok := zero.Prev(); ok || previous.Valid() {
		t.Fatalf("zero cursor has previous=%#v ok=%v", previous, ok)
	}

	first := &abstractStructBase{id: GenID(4, 0), length: 1}
	second := &abstractStructBase{id: GenID(4, 1), length: 1}
	list.Append(first)
	list.Append(second)
	snapshot := list.Snapshot(nil)
	snapshot[0] = second
	cursor, _ := list.First()
	if cursor.Value() != first {
		t.Fatal("mutating a flattened snapshot changed list storage")
	}
	if _, ok := cursor.Prev(); ok {
		t.Fatal("first cursor has a predecessor")
	}
	last, _ := list.Last()
	if _, ok := last.Next(); ok {
		t.Fatal("last cursor has a successor")
	}
	if next, ok := list.Remove(cursor, last); ok || next.Valid() || list.Len() != 0 {
		t.Fatalf("removing the full list left next=%#v ok=%v len=%d", next, ok, list.Len())
	}
}

func TestClientStructListInvalidatesCursorsOnlyWhenPositionsShift(t *testing.T) {
	list := newClientStructList(2)
	first := &abstractStructBase{id: GenID(6, 0), length: 1}
	last := &abstractStructBase{id: GenID(6, 2), length: 1}
	firstCursor := list.Append(first)
	staleLast := list.Append(last)

	middle := &abstractStructBase{id: GenID(6, 1), length: 1}
	middleCursor := list.InsertAfter(firstCursor, middle)
	if staleLast.Valid() {
		t.Fatal("insertion left a shifted cursor valid")
	}
	requireClientStructCursorPanic(t, func() { staleLast.Value() })

	// Appending and reserving storage preserve every existing ordinal. Replacing
	// a value changes the struct at that ordinal without moving the cursor.
	appended := &abstractStructBase{id: GenID(6, 3), length: 1}
	list.Append(appended)
	list.Reserve(128)
	replacement := &abstractStructBase{id: GenID(6, 1), length: 1}
	list.Replace(middleCursor, replacement)
	if !middleCursor.Valid() || middleCursor.Value() != replacement {
		t.Fatal("append, reserve, or replace invalidated a position-stable cursor")
	}

	currentFirst, _ := list.First()
	currentLast, _ := list.Last()
	list.Remove(currentFirst, currentFirst)
	if currentLast.Valid() {
		t.Fatal("removal left a cursor from the shifted generation valid")
	}
	requireClientStructCursorPanic(t, func() { currentLast.Value() })
}

func requireClientStructCursorPanic(t *testing.T, operation func()) {
	t.Helper()
	if !clientStructOperationPanicked(operation) {
		t.Fatal("stale cursor operation did not panic")
	}
}

func clientStructOperationPanicked(operation func()) (panicked bool) {
	defer func() { panicked = recover() != nil }()
	operation()
	return false
}

func TestClientStructListFindUsesContainingClockRange(t *testing.T) {
	list := newClientStructList(2)
	first := &abstractStructBase{id: GenID(7, 0), length: 2}
	second := &abstractStructBase{id: GenID(7, 2), length: 3}
	list.Append(first)
	list.Append(second)

	for _, test := range []struct {
		clock Number
		want  abstractStruct
	}{
		{clock: 0, want: first},
		{clock: 1, want: first},
		{clock: 2, want: second},
		{clock: 4, want: second},
	} {
		cursor, err := list.Find(test.clock)
		if err != nil {
			t.Fatalf("Find(%d): %v", test.clock, err)
		}
		if got := cursor.Value(); got != test.want {
			t.Fatalf("Find(%d)=%p, want %p", test.clock, got, test.want)
		}
	}

	for _, clock := range []Number{-1, 5} {
		if _, err := list.Find(clock); err == nil {
			t.Fatalf("Find(%d) succeeded outside the stored clock range", clock)
		}
	}
}

var clientStructCursorBenchmarkSink Number

func BenchmarkClientStructCursorWalk(b *testing.B) {
	const count = 32_000
	list := newClientStructList(count)
	for clock := Number(0); clock < count; clock++ {
		list.Append(&abstractStructBase{id: GenID(1, clock), length: 1})
	}
	if list.tree.active() != nil {
		b.Fatal("append-built flat-path fixture activated the tree")
	}
	b.Run("cursor", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			var sum Number
			for cursor, ok := list.First(); ok; cursor, ok = cursor.Next() {
				sum += cursor.Value().structLength()
			}
			clientStructCursorBenchmarkSink = sum
		}
	})
	b.Run("flat-control", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			var sum Number
			for _, value := range list.items {
				sum += value.structLength()
			}
			clientStructCursorBenchmarkSink = sum
		}
	})
}
