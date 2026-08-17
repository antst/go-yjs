package crdt

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
)

// ---------------------------------------------------------------- from object_deep_copy_test.go
// cloneDataObject is now the library's only deep copy of an Object, after
// Object.Clone (reflection, via copystructure) was removed rather than
// reimplemented. Its allocation budget was already covered by
// TestAwarenessDataCloneAllocationBudget, but nothing asserted the property the
// walker exists for: that the result shares no mutable substructure.
//
// The test mutates EVERY mutable position in the original after copying and
// requires the copy to be unmoved. A shared []byte backing array, a shared nested
// Object store, a shared slice or map shows up here as a mutation that leaked.
func TestCloneDataObjectSharesNoMutableSubstructure(t *testing.T) {
	nested := newObject()
	nested.Set("bytes", []byte{1, 2, 3})
	nested.Set("scalar", 7)

	inner := newObject()
	inner.Set("deep", "original")

	original := newObject()
	original.Set("bytes", []byte{10, 20})
	original.Set("object", nested)
	original.Set("array", []any{[]byte{30}, inner, "str"})
	original.Set("map", map[string]any{"k": []byte{40}})

	clone, err := cloneDataObject(original)
	if err != nil {
		t.Fatal(err)
	}

	original.GetOr("bytes").([]byte)[0] = 99
	nested.GetOr("bytes").([]byte)[0] = 99
	nested.Set("scalar", 99)
	arr := original.GetOr("array").([]any)
	arr[0].([]byte)[0] = 99
	arr[2] = "mutated"
	inner.Set("deep", "mutated")
	original.GetOr("map").(map[string]any)["k"].([]byte)[0] = 99

	if got := clone.GetOr("bytes").([]byte)[0]; got != 10 {
		t.Fatalf("top-level []byte aliased: got %d want 10", got)
	}
	clonedNested := clone.GetOr("object").(Object)
	if got := clonedNested.GetOr("bytes").([]byte)[0]; got != 1 {
		t.Fatalf("nested Object's []byte aliased: got %d want 1", got)
	}
	if got := clonedNested.GetOr("scalar"); got != 7 {
		t.Fatalf("nested Object store aliased: got %v want 7", got)
	}
	clonedArr := clone.GetOr("array").([]any)
	if got := clonedArr[0].([]byte)[0]; got != 30 {
		t.Fatalf("[]byte inside the array aliased: got %d want 30", got)
	}
	if got := clonedArr[2]; got != "str" {
		t.Fatalf("array backing store aliased: got %v want \"str\"", got)
	}
	if got := clonedArr[1].(Object).GetOr("deep"); got != "original" {
		t.Fatalf("Object inside the array aliased: got %v want \"original\"", got)
	}
	if got := clone.GetOr("map").(map[string]any)["k"].([]byte)[0]; got != 40 {
		t.Fatalf("map[string]any value aliased: got %d want 40", got)
	}
}

// Rejecting an unrecognized value is the whole reason this walker replaced the
// reflection-based one. copystructure fell back to sharing the original on a
// value it could not handle, which returns something that LOOKS owned and is not
// — silently defeating the ownership boundary the copy was made for. An error
// here is the contract; a shared reference would be the bug.
func TestCloneDataObjectRejectsUnsupportedValues(t *testing.T) {
	seven := 7
	original := newObject()
	original.Set("exotic", &seven)

	if _, err := cloneDataObject(original); !errors.Is(err, ErrUnsupportedDataValue) {
		t.Fatalf("unsupported value must be rejected, got err=%v", err)
	}

	// Nested inside a container, too — the walk must not lose strictness on the
	// way down, which is exactly where a per-value fallback would hide.
	deep := newObject()
	deep.Set("arr", []any{MakeObject("k", &seven)})
	if _, err := cloneDataObject(deep); !errors.Is(err, ErrUnsupportedDataValue) {
		t.Fatalf("unsupported value nested in an array must be rejected, got err=%v", err)
	}
}

