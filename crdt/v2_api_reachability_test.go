package crdt

import (
	"bytes"
	"testing"
)

// The reference exposes mergeUpdatesV2 / parseUpdateMetaV2 as named functions; Go instead
// parameterises the codec. This proves the V2 paths are genuinely reachable that way.
func TestV2ReachableViaFactories(t *testing.T) {
	mk := func(id Number, s string) []uint8 {
		d := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(id))
		d.GetText("t").Insert(0, s, Object{})
		u, err := EncodeStateAsUpdateV2(d, nil)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	a, b := mk(1, "hello"), mk(2, "world")

	merged, err := mergeUpdatesWith([][]uint8{a, b}, func(x []byte) updateDecoder { return newUpdateDecoderV2(x) },
		func() updateEncoder { return newDefaultUpdateEncoderV2() })
	if err != nil {
		t.Fatalf("MergeUpdates with V2 factories: %v", err)
	}
	sv, ds, err := parseUpdateMetaWith(merged, func(x []byte) updateDecoder { return newUpdateDecoderV2(x) })
	if err != nil {
		t.Fatalf("ParseUpdateMetaWith with a V2 decoder: %v", err)
	}
	if len(sv) != 2 {
		t.Errorf("merged V2 update should mention 2 clients, got %d (ds=%v)", len(sv), len(ds))
	}

	// And the merged V2 bytes must apply to give the same document as applying both separately.
	d1 := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(9))
	_ = ApplyUpdateV2(d1, merged, nil)
	d2 := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(9))
	_ = ApplyUpdateV2(d2, a, nil)
	_ = ApplyUpdateV2(d2, b, nil)
	s1, _ := EncodeStateAsUpdate(d1, nil)
	s2, _ := EncodeStateAsUpdate(d2, nil)
	if !bytes.Equal(s1, s2) {
		t.Error("merged-V2 document differs from sequentially-applied V2 documents")
	}
	if got := d1.GetText("t").ToString(); len(got) != 10 {
		t.Errorf("merged text = %q, want both inserts present", got)
	}
}
