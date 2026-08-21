//go:build !structstoreoracle

package crdt

import (
	"go/ast"
	"testing"
	"unsafe"
)

func TestProductionStructTreePolicyUsesMeasuredThresholdAndCompactLayout(t *testing.T) {
	if clientStructTreeActivationLimit != 8192 || clientStructTreeDeactivationLimit != 4096 {
		t.Fatalf(
			"production activation/deactivation=%d/%d, want measured 8192/4096 policy",
			clientStructTreeActivationLimit, clientStructTreeDeactivationLimit,
		)
	}
	if clientStructTreeHybridLeafLimit != 64 || clientStructTreeHybridBranchLimit != 32 {
		t.Fatalf(
			"production leaf/branch=%d/%d, want measured 64/32 geometry",
			clientStructTreeHybridLeafLimit, clientStructTreeHybridBranchLimit,
		)
	}
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("layout guard is for 64-bit production targets")
	}
	if got := unsafe.Sizeof(clientStructList{}); got != 40 {
		t.Fatalf("enabled hybrid clientStructList=%d bytes, want existing 40-byte class", got)
	}
	if got := unsafe.Sizeof(clientStructCursor{}); got != 24 {
		t.Fatalf("enabled hybrid clientStructCursor=%d bytes, want existing 24-byte value", got)
	}
	if tree, items, generation :=
		unsafe.Offsetof((clientStructList{}).tree),
		unsafe.Offsetof((clientStructList{}).items),
		unsafe.Offsetof((clientStructList{}).generation); tree != 0 || items != 8 || generation != 32 {
		t.Fatalf("enabled hybrid list offsets tree/items/generation=%d/%d/%d", tree, items, generation)
	}
	if list, leaf, packed :=
		unsafe.Offsetof((clientStructCursor{}).list),
		unsafe.Offsetof((clientStructCursor{}).leaf),
		unsafe.Offsetof((clientStructCursor{}).packed); list != 0 || leaf != 8 || packed != 16 {
		t.Fatalf("enabled hybrid cursor offsets list/leaf/packed=%d/%d/%d", list, leaf, packed)
	}
}

func TestProductionStructTreeActivatesOnlyAfterExactThreshold(t *testing.T) {
	list := newClientStructList(clientStructTreeActivationLimit)
	values := make([]*abstractStructBase, clientStructTreeActivationLimit-1)
	for i := range values {
		values[i] = &abstractStructBase{id: GenID(23, i*2), length: 2}
		list.Append(values[i])
	}

	first, err := list.Find(0)
	if err != nil {
		t.Fatal(err)
	}
	values[0].length = 1
	list.InsertAfter(first, &abstractStructBase{id: GenID(23, 1), length: 1})
	if list.Len() != clientStructTreeActivationLimit || list.tree.active() != nil {
		t.Fatalf("below threshold len=%d tree=%p, want flat at exact limit", list.Len(), list.tree.active())
	}

	second, err := list.Find(2)
	if err != nil {
		t.Fatal(err)
	}
	values[1].length = 1
	list.InsertAfter(second, &abstractStructBase{id: GenID(23, 3), length: 1})
	if list.Len() != clientStructTreeActivationLimit+1 || list.tree.active() == nil {
		t.Fatalf("at threshold len=%d tree=%p, want active", list.Len(), list.tree.active())
	}
	if second.Valid() {
		t.Fatal("activation left a pre-conversion cursor valid")
	}
}