// The depth bound is what stops a locally-built cycle from recursing until the
// stack dies. Values decoded from the wire cannot reach it — the binary decoder
// bounds them at the same maxAnyDepth — so this is reachable only from a
// structure that could never be encoded in the first place.
func TestCloneDataObjectRejectsACycle(t *testing.T) {
	cyclic := newObject()
	cyclic.Set("self", cyclic)

	if _, err := cloneDataObject(cyclic); !errors.Is(err, ErrUnsupportedDataValue) {
		t.Fatalf("a cycle must be rejected at the depth bound, got err=%v", err)
	}

	// A cycle that never passes through an Object. cloneDataObjectDepth's own
	// bound cannot see these — only the one in cloneDataValue can — so without a
	// case per container shape the two guards look interchangeable and one can be
	// deleted as redundant while these inputs still overflow the stack.
	selfSlice := make([]any, 1)
	selfSlice[0] = selfSlice
	viaSlice := newObject()
	viaSlice.Set("arr", selfSlice)
	if _, err := cloneDataObject(viaSlice); !errors.Is(err, ErrUnsupportedDataValue) {
		t.Fatalf("a cycle through a slice must be rejected, got err=%v", err)
	}

	selfMap := map[string]any{}
	selfMap["self"] = selfMap
	viaMap := newObject()
	viaMap.Set("m", selfMap)
	if _, err := cloneDataObject(viaMap); !errors.Is(err, ErrUnsupportedDataValue) {
		t.Fatalf("a cycle through a map must be rejected, got err=%v", err)
	}
}

// ---------------------------------------------------------------- from object_order_guard_test.go
func TestObjectAppendNewPreservesOrderedSetSemantics(t *testing.T) {
	o := newObjectWithCapacity(4)
	o.appendNew("z", 1)
	o.appendNew("a", 2)
	o.appendNew("m", 3)

	if got, want := o.Keys(), []string{"z", "a", "m"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("appendNew order = %v, want %v", got, want)
	}
	if got, ok := o.Get("a"); !ok || got != 2 {
		t.Fatalf("appendNew value = (%v, %v), want (2, true)", got, ok)
	}

	// Once constructed, the Object retains normal public update semantics:
	// replacing a key does not move it in the insertion order.
	o.Set("a", 4)
	if got, want := o.Keys(), []string{"z", "a", "m"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Set after appendNew moved existing key: got %v, want %v", got, want)
	}
	if got, _ := o.Get("a"); got != 4 {
		t.Fatalf("Set after appendNew = %v, want 4", got)
	}
}

// US9 / FR-031 (work item 1.5). Deterministic emission order for multi-key /
// multi-client documents is already correct (every yjs-ordered encode path sorts:
// writeClientsStructs / writeStateVector descending, WriteDeleteSet descending,
// integrateStructs ascending). This is a REGRESSION GUARD, not a fix: it rebuilds
// the doc many times — each build re-randomizes Go's map iteration order — and
// asserts the encoded update (V1+V2) and state vector are byte-identical every
// time. Cross-impl byte-parity vs yjs for map encoding is covered separately by
// compatibility_v2_test.go's map fixtures.
func TestDeterministicMultiKeyMultiClientOrdering(t *testing.T) {
	build := func() (v1, v2, sv []byte) {
		d1 := newDoc("guid", false, nil, nil, false, WithClientID(1))
		m1 := d1.GetMap("m")
		m1.Set("k1", "v1")
		m1.Set("k3", "v3")
		m1.Set("k5", "v5")

		d99 := newDoc("guid", false, nil, nil, false, WithClientID(99))
		m99 := d99.GetMap("m")
		m99.Set("k2", "v2")
		m99.Set("k4", "v4")

		u1, err := EncodeStateAsUpdateV2(d1, nil)
		if err != nil {
			t.Fatalf("encode d1: %v", err)
		}
		u99, err := EncodeStateAsUpdateV2(d99, nil)
		if err != nil {
			t.Fatalf("encode d99: %v", err)
		}

		merged := newDoc("guid", false, nil, nil, false, WithClientID(7))
		_ = ApplyUpdateV2(merged, u1, nil)
		_ = ApplyUpdateV2(merged, u99, nil)

		v2b, err := EncodeStateAsUpdateV2(merged, nil)
		if err != nil {
			t.Fatalf("encode merged v2: %v", err)
		}
		v1b, err := EncodeStateAsUpdate(merged, nil)
		if err != nil {
			t.Fatalf("encode merged v1: %v", err)
		}
		svb := encodeStateVectorWith(merged, nil, newUpdateEncoderV1())
		return v1b, v2b, svb
	}

	v1First, v2First, svFirst := build()
	const runs = 100
	for i := 0; i < runs; i++ {
		v1, v2, sv := build()
		if !bytes.Equal(v1, v1First) {
			t.Fatalf("run %d: V1 update bytes differ — nondeterministic emission order", i)
		}
		if !bytes.Equal(v2, v2First) {
			t.Fatalf("run %d: V2 update bytes differ — nondeterministic emission order", i)
		}
		if !bytes.Equal(sv, svFirst) {
			t.Fatalf("run %d: state-vector bytes differ — nondeterministic emission order", i)
		}
	}
}

