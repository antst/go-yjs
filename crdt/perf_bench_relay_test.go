package crdt

import (
	"fmt"
	"testing"
)

// Benchmarks shaped like a relay serving multiple Yjs clients.
//
// WHY. Every other apply/encode benchmark uses a FULL document: perfLarge is
// 10,000 ops, ApplyV1 moves 2.6 MB in ~2.4 ms. That is the CONNECT path. It says
// nothing about a live session, where a client sends one keystroke and the server
// integrates a handful of structs into a document that already exists. Nothing
// measured that, so every ratio we had described joining a session rather than
// being in one.
//
// THE TWO SHAPES ARE NOT INTERCHANGEABLE, and picking the wrong one silently
// measures the wrong thing. Measured here:
//
//   - A transaction update — what the doc's "update" event emits and what a relay
//     actually broadcasts — carries only that transaction's structs and deletes.
//     Applying one costs ~2-3 us and does NOT grow as the document ages.
//   - A catch-up diff — EncodeStateAsUpdate against a client's state vector, what
//     a reconnecting peer receives — carries the document's ENTIRE delete set.
//     That set is compact on the wire (one range) but expands on apply: a 25-byte
//     payload can name 28,000 tombstoned structs, every one of which is walked.
//     Linear in tombstones with allocations flat, so it is traversal, not
//     allocation, and it is per RECONNECT rather than per message.
//
// Current figures on arm64, medians:
//
//	RelaySteadyState          2,016 ns    2,519 B   35 allocs
//	RelayReconnect tomb-0     1,483 ns    1,928 B   32 allocs
//	RelayReconnect tomb-4k   14,953 ns    1,984 B   35 allocs
//	RelayReconnect tomb-12k  40,226 ns    1,984 B   35 allocs
//	RelayReconnect tomb-28k  92,868 ns    1,984 B   35 allocs
//	RelayDiffEncode          16,224 ns   41,392 B   10 allocs
//
// The reconnect rows improved 17-19% when StructStore's representation was
// encapsulated (c21ebf7) and the delete-set walk moved behind the per-client
// bulk methods — 17,932 -> 14,953 at 4k, 49,786 -> 40,226 at 12k, 114,100 ->
// 92,868 at 28k. The scaling term is unchanged; only its constant moved. Keep
// these numbers current: they are cited as the evidence for the claim above, and
// a stale table is an argument for something the code no longer does.
//
// The first draft of this file used the catch-up shape and called it steady
// state, which made a per-reconnect cost look like a per-keystroke cost and
// invented an O(tombstones) scaling defect in the hot path that does not exist.
// Both shapes are kept, named for what they are, and the reconnect row is
// parameterised by tombstone count so the scaling term is visible in the suite
// instead of hiding below the fixture sizes the rest of the suite happens to use.
//
// Fan-out is deliberately absent: a relay broadcasts the same bytes to every
// other client, so the library is not re-entered per recipient.

const (
	relayLiveKeys  = 4000
	relayDeltaPool = 8192
)

// relayDoc builds a map document with liveKeys distinct keys written reps times.
// reps > 1 leaves liveKeys*(reps-1) tombstones, which is what repeated edits to
// the same elements produce — dragging a shape, editing a field.
func relayDoc(id Number, liveKeys, reps int) *Doc {
	d := newDoc("relay", false, defaultGCFilter, nil, false, WithClientID(id))
	m := d.GetMap("m")
	for r := 0; r < reps; r++ {
		for j := 0; j < liveKeys; j++ {
			m.Set(fmt.Sprintf("k%d", j), r*liveKeys+j)
		}
	}
	return d
}

// relayTransactionUpdates captures the update each local transaction emits: the
// exact bytes a peer puts on the wire mid-session.
func relayTransactionUpdates(tb testing.TB, base []uint8, n int) [][]uint8 {
	tb.Helper()
	client := newDoc("relay", false, defaultGCFilter, nil, false, WithClientID(7001))
	if err := ApplyUpdate(client, base, nil); err != nil {
		tb.Fatal(err)
	}
	m := client.GetMap("m")
	out := make([][]uint8, 0, n)
	client.On("update", NewObserverHandler(func(args ...interface{}) {
		if b, ok := args[0].([]uint8); ok {
			out = append(out, append([]uint8(nil), b...))
		}
	}))
	for i := 0; i < n; i++ {
		m.Set(fmt.Sprintf("fresh%d", i), i)
	}
	if len(out) != n {
		tb.Fatalf("captured %d transaction updates, want %d", len(out), n)
	}
	return out
}

// The per-message cost of a live session.
func BenchmarkRelaySteadyState(b *testing.B) {
	base, err := EncodeStateAsUpdate(relayDoc(1, relayLiveKeys, 1), nil)
	if err != nil {
		b.Fatal(err)
	}
	deltas := relayTransactionUpdates(b, base, relayDeltaPool)

	benchReleaseSinks(b)
	server := perfSinkDoc()
	if err := ApplyUpdate(server, base, nil); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i%relayDeltaPool == 0 && i > 0 {
			// Replaying an already-integrated update measures the no-op path, not
			// integration, so the document is rebuilt rather than allowed to repeat.
			b.StopTimer()
			server = perfSinkDoc()
			if err := ApplyUpdate(server, base, nil); err != nil {
				b.Fatal(err)
			}
			b.StartTimer()
		}
		benchSinkErr = ApplyUpdate(server, deltas[i%relayDeltaPool], nil)
	}
}

// The reconnect cost, as a function of accumulated tombstones. Struct
// integration happens on the first iteration; the delete-set walk happens on
// EVERY one, and that walk is the term that scales — which is exactly what this
// row exists to expose.
func benchRelayReconnect(b *testing.B, reps int) {
	server := relayDoc(1, relayLiveKeys, reps)
	base, err := EncodeStateAsUpdate(server, nil)
	if err != nil {
		b.Fatal(err)
	}
	sv := encodeStateVectorWith(server, nil, newUpdateEncoderV1())

	peer := newDoc("relay", false, defaultGCFilter, nil, false, WithClientID(7002))
	if err := ApplyUpdate(peer, base, nil); err != nil {
		b.Fatal(err)
	}
	pm := peer.GetMap("m")
	pm.Set("late", 1)
	catchup, err := EncodeStateAsUpdate(peer, sv)
	if err != nil {
		b.Fatal(err)
	}

	benchReleaseSinks(b)
	target := perfSinkDoc()
	if err := ApplyUpdate(target, base, nil); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchSinkErr = ApplyUpdate(target, catchup, nil)
	}
}

func BenchmarkRelayReconnect(b *testing.B) {
	for _, reps := range []int{1, 2, 4, 8} {
		b.Run(fmt.Sprintf("tomb-%d", relayLiveKeys*(reps-1)), func(b *testing.B) {
			benchRelayReconnect(b, reps)
		})
	}
}

// Server-side half of a reconnect: encode only what the returning peer misses.
// Distinct from EncodeV1, which passes a nil state vector and always serialises
// the whole document.
func BenchmarkRelayDiffEncode(b *testing.B) {
	doc := relayDoc(1, relayLiveKeys, 1)
	sv := encodeStateVectorWith(doc, nil, newUpdateEncoderV1())
	m := doc.GetMap("m")
	for i := 0; i < 16; i++ {
		m.Set(fmt.Sprintf("after%d", i), i)
	}

	benchReleaseSinks(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, err := EncodeStateAsUpdate(doc, sv)
		benchSinkErr = err
		benchSinkBool = len(out) > 0
	}
}
