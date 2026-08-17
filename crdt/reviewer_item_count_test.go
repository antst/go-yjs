package crdt

import (
	"fmt"
	"testing"
)

// Independent verification of the linked-item counter.
//
// The counter only SIZES caches, so drift cannot corrupt a document — it silently mis-sizes the
// marker and read-index caches, which is a performance defect that no correctness gate can see.
// That is precisely why it needs its own invariant test: nothing else in the suite will ever go red
// if this number is wrong.
//
// Two properties, checked separately because they fail differently:
//
//   1. No built-in type may take the pointer-walk fallback. listItemCount walks the whole list for
//      any IAbstractType that cannot implement the package-private counter. That fallback exists for
//      external implementations, but if one of OUR types missed the interface every marker lookup
//      would become O(n) — a severe regression that still passes every correctness test and every
//      counter-equality check, because the walk returns the RIGHT answer. Equality testing alone
//      cannot detect it; only asserting the fast path is taken can.
//
//   2. The counter must equal an actual pointer walk after EVERY mutation, not at quiescence. A
//      counter that drifts and self-corrects at a transaction boundary is still wrong in between,
//      and in between is when findMarker reads it.

func walkCount(t abstractType) Number {
	n := Number(0)
	for item := t.startItem(); item != nil; item = item.right {
		n++
	}
	return n
}

func assertCount(t *testing.T, ty abstractType, label string) {
	t.Helper()
	if got, want := listItemCount(ty), walkCount(ty); got != want {
		t.Fatalf("%s: counter=%d, actual pointer walk=%d (drift %+d)", label, got, want, got-want)
	}
}

// Property 1: every built-in list type must satisfy the counter interface.
func TestEveryBuiltinTypeUsesTheCounterFastPath(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	cases := []struct {
		name string
		ty   abstractType
	}{
		{"YArray", doc.GetArray("a")},
		{"YText", doc.GetText("t")},
		{"YMap", doc.GetMap("m")},
		{"YXmlFragment", doc.GetXmlFragment("x")},
		{"YXmlElement", NewYXmlElement("div")},
		{"YXmlText", NewYXmlText()},
	}
	for _, c := range cases {
		if _, ok := c.ty.(linkedItemCounter); !ok {
			t.Errorf("%s does NOT implement linkedItemCounter, so every marker lookup on it "+
				"falls back to an O(n) pointer walk — correct, and quietly quadratic", c.name)
		}
	}
}

// Property 2: no drift, checked after every single operation.
func TestItemCountNeverDriftsUnderMixedOps(t *testing.T) {
	for seed := 0; seed < 120; seed++ {
		gc := seed%2 == 0
		doc := newDoc("g", gc, defaultGCFilter, nil, false, WithClientID(1))
		arr := doc.GetArray("a")
		txt := doc.GetText("t")
		f := doc.GetXmlFragment("x")
		rng := markerLCG(uint32(seed*2654435761 + 101))
		um := newUndoManager(arr, 500, func(_ *itemStruct) bool { return true }, defaultTrackedOrigins())

		step := func(i int) {
			switch rng(11) {
			case 0:
				arr.Insert(Number(rng(arr.GetLength()+1)), ArrayAny{i})
			case 1:
				if arr.GetLength() > 1 {
					arr.Delete(Number(rng(arr.GetLength()-1)), 1)
				}
			case 2: // large single insert then a split: the density case that broke char-sizing
				txt.Insert(0, "0123456789012345678901234567890123456789", Object{})
			case 3:
				if txt.Length() > 4 {
					txt.Insert(Number(1+rng(txt.Length()-2)), "s", Object{})
				}
			case 4:
				if txt.Length() > 4 {
					txt.Delete(Number(rng(txt.Length()-2)), 2)
				}
			case 5:
				nested := NewYMap(nil)
				nested.Set("k", i)
				arr.Insert(Number(rng(arr.GetLength()+1)), ArrayAny{nested})
			case 6:
				f.Insert(Number(rng(f.GetLength()+1)), ArrayAny{NewYXmlElement("div")})
			case 7:
				if f.GetLength() > 1 {
					f.Delete(Number(rng(f.GetLength()-1)), 1)
				}
			case 8: // batched: merges happen at cleanup, inside one transaction
				Transact(doc, func(*Transaction) {
					for k := 0; k < 6; k++ {
						arr.Insert(arr.GetLength(), ArrayAny{900 + k})
						assertCount(t, arr, fmt.Sprintf("seed %d step %d in-transaction k=%d", seed, i, k))
					}
				}, nil, true)
			case 9:
				um.Undo()
			default:
				um.Redo()
			}
		}

		for i := 0; i < 45; i++ {
			step(i)
			assertCount(t, arr, fmt.Sprintf("seed %d step %d arr", seed, i))
			assertCount(t, txt, fmt.Sprintf("seed %d step %d txt", seed, i))
			assertCount(t, f, fmt.Sprintf("seed %d step %d xml", seed, i))
		}

		// Remote decode into a fresh document: items arrive through Integrate, never the public API.
		enc, err := EncodeStateAsUpdateV2(doc, nil)
		if err != nil {
			t.Fatalf("seed %d encode: %v", seed, err)
		}
		peer := newDoc("g", gc, defaultGCFilter, nil, false, WithClientID(2))
		_ = ApplyUpdateV2(peer, enc, nil)
		assertCount(t, peer.GetArray("a"), fmt.Sprintf("seed %d decoded arr", seed))
		assertCount(t, peer.GetText("t"), fmt.Sprintf("seed %d decoded txt", seed))
		assertCount(t, peer.GetXmlFragment("x"), fmt.Sprintf("seed %d decoded xml", seed))

		// Lazy root adoption: the root is materialised AFTER the update landed, so the counter must
		// be reconstructed rather than left at zero.
		late := newDoc("g", gc, defaultGCFilter, nil, false, WithClientID(3))
		_ = ApplyUpdateV2(late, enc, nil)
		assertCount(t, late.GetArray("a"), fmt.Sprintf("seed %d late-adopted arr", seed))
		assertCount(t, late.GetText("t"), fmt.Sprintf("seed %d late-adopted txt", seed))
	}
}
