//go:build structstoreoracle

package crdt

import (
	"fmt"
	"sort"
)

const clientStructListOracleEnabled = true

// clientStructListOracle is a test-only flat shadow. It accepts semantic struct
// identity only and independently derives every position from exact start
// clocks; no production cursor or ordinal can launder a placement error into it.
type clientStructListOracle struct {
	items []abstractStruct
}

func (o *clientStructListOracle) startIndex(clock Number) (int, bool) {
	index := sort.Search(len(o.items), func(i int) bool {
		return o.items[i].getID().Clock >= clock
	})
	return index, index < len(o.items) && o.items[index].getID().Clock == clock
}

func (o *clientStructListOracle) insert(value abstractStruct) {
	// Malformed updates can transiently append duplicate clocks before their
	// enclosing apply returns an error. Model that state deterministically rather
	// than making the test instrumentation introduce a panic the public API does
	// not have: equal clocks retain insertion order.
	index := sort.Search(len(o.items), func(i int) bool {
		return o.items[i].getID().Clock > value.getID().Clock
	})
	o.items = append(o.items, nil)
	copy(o.items[index+1:], o.items[index:])
	o.items[index] = value
}

func (o *clientStructListOracle) remove(values []abstractStruct) {
	for _, value := range values {
		index, found := o.startIndex(value.getID().Clock)
		for found && index < len(o.items) && o.items[index].getID().Clock == value.getID().Clock && o.items[index] != value {
			index++
		}
		if !found || index >= len(o.items) || o.items[index] != value {
			panic(fmt.Sprintf("struct-store oracle: remove missing struct %v", value.getID()))
		}
		copy(o.items[index:], o.items[index+1:])
		o.items[len(o.items)-1] = nil
		o.items = o.items[:len(o.items)-1]
	}
}

func (o *clientStructListOracle) removeRange(first, last abstractStruct) {
	firstIndex, firstFound := o.startIndex(first.getID().Clock)
	for firstFound && firstIndex < len(o.items) && o.items[firstIndex].getID().Clock == first.getID().Clock && o.items[firstIndex] != first {
		firstIndex++
	}
	lastIndex, lastFound := o.startIndex(last.getID().Clock)
	for lastFound && lastIndex < len(o.items) && o.items[lastIndex].getID().Clock == last.getID().Clock && o.items[lastIndex] != last {
		lastIndex++
	}
	if !firstFound || !lastFound || firstIndex >= len(o.items) || lastIndex >= len(o.items) ||
		o.items[firstIndex] != first || o.items[lastIndex] != last || firstIndex > lastIndex {
		panic(fmt.Sprintf("struct-store oracle: remove missing/reversed range %v..%v", first.getID(), last.getID()))
	}
	copy(o.items[firstIndex:], o.items[lastIndex+1:])
	removed := lastIndex - firstIndex + 1
	for i := len(o.items) - removed; i < len(o.items); i++ {
		o.items[i] = nil
	}
	o.items = o.items[:len(o.items)-removed]
}

func (o *clientStructListOracle) replace(old, replacement abstractStruct) {
	o.remove([]abstractStruct{old})
	o.insert(replacement)
}

func (o *clientStructListOracle) checkList(list *clientStructList) {
	if list.Len() != len(o.items) {
		panic(fmt.Sprintf("struct-store oracle: length=%d, want %d", list.Len(), len(o.items)))
	}
	position := 0
	list.forEachChunk(func(values []abstractStruct) bool {
		for _, actual := range values {
			if actual != o.items[position] {
				panic(fmt.Sprintf("struct-store oracle: position %d=%v, want %v", position, actual.getID(), o.items[position].getID()))
			}
			position++
		}
		return true
	})
	if position != len(o.items) {
		panic(fmt.Sprintf("struct-store oracle: walked=%d, want %d", position, len(o.items)))
	}
}

func (o *clientStructListOracle) checkFind(clock Number, actual abstractStruct, found bool) {
	wantFound := false
	actualValid := false
	var want abstractStruct
	for _, value := range o.items {
		start := value.getID().Clock
		if start <= clock && clock < start+value.structLength() {
			wantFound = true
			want = value
			if value == actual {
				actualValid = true
			}
		}
		if start > clock {
			break
		}
	}
	if found != wantFound || found && !actualValid {
		panic(fmt.Sprintf("struct-store oracle: Find(%d) found=%v value=%v, want found=%v value=%v",
			clock, found, abstractStructID(actual), wantFound, abstractStructID(want)))
	}
}

func abstractStructID(value abstractStruct) any {
	if value == nil {
		return nil
	}
	return *value.getID()
}
