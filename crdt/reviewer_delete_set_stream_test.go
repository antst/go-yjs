package crdt

import (
	"bytes"
	"fmt"
	"sort"
	"testing"
)

// The streaming delete-set writer must be byte-identical to the materialized one it replaces, on
// EVERY store shape rather than on five constructed ones.
//
// It is the same hazard as the JSON scalar path: a count pass and an emit pass that must agree. If
// deletedStructRangeCount folds a run the emit loop splits (or vice versa), the range count in the
// header disagrees with the ranges that follow, and every later field in the update decodes at the
// wrong offset. Descending client order and omitting delete-free clients are the same kind of
// property — silent when wrong, and fatal to the stream.
//
// So this compares the two implementations directly over randomized STORES BUILT BY REAL DOCUMENTS,
// which produce the structure shapes an encoder actually meets: merged runs, tombstones from
// several peers, GC'd items, clients holding no deletions at all.

func streamedVsMaterialized(t *testing.T, store *structStore, label string) {
	t.Helper()

	clients := store.appendClientIDs(make([]Number, 0, store.clientCountValue()))
	sort.Slice(clients, func(i, j int) bool { return clients[i] > clients[j] })

	streamed := newUpdateEncoderV1()
	if err := writeDeleteSetFromStructStore(streamed, store, clients); err != nil {
		t.Fatalf("%s: streamed: %v", label, err)
	}
	materialized := newUpdateEncoderV1()
	if err := writeDeleteSet(materialized, newDeleteSetFromStructStore(store)); err != nil {
		t.Fatalf("%s: materialized: %v", label, err)
	}
	if a, b := streamed.toBytes(), materialized.toBytes(); !bytes.Equal(a, b) {
		t.Fatalf("%s: streamed delete set differs from materialized\n streamed     % x\n materialized % x",
			label, a, b)
	}

	// Also with the caller's slice absent, which is the path that rebuilds and re-sorts. Both
	// branches must agree with the same materialized answer, or the encoding would depend on how
	// the caller happened to enumerate clients.
	rebuilt := newUpdateEncoderV1()
	if err := writeDeleteSetFromStructStore(rebuilt, store, nil); err != nil {
		t.Fatalf("%s: rebuilt-clients: %v", label, err)
	}
	if a, b := rebuilt.toBytes(), materialized.toBytes(); !bytes.Equal(a, b) {
		t.Fatalf("%s: rebuilt-client-order path differs from materialized\n rebuilt      % x\n materialized % x",
			label, a, b)
	}
}

func TestStreamedDeleteSetMatchesMaterializedOnRealDocuments(t *testing.T) {
	for seed := 0; seed < 300; seed++ {
		gc := seed%2 == 0
		doc := newDoc("g", gc, defaultGCFilter, nil, false, WithClientID(Number(1+seed%4)))
		txt := doc.GetText("t")
		arr := doc.GetArray("a")
		m := doc.GetMap("m")
		rng := markerLCG(uint32(seed*2654435761 + 29))

		// Several peers, so the store holds multiple clients in non-numeric insertion order.
		for peer := 0; peer < 1+seed%3; peer++ {
			other := newDoc("g", gc, defaultGCFilter, nil, false, WithClientID(Number(100+peer*37+seed%11)))
			ot := other.GetText("t")
			for i := 0; i < 5+rng(20); i++ {
				ot.Insert(Number(rng(int(ot.Length())+1)), "p", Object{})
			}
			if ot.Length() > 4 {
				ot.Delete(Number(rng(int(ot.Length())-2)), 2)
			}
			enc, err := EncodeStateAsUpdateV2(other, nil)
			if err != nil {
				t.Fatalf("seed %d peer encode: %v", seed, err)
			}
			_ = ApplyUpdateV2(doc, enc, nil)
		}

		for step := 0; step < 30; step++ {
			switch rng(6) {
			case 0:
				txt.Insert(Number(rng(int(txt.Length())+1)), "x", Object{})
			case 1:
				if txt.Length() > 3 {
					// Adjacent deletes on consecutive steps are what produce foldable runs.
					txt.Delete(Number(rng(int(txt.Length())-2)), 1+Number(rng(2)))
				}
			case 2:
				arr.Insert(Number(rng(arr.GetLength()+1)), ArrayAny{step})
			case 3:
				if arr.GetLength() > 2 {
					arr.Delete(Number(rng(arr.GetLength()-1)), 1)
				}
			case 4:
				m.Set(fmt.Sprintf("k%d", rng(6)), step)
			default:
				m.Delete(fmt.Sprintf("k%d", rng(6)))
			}
		}
		streamedVsMaterialized(t, doc.store, fmt.Sprintf("seed %d", seed))
	}
}

// An empty store and a store whose every client is delete-free are the two shapes where the header
// must be a bare zero; they are cheap and they are the ones an encoder meets first.
func TestStreamedDeleteSetEdgeShapes(t *testing.T) {
	streamedVsMaterialized(t, newStructStore(), "empty store")

	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	doc.GetText("t").Insert(0, "no deletions here", Object{})
	streamedVsMaterialized(t, doc.store, "single client, no deletions")

	peer := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(2))
	peer.GetText("t").Insert(0, "also none", Object{})
	enc, err := EncodeStateAsUpdateV2(peer, nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	_ = ApplyUpdateV2(doc, enc, nil)
	streamedVsMaterialized(t, doc.store, "two clients, no deletions")
}
