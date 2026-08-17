package crdt

import (
	"bytes"
	"testing"
)

// Bulk string coalescing must be BYTE-EQUIVALENT to the merge it replaces.
//
// A run of N one-character inserts inside one transaction merges to a single Item with clock 0 and
// length N. Inserting the same N characters in ONE call produces a single Item with clock 0 and
// length N directly, never touching the coalescer. Same ID, same content, same length -- so the two
// documents must encode to identical bytes.
//
// That equivalence is the whole safety claim of the optimization, and unlike the batched-vs-per-op
// gate (where differing bytes are legitimate) this one IS byte-comparable. Sizes straddle the
// >=64-clock activation threshold so both the coalesced and the ordinary path are covered.
func TestCoalescedRunEncodesLikeSingleInsert(t *testing.T) {
	for _, n := range []int{8, 63, 64, 65, 200, 5000} {
		want := make([]byte, n)
		for i := range want {
			want[i] = byte('a' + i%26)
		}
		text := string(want)

		piecewise := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
		pt := piecewise.GetText("t")
		Transact(piecewise, func(*Transaction) {
			for i := 0; i < n; i++ {
				pt.Insert(pt.Length(), string(text[i]), Object{})
			}
		}, nil, true)

		single := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
		st := single.GetText("t")
		st.Insert(0, text, Object{})

		if got := pt.ToString(); got != text {
			t.Fatalf("n=%d: coalesced text wrong", n)
		}
		// One Item either way: the run must fully collapse.
		items := 0
		for it := pt.start; it != nil; it = it.right {
			items++
		}
		if items != 1 {
			t.Errorf("n=%d: coalesced run left %d items, want 1", n, items)
		}

		a, err := EncodeStateAsUpdateV2(piecewise, nil)
		if err != nil {
			t.Fatalf("n=%d encode piecewise: %v", n, err)
		}
		b, err := EncodeStateAsUpdateV2(single, nil)
		if err != nil {
			t.Fatalf("n=%d encode single: %v", n, err)
		}
		if !bytes.Equal(a, b) {
			t.Errorf("n=%d: coalesced encoding differs from single-insert encoding\n %d vs %d bytes",
				n, len(a), len(b))
		}

		a1, _ := EncodeStateAsUpdate(piecewise, nil)
		b1, _ := EncodeStateAsUpdate(single, nil)
		if !bytes.Equal(a1, b1) {
			t.Errorf("n=%d: V1 encodings differ", n)
		}
	}
}
