//go:build structstoreoracle

package crdt

import (
	"bytes"
	"go/ast"
	"testing"
	"unsafe"
)

// TestStructTreeActiveCapableFlatCursorDispatchesOncePerOperation pins the
// source-level shape behind BenchmarkClientStructCursorWalk's disabled/tagged
// comparison. Every operation loads the optional tree once and then stays on
// either the tree or flat path. Calling Valid or treeCursor here would repeat
// that dispatch for every struct even while the list remains flat.
func TestStructTreeActiveCapableFlatCursorDispatchesOncePerOperation(t *testing.T) {
	// Located by the symbol it audits rather than by filename: guards that named
	// a file as a string broke three times during file reorganisation, each with
	// an error that said nothing about what to fix.
	_, file := parseProductionFileDeclaring(t, "clientStructCursor.Next")

	want := map[string]string{
		"Valid": "validTree", "Value": "treeValue", "Next": "nextTree", "Prev": "prevTree",
	}
	seen := make(map[string]bool, len(want))
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Recv == nil {
			continue
		}
		slowHelper, wanted := want[function.Name.Name]
		if !wanted {
			continue
		}
		receiver, ok := function.Recv.List[0].Type.(*ast.Ident)
		if !ok || receiver.Name != "clientStructCursor" {
			continue
		}
		seen[function.Name.Name] = true
		slowCalls := 0
		ast.Inspect(function.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch selector.Sel.Name {
			case "active":
				t.Errorf("%s calls active and repeats the cursor's representation dispatch", function.Name.Name)
			case "Valid":
				if function.Name.Name == "Valid" {
					return true
				}
				t.Errorf("%s calls %s and repeats hybrid dispatch", function.Name.Name, selector.Sel.Name)
			case "treeCursor":
				t.Errorf("%s calls %s and repeats hybrid dispatch", function.Name.Name, selector.Sel.Name)
			default:
				if selector.Sel.Name == slowHelper {
					slowCalls++
				}
			}
			return true
		})
		if slowCalls != 1 {
			t.Errorf("%s calls tree-only helper %s %d times, want exactly once", function.Name.Name, slowHelper, slowCalls)
		}
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("clientStructCursor.%s not found", name)
		}
	}
}

func TestStructTreeHybridCursorStaysCompact(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("layout guard is for 64-bit production targets")
	}
	if got := unsafe.Sizeof(clientStructCursor{}); got != 24 {
		t.Fatalf("enabled hybrid cursor=%d bytes, want the existing 24-byte flat cursor", got)
	}
}

func TestStructTreeHybridExactActivationThreshold(t *testing.T) {
	list := newClientStructList(clientStructTreeActivationLimit)
	values := make([]*abstractStructBase, clientStructTreeActivationLimit-1)
	for i := range values {
		values[i] = &abstractStructBase{id: GenID(23, Number(i*2)), length: 2}
		list.Append(values[i])
	}

	first, err := list.Find(0)
	if err != nil {
		t.Fatal(err)
	}
	values[0].length = 1
	list.InsertAfter(first, &abstractStructBase{id: GenID(23, 1), length: 1})
	if list.Len() != clientStructTreeActivationLimit || list.tree.active() != nil {
		t.Fatalf("below-threshold insertion len=%d tree=%p, want flat at exact limit", list.Len(), list.tree.active())
	}

	stale, err := list.Find(2)
	if err != nil {
		t.Fatal(err)
	}
	values[1].length = 1
	list.InsertAfter(stale, &abstractStructBase{id: GenID(23, 3), length: 1})
	if list.Len() != clientStructTreeActivationLimit+1 || list.tree.active() == nil {
		t.Fatalf("at-threshold insertion len=%d tree=%p, want active", list.Len(), list.tree.active())
	}
	if stale.Valid() {
		t.Fatal("activation left a pre-conversion cursor valid")
	}
}

func TestStructTreeHybridCursorSurvivesAppendAndLeafGrowth(t *testing.T) {
	list := newClientStructList(clientStructTreeActivationLimit)
	values := make([]*abstractStructBase, clientStructTreeActivationLimit)
	for i := range values {
		values[i] = &abstractStructBase{id: GenID(41, Number(i*2)), length: 2}
		list.Append(values[i])
	}

	first, err := list.Find(0)
	if err != nil {
		t.Fatal(err)
	}
	values[0].length = 1
	list.InsertAfter(first, &abstractStructBase{id: GenID(41, 1), length: 1})
	if list.tree.active() == nil {
		t.Fatal("fixture did not activate the tree")
	}
	retained, err := list.Find(0)
	if err != nil {
		t.Fatal(err)
	}
	leavesBefore := clientStructTreeLeafCount(list.tree.active())
	clock := Number(clientStructTreeActivationLimit * 2)
	for i := 0; i < clientStructTreeHybridLeafLimit*3; i++ {
		list.Append(&abstractStructBase{id: GenID(41, clock), length: 1})
		clock++
	}
	if leavesAfter := clientStructTreeLeafCount(list.tree.active()); leavesAfter <= leavesBefore {
		t.Fatalf("append leaves=%d, want growth beyond %d", leavesAfter, leavesBefore)
	}
	if !retained.Valid() || retained.Value() != values[0] {
		t.Fatal("append or rightmost leaf growth invalidated a position-stable cursor")
	}
}

