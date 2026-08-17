package crdt

import (
	"testing"
	"unsafe"
)

// ---------------------------------------------------------------- from id_compact_test.go
func TestIDRemainsCompact(t *testing.T) {
	t.Parallel()

	want := 2 * unsafe.Sizeof(Number(0))
	if got := unsafe.Sizeof(ID{}); got != want {
		t.Fatalf("unsafe.Sizeof(ID{}) = %d, want %d", got, want)
	}
	if isAbstractType(&ID{}) {
		t.Fatal("ID unexpectedly implements IAbstractType")
	}
}

func TestNewItemAcceptsUnresolvedParentID(t *testing.T) {
	t.Parallel()

	parent := GenID(1, 2)
	item := newItem(GenID(3, 4), nil, nil, nil, nil, &parent, "", newContentString("x"))
	if item == nil {
		t.Fatal("NewItem returned nil")
	}
	if item.parent != &parent {
		t.Fatalf("NewItem parent = %v, want original ID pointer", item.parent)
	}
}

// ---------------------------------------------------------------- from id_parent_split_regression_test.go
func TestLazySlicePreservesUnresolvedParentID(t *testing.T) {
	parent := GenID(7, 9)
	item := newItem(GenID(3, 0), nil, nil, nil, nil, &parent, "", newContentString("abcdefghij"))

	sliced, ok := sliceStruct(item, 4).(*itemStruct)
	if !ok {
		t.Fatal("sliceStruct did not return an Item")
	}
	if sliced.parent != &parent {
		t.Fatalf("sliceStruct parent = %v, want original unresolved ID pointer", sliced.parent)
	}
}

// ---------------------------------------------------------------- from id_test.go
func TestGenID(t *testing.T) {
	id := GenID(1, 2)
	if id.Client != 1 {
		t.Errorf("id.Client = %d, want 1", id.Client)
	}

	if id.Clock != 2 {
		t.Errorf("id.Clock = %d, want 2", id.Clock)
	}
}

func TestCompareIDs(t *testing.T) {
	id1 := GenID(1, 2)
	id2 := GenID(1, 3)
	id3 := GenID(1, 2)
	if CompareIDs(&id1, &id2) {
		t.Error("CompareIDs(id1, id2) = true, want false")
	}

	if !CompareIDs(&id1, &id3) {
		t.Error("CompareIDs(id1, id3) = false, want true")
	}
}
