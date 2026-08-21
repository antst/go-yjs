//go:build !structstoreoracle

package crdt

import (
	"fmt"
	"strings"
	"testing"
)

func buildActivationTextDoc(tb testing.TB, client Number, deletes int) *Doc {
	tb.Helper()
	doc := newDoc("activation-policy", false, defaultGCFilter, nil, false, WithClientID(client))
	text := doc.GetText("t")
	text.Insert(0, strings.Repeat("x", deletes*5), Object{})
	rng := perfRand()
	for i := 0; i < deletes; i++ {
		length := text.Length()
		if length < 2 {
			tb.Fatal("activation fixture exhausted its text")
		}
		text.Delete(rng.intn(length-1), 1)
	}
	return doc
}

func requireClientTreeState(tb testing.TB, doc *Doc, client Number, wantActive bool) int {
	tb.Helper()
	list, ok := doc.store.clientStructs(client)
	if !ok {
		tb.Fatalf("missing client %d struct list", client)
	}
	active := list.tree.active() != nil
	if active != wantActive {
		tb.Fatalf("client %d structs=%d active=%v, want active=%v", client, list.Len(), active, wantActive)
	}
	return list.Len()
}

func BenchmarkStructTreeActivationRandomDelete(b *testing.B) {
	for _, deletes := range []int{2_000, 4_000, 8_000, 16_000, 32_000} {
		b.Run(fmt.Sprintf("delete-%d", deletes), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				doc := newDoc("activation-policy", false, defaultGCFilter, nil, false, WithClientID(1))
				text := doc.GetText("t")
				text.Insert(0, strings.Repeat("x", deletes*5), Object{})
				rng := perfRand()
				b.StartTimer()
				for operation := 0; operation < deletes; operation++ {
					text.Delete(rng.intn(text.Length()-1), 1)
				}
				b.StopTimer()
				list, ok := doc.store.clientStructs(doc.ClientID)
				if !ok {
					b.Fatal("missing locally-created client struct list")
				}
				wantActive := list.Len() > clientStructTreeActivationLimit
				if active := list.tree.active() != nil; active != wantActive {
					b.Fatalf("deletes=%d structs=%d active=%v, threshold=%d", deletes, list.Len(), active, clientStructTreeActivationLimit)
				}
				b.ReportMetric(float64(list.Len()), "structs")
			}
		})
	}
}

func BenchmarkStructTreeActivationReconnect(b *testing.B) {
	for _, deletes := range []int{12_000, 28_000} {
		b.Run(fmt.Sprintf("tomb-%d", deletes), func(b *testing.B) {
			activeTarget := buildActivationTextDoc(b, 1, deletes)
			structs := requireClientTreeState(b, activeTarget, 1, true)
			base, err := EncodeStateAsUpdate(activeTarget, nil)
			if err != nil {
				b.Fatal(err)
			}
			stateVector := encodeStateVectorWith(activeTarget, nil, newUpdateEncoderV1())

			peer := newDoc("activation-policy", false, defaultGCFilter, nil, false, WithClientID(7002))
			if err := ApplyUpdate(peer, base, nil); err != nil {
				b.Fatal(err)
			}
			peer.GetText("t").Insert(0, "late", Object{})
			catchup, err := EncodeStateAsUpdate(peer, stateVector)
			if err != nil {
				b.Fatal(err)
			}

			flatTarget := newDoc("activation-policy", false, defaultGCFilter, nil, false, WithClientID(999))
			if err := ApplyUpdate(flatTarget, base, nil); err != nil {
				b.Fatal(err)
			}
			if got := requireClientTreeState(b, flatTarget, 1, false); got != structs {
				b.Fatalf("flat structs=%d, active structs=%d", got, structs)
			}

			b.Run("flat", func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					benchSinkErr = ApplyUpdate(flatTarget, catchup, nil)
				}
			})
			b.Run("active", func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					benchSinkErr = ApplyUpdate(activeTarget, catchup, nil)
				}
			})
		})
	}
}