// Every hot flat path in the enabled production build must keep its direct
// compiler-visible shape. A hybrid helper on a path whose document never
// activates has caused the same regression three times: a lost inline boundary
// or repeated dispatch makes the abstraction cost more than the underlying
// work. Add new S2 hot paths to this table rather than creating one-off guards.
func TestProductionHotFlatPathsKeepDirectShape(t *testing.T) {
	type hotPath struct {
		function        string
		requiredFields  []string
		requiredCalls   []string
		forbiddenCalls  []string
		allowedSlowCall string
	}
	paths := []hotPath{
		{
			function:        "appendValue",
			requiredFields:  []string{"tree", "items"},
			requiredCalls:   []string{"append"},
			forbiddenCalls:  []string{"active", "Append", "cursorAt"},
			allowedSlowCall: "appendTreeValue",
		},
		{
			function:        "lastValue",
			requiredFields:  []string{"tree", "items"},
			forbiddenCalls:  []string{"active", "Last", "Value", "cursorAt"},
			allowedSlowCall: "lastTreeValue",
		},
		{
			function:        "refreshValue",
			requiredFields:  []string{"tree"},
			forbiddenCalls:  []string{"active", "clientStructs", "findValue"},
			allowedSlowCall: "refreshTreeValue",
		},
		{
			function:        "Next",
			requiredFields:  []string{"items", "packed"},
			forbiddenCalls:  []string{"active", "Valid", "treeCursor", "cursorAt", "flatCursorLen"},
			allowedSlowCall: "nextTree",
		},
		{
			function:      "applyDeleteRange",
			requiredCalls: []string{"active", "applyDeleteRangeFlat", "applyDeleteRangeTree"},
		},
		{
			function:       "applyDeleteRangeFlat",
			requiredFields: []string{"items"}, requiredCalls: []string{"findIndexSS"},
			forbiddenCalls: []string{"Find", "Value", "Next", "Valid"},
		},
		{
			function:       "appendClientStruct",
			requiredCalls:  []string{"appendValue"},
			forbiddenCalls: []string{"Append"},
		},
		{
			function:       "addStruct",
			requiredCalls:  []string{"appendValue", "lastValue"},
			forbiddenCalls: []string{"Append", "Last", "Value"},
		},
		{
			function:       "integrateNewMapKey",
			requiredCalls:  []string{"appendValue"},
			forbiddenCalls: []string{"Append"},
		},
		{
			function:       "integrateNewPrimitiveMapKey",
			requiredCalls:  []string{"appendValue"},
			forbiddenCalls: []string{"Append"},
		},
		{
			function:       "integratePrimitiveMapOverwrite",
			requiredCalls:  []string{"appendValue"},
			forbiddenCalls: []string{"Append"},
		},
		{
			function:       "setFreshPrimitiveKnown",
			requiredCalls:  []string{"clientStructs", "lastValue", "appendValue"},
			forbiddenCalls: []string{"GetState", "Append"},
		},
		{
			function:       "tryAppendSoleString",
			requiredCalls:  []string{"clientStructs", "lastValue", "refreshValue"},
			forbiddenCalls: []string{"GetState"},
		},
		{
			function:      "typeListInsertGenericsAfter",
			requiredCalls: []string{"lastValue", "refreshValue"},
		},
	}

	for _, path := range paths {
		t.Run(path.function, func(t *testing.T) {
			file := parseProductionFileDeclaring(t, path.function)
			var target *ast.FuncDecl
			for _, declaration := range file.Decls {
				function, ok := declaration.(*ast.FuncDecl)
				if ok && function.Name.Name == path.function {
					target = function
					break
				}
			}
			if target == nil {
				t.Fatalf("%s not found in the file that declares it", path.function)
			}

			fields := make(map[string]int)
			calls := make(map[string]int)
			ast.Inspect(target.Body, func(node ast.Node) bool {
				switch typed := node.(type) {
				case *ast.SelectorExpr:
					fields[typed.Sel.Name]++
				case *ast.CallExpr:
					switch called := typed.Fun.(type) {
					case *ast.Ident:
						calls[called.Name]++
					case *ast.SelectorExpr:
						calls[called.Sel.Name]++
					}
				}
				return true
			})
			for _, field := range path.requiredFields {
				if fields[field] == 0 {
					t.Errorf("%s has no direct %s field access", path.function, field)
				}
			}
			for _, call := range path.requiredCalls {
				if calls[call] != 1 {
					t.Errorf("%s calls %s %d times, want exactly once", path.function, call, calls[call])
				}
			}
			for _, call := range path.forbiddenCalls {
				if calls[call] != 0 {
					t.Errorf("%s routes its flat path through %s", path.function, call)
				}
			}
			if path.allowedSlowCall != "" && calls[path.allowedSlowCall] != 1 {
				t.Errorf(
					"%s tree-only helper %s calls=%d, want one outlined slow arm",
					path.function, path.allowedSlowCall, calls[path.allowedSlowCall],
				)
			}
		})
	}
}

type countedDeletedStruct struct {
	gcStruct
	deletedCalls int
}

func (s *countedDeletedStruct) isDeleted() bool {
	s.deletedCalls++
	if s.deletedCalls > 1 {
		panic("already-deleted covering struct was scanned after the no-op decision")
	}
	return true
}

func TestDeleteRangeStopsAtAlreadyDeletedCoveringStruct(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(map[bool]string{false: "flat", true: "active"}[active], func(t *testing.T) {
			value := &countedDeletedStruct{gcStruct: gcStruct{abstractStructBase: abstractStructBase{
				id: GenID(9, 0), length: 8,
			}}}
			list := newClientStructList(1)
			list.Append(value)
			if active {
				list.tree.set(newClientStructTreeFromFlat(
					list.items, clientStructTreeHybridLeafLimit, clientStructTreeHybridBranchLimit,
				))
				list.items = nil
			}
			if err := list.applyDeleteRange(nil, 2, 5); err != nil {
				t.Fatal(err)
			}
			if value.deletedCalls != 1 {
				t.Fatalf("Deleted calls=%d, want one no-op coverage decision", value.deletedCalls)
			}
		})
	}
}

func TestMutationValueFastPathsMatchFlatAndTreeRepresentations(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(map[bool]string{false: "flat", true: "active"}[active], func(t *testing.T) {
			list := newClientStructList(3)
			first := &abstractStructBase{id: GenID(17, 0), length: 1}
			second := &abstractStructBase{id: GenID(17, 1), length: 1}
			third := &abstractStructBase{id: GenID(17, 2), length: 1}
			list.appendValue(first)
			list.appendValue(second)
			if active {
				list.tree.set(newClientStructTreeFromFlat(
					list.items, clientStructTreeHybridLeafLimit, clientStructTreeHybridBranchLimit,
				))
				list.items = nil
			}

			list.appendValue(third)
			if got := list.lastValue(); got != third {
				t.Fatalf("last value=%p, want appended %p", got, third)
			}
			got := list.Snapshot(nil)
			if len(got) != 3 || got[0] != first || got[1] != second || got[2] != third {
				t.Fatalf("snapshot=%v, want append order preserved", got)
			}

			third.length = 2
			list.refreshValue(third)
			if active {
				if end := list.tree.tree.root.endClock; end != 4 {
					t.Fatalf("refreshed tree endClock=%d, want 4", end)
				}
				if err := list.tree.tree.Validate(); err != nil {
					t.Fatalf("refreshed tree: %v", err)
				}
			}
		})
	}
}