// ---------------------------------------------------------------- from object_order_regression_test.go
// Regression tests for INSERTION-ORDERED object encoding (byte-parity with
// lib0/Yjs for multi-key objects).
//
// Before the ordered-Object change, Object was `type Object = map[string]any`:
// WriteObject/WriteAny emitted keys in Go-map-randomized order and
// ContentJson.Write / V1 WriteJson used json.Marshal (SORTED keys), so a Go-
// encoded multi-key object was NOT byte-identical to the JS one (which uses JS
// insertion order). These tests lock in the fix end to end.

// findObjBytes returns the bytes of the first lib0-any object (tag 118) in u,
// starting at the tag and spanning n bytes, or nil if not found.
func findObjBytes(u []byte, n int) []byte {
	for i := 0; i < len(u)-1; i++ {
		if u[i] == 118 { // lib0 any tag: object<string,any>
			end := i + n
			if end > len(u) {
				end = len(u)
			}
			return u[i:end]
		}
	}
	return nil
}

// TestObjectInsertionOrderRoundTripV2 builds a Y.Map value {z:1,a:2,m:3} (keys
// in a deliberately NON-sorted order), encodes V2, decodes into a fresh doc and
// re-encodes: the bytes must be IDENTICAL, the decoded key order must be the
// insertion order z,a,m (not the sorted a,m,z), and the on-wire object must list
// keys z,a,m in that order.
func TestObjectInsertionOrderRoundTripV2(t *testing.T) {
	d := newDoc("guid", true, defaultGCFilter, nil, false, WithClientID(12345))
	d.Transact(func(tr *Transaction) {
		m := d.GetMap("m")
		m.Set("obj", MakeObject("z", 1, "a", 2, "m", 3))
	}, nil)
	v2 := mustBytes(EncodeStateAsUpdateV2(d, nil))

	d2 := newDoc("guid", true, defaultGCFilter, nil, false)
	_ = ApplyUpdateV2(d2, v2, nil)
	v2b := mustBytes(EncodeStateAsUpdateV2(d2, nil))

	if !bytes.Equal(v2, v2b) {
		t.Fatalf("V2 re-encode not byte-identical after round-trip\n  first=%v\n  again=%v", v2, v2b)
	}

	got, ok := d2.GetMap("m").Get("obj").(Object)
	if !ok {
		t.Fatalf("decoded value is not an Object: %T", d2.GetMap("m").Get("obj"))
	}
	keys := got.Keys()
	if len(keys) != 3 || keys[0] != "z" || keys[1] != "a" || keys[2] != "m" {
		t.Fatalf("decoded key order = %v, want [z a m] (insertion order, NOT sorted)", keys)
	}

	// On-wire: object tag 118, size 3, then key "z"(122)=1, "a"(97)=2, "m"(109)=3.
	// 119 is the string tag; keys are written as raw varstrings (no tag), and
	// values as any-encoded varints (125, n). The byte after 118 is the count (3),
	// then for each pair: keylen(1), keybyte, value-tag(125), value-varint.
	want := []byte{118, 3, 1, 'z', 125, 1, 1, 'a', 125, 2, 1, 'm', 125, 3}
	objBytes := findObjBytes(v2, len(want))
	if objBytes == nil {
		t.Fatalf("no lib0 object (tag 118) found in V2 output: %v", v2)
	}
	if !bytes.Equal(objBytes, want) {
		t.Fatalf("on-wire object key order wrong\n  got  %v\n  want %v (keys z,a,m in insertion order)", objBytes, want)
	}
}