func clientStructTreeLeafCount(tree *clientStructTree) int {
	count := 0
	for leaf := tree.first; leaf != nil; leaf = leaf.next {
		count++
	}
	return count
}

func TestStructTreeHybridLifecycleAndExactThresholds(t *testing.T) {
	resetClientStructTreeGateLifecycle()
	list := newClientStructList(0)
	oracle := &flatClientStructOracle{}
	originals := make([]abstractStruct, 40)
	for i := range originals {
		value := &abstractStructBase{id: GenID(17, Number(i*2)), length: 2}
		originals[i] = value
		list.Append(value)
		if err := oracle.insert(value); err != nil {
			t.Fatal(err)
		}
	}
	list.Reserve(128)
	if list.tree.active() != nil {
		t.Fatal("append and reserve activated the tree without a middle insertion")
	}

	for _, abstract := range originals[:24] {
		left := abstract.(*abstractStructBase)
		cursor, err := list.Find(left.id.Clock)
		if err != nil {
			t.Fatal(err)
		}
		left.length = 1
		right := &abstractStructBase{id: GenID(left.id.Client, left.id.Clock+1), length: 1}
		list.InsertAfter(cursor, right)
		if err := oracle.insert(right); err != nil {
			t.Fatal(err)
		}
	}
	if tree := list.tree.active(); tree == nil {
		t.Fatal("middle insertion at the activation threshold left the list flat")
	} else if tree.root == nil || tree.root.branch == nil || tree.root.branch.children[0].branch == nil {
		t.Fatal("fixture did not reach a multi-level tree")
	} else if err := tree.Validate(); err != nil {
		t.Fatalf("activated tree: %v", err)
	}
	requireClientStructListMatches(t, list, oracle)

	first, _ := list.First()
	keepFive, ok := list.cursorAtPosition(list.Len() - 6)
	if !ok {
		t.Fatal("missing removal boundary")
	}
	list.Remove(first, keepFive)
	oracle.items = append(oracle.items[:0], oracle.items[len(oracle.items)-5:]...)
	if list.Len() != 5 || list.tree.active() == nil {
		t.Fatalf("len=%d tree=%p, want five structs still active", list.Len(), list.tree.active())
	}
	if err := list.tree.active().Validate(); err != nil {
		t.Fatalf("rebalanced tree: %v", err)
	}
	requireClientStructListMatches(t, list, oracle)

	first, _ = list.First()
	staleTreeCursor := first
	list.Remove(first, first)
	oracle.items = oracle.items[1:]
	if list.Len() != clientStructTreeDeactivationLimit || list.tree.active() != nil {
		t.Fatalf("len=%d tree=%p, want exact-threshold deactivation", list.Len(), list.tree.active())
	}
	if staleTreeCursor.Valid() {
		t.Fatal("deactivation left a tree cursor valid")
	}
	requireClientStructListMatches(t, list, oracle)
	requireClientStructTreeGateLifecycle(t)
}

func TestCleanBoundarySplitIsTheTreeActivationPoint(t *testing.T) {
	doc := newDoc("hybrid-split", false, defaultGCFilter, nil, false)
	text := doc.GetText("t")
	list := newClientStructList(0)
	var split clientStructCursor
	doc.Transact(func(transaction *Transaction) {
		var left *itemStruct
		for i := 0; i < clientStructTreeActivationLimit; i++ {
			item := newItem(
				GenID(doc.ClientID, Number(i*2)), left, getItemLastID(left), nil, nil,
				text, "", newContentString("ab"),
			)
			if left != nil {
				left.right = item
			}
			left = item
			list.Append(item)
		}
		if list.tree.active() != nil {
			t.Fatal("append-built fixture activated before a split")
		}
		var err error
		split, err = findIndexCleanStart(transaction, list, 1)
		if err != nil {
			t.Fatal(err)
		}
	}, nil)
	if list.tree.active() == nil || split.Value().getID().Clock != 1 {
		t.Fatalf("clean-start split cursor=%v tree=%p", split.Valid(), list.tree.active())
	}
	if err := list.tree.active().Validate(); err != nil {
		t.Fatalf("tree after clean-start split: %v", err)
	}
}