// TestObjectInsertionOrderRoundTripV1 is the V1 analogue: V1 encodes the same
// Y.Map object value via lib0 any (ContentAny), so the round-trip must likewise
// be byte-identical with keys in insertion order.
func TestObjectInsertionOrderRoundTripV1(t *testing.T) {
	d := newDoc("guid", true, defaultGCFilter, nil, false, WithClientID(54321))
	d.Transact(func(tr *Transaction) {
		m := d.GetMap("m")
		m.Set("obj", MakeObject("z", 1, "a", 2, "m", 3))
	}, nil)
	v1 := mustBytes(EncodeStateAsUpdate(d, nil))

	d2 := newDoc("guid", true, defaultGCFilter, nil, false)
	_ = ApplyUpdate(d2, v1, nil)
	v1b := mustBytes(EncodeStateAsUpdate(d2, nil))

	if !bytes.Equal(v1, v1b) {
		t.Fatalf("V1 re-encode not byte-identical after round-trip\n  first=%v\n  again=%v", v1, v1b)
	}

	want := []byte{118, 3, 1, 'z', 125, 1, 1, 'a', 125, 2, 1, 'm', 125, 3}
	objBytes := findObjBytes(v1, len(want))
	if objBytes == nil || !bytes.Equal(objBytes, want) {
		t.Fatalf("V1 on-wire object key order wrong\n  got  %v\n  want %v", objBytes, want)
	}
}

// TestContentJsonInsertionOrder locks in ContentJson.Write (the JSON.stringify
// path): a multi-key object embedded in Y.Text via insertEmbed is serialized to
// JSON with keys in insertion order, NOT json.Marshal's sorted order. We assert
// the JSON substring "{\"z\":1,\"a\":2,\"m\":3}" (insertion order) appears in the
// V1 update, and that a round-trip is byte-identical.
func TestContentJsonInsertionOrder(t *testing.T) {
	d := newDoc("guid", true, defaultGCFilter, nil, false, WithClientID(99999))
	d.Transact(func(tr *Transaction) {
		txt := d.GetText("t")
		txt.Insert(0, "x", Object{})
		// insertEmbed stores its embed as ContentJson (JSON-serialized).
		txt.InsertEmbed(1, MakeObject("z", 1, "a", 2, "m", 3), Object{})
	}, nil)
	v1 := mustBytes(EncodeStateAsUpdate(d, nil))

	if !bytes.Contains(v1, []byte(`{"z":1,"a":2,"m":3}`)) {
		t.Fatalf("ContentJson did not serialize keys in insertion order z,a,m; update=%q", v1)
	}
	if bytes.Contains(v1, []byte(`{"a":2,"m":3,"z":1}`)) {
		t.Fatalf("ContentJson serialized keys in SORTED order (json.Marshal regression)")
	}

	d2 := newDoc("guid", true, defaultGCFilter, nil, false)
	_ = ApplyUpdate(d2, v1, nil)
	v1b := mustBytes(EncodeStateAsUpdate(d2, nil))
	if !bytes.Equal(v1, v1b) {
		t.Fatalf("ContentJson V1 re-encode not byte-identical after round-trip")
	}
}

// TestMarshalJSONOrderedKeyOrder is a unit check on the JSON encoder: object keys
// serialize in insertion order, and a nested null becomes JSON null.
func TestMarshalJSONOrderedKeyOrder(t *testing.T) {
	o := MakeObject("z", 1, "a", "two", "m", Null)
	b, err := marshalJSONOrdered(o)
	if err != nil {
		t.Fatalf("marshalJSONOrdered: %v", err)
	}
	if string(b) != `{"z":1,"a":"two","m":null}` {
		t.Fatalf("marshalJSONOrdered key order wrong: got %s, want {\"z\":1,\"a\":\"two\",\"m\":null}", b)
	}

	// Decode-then-encode round-trip preserves order.
	v, err := unmarshalJSONOrdered([]byte(`{"q":true,"b":2,"k":null}`))
	if err != nil {
		t.Fatalf("unmarshalJSONOrdered: %v", err)
	}
	ro, ok := v.(Object)
	if !ok {
		t.Fatalf("unmarshalJSONOrdered returned %T, want Object", v)
	}
	if keys := ro.Keys(); len(keys) != 3 || keys[0] != "q" || keys[1] != "b" || keys[2] != "k" {
		t.Fatalf("unmarshalJSONOrdered key order = %v, want [q b k]", ro.Keys())
	}
	rb, err := marshalJSONOrdered(ro)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if string(rb) != `{"q":true,"b":2,"k":null}` {
		t.Fatalf("JSON round-trip not order-preserving: got %s", rb)
	}
}