func TestStructTreeHybridMaintainsSoleStringTailGrowth(t *testing.T) {
	doc := newDoc("hybrid-string-tail", false, defaultGCFilter, nil, false, WithClientID(29))
	array := doc.GetArray("fragment")
	values := make(ArrayAny, 32)
	for i := range values {
		values[i] = i
	}
	array.Insert(0, values)
	for index := 30; index >= 0; index -= 2 {
		array.Delete(Number(index), 1)
	}
	list, ok := doc.store.clientStructs(doc.ClientID)
	if !ok || list.tree.active() == nil {
		t.Fatal("fragmentation fixture did not activate the client tree")
	}

	text := doc.GetText("sole")
	text.Insert(0, "a", Object{})
	structsBefore := list.Len()
	text.Insert(1, "bc", Object{})
	if list.Len() != structsBefore {
		t.Fatalf("tail append created %d structs, want sole-string fast path to retain %d", list.Len(), structsBefore)
	}
	lastClock := text.start.id.Clock + text.start.length - 1
	cursor, err := list.Find(lastClock)
	if err != nil || cursor.Value() != text.start {
		t.Fatalf("Find(grown tail %d) cursor=%v err=%v", lastClock, cursor.Valid(), err)
	}
	if err := list.tree.active().Validate(); err != nil {
		t.Fatalf("tree after sole-string growth: %v", err)
	}
}

func TestStructTreeHybridWirePathsMatchFlatRepresentation(t *testing.T) {
	doc := newDoc("hybrid-wire", false, defaultGCFilter, nil, false, WithClientID(31))
	array := doc.GetArray("a")
	values := make(ArrayAny, 64)
	for i := range values {
		values[i] = i
	}
	array.Insert(0, values)
	for index := 62; index >= 0; index -= 2 {
		array.Delete(Number(index), 1)
	}
	text := doc.GetText("t")
	text.Insert(0, "tree-backed-wire", Object{})

	activeLists := 0
	doc.store.forEachClient(func(_ Number, list *clientStructList) bool {
		if list.tree.active() != nil {
			activeLists++
		}
		return true
	})
	if activeLists == 0 {
		t.Fatal("wire fixture did not activate a client tree")
	}

	partialClock := text.start.id.Clock + 3
	stateVector := encodeStateVectorWith(doc, map[Number]Number{doc.ClientID: partialClock}, newUpdateEncoderV1())
	type encodings struct {
		fullV1 []byte
		fullV2 []byte
		diffV1 []byte
		diffV2 []byte
		snapV1 []byte
		snapV2 []byte
	}
	encode := func() encodings {
		fullV1, err := EncodeStateAsUpdate(doc, nil)
		if err != nil {
			t.Fatal(err)
		}
		fullV2, err := EncodeStateAsUpdateV2(doc, nil)
		if err != nil {
			t.Fatal(err)
		}
		diffV1, err := EncodeStateAsUpdate(doc, stateVector)
		if err != nil {
			t.Fatal(err)
		}
		diffV2, err := EncodeStateAsUpdateV2(doc, stateVector)
		if err != nil {
			t.Fatal(err)
		}
		snapshot := NewSnapshotByDoc(doc)
		restored, err := CreateDocFromSnapshot(
			doc, snapshot, newDoc("hybrid-wire-restored", false, defaultGCFilter, nil, false, WithClientID(32)),
		)
		if err != nil {
			t.Fatal(err)
		}
		snapV1, err := EncodeStateAsUpdate(restored, nil)
		if err != nil {
			t.Fatal(err)
		}
		snapV2, err := EncodeStateAsUpdateV2(restored, nil)
		if err != nil {
			t.Fatal(err)
		}
		return encodings{fullV1, fullV2, diffV1, diffV2, snapV1, snapV2}
	}
	active := encode()

	doc.store.forEachClient(func(_ Number, list *clientStructList) bool {
		if tree := list.tree.active(); tree != nil {
			list.items = tree.Snapshot(list.items[:0])
			list.tree.set(nil)
			list.generation++
		}
		return true
	})
	flat := encode()
	for _, comparison := range []struct {
		name string
		got  []byte
		want []byte
	}{
		{name: "full-v1", got: active.fullV1, want: flat.fullV1},
		{name: "full-v2", got: active.fullV2, want: flat.fullV2},
		{name: "diff-v1", got: active.diffV1, want: flat.diffV1},
		{name: "diff-v2", got: active.diffV2, want: flat.diffV2},
		{name: "snapshot-v1", got: active.snapV1, want: flat.snapV1},
		{name: "snapshot-v2", got: active.snapV2, want: flat.snapV2},
	} {
		if !bytes.Equal(comparison.got, comparison.want) {
			t.Errorf("%s differs between tree and flat representations", comparison.name)
		}
	}
}
