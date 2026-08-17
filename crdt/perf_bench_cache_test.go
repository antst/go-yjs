package crdt

import (
	"fmt"
	"testing"
)

// Map read projections under WRITES, which is the shape that decides whether
// their caches are a real win or a benchmark artifact.
//
// WHY. MapKeys, MapEntries and MapToJson are cached, and every published row
// reads the same map forever without ever writing to it. That is the single most
// favourable workload a cache can be given, and it is not how a document behaves:
// editing interleaves writes, and each write invalidates.
//
// Measured here at 2,000 keys, with the write cost separated out (writes alone
// cost 25 ns at 16:1, 100 ns at 4:1, 407 ns at 1:1, so essentially all of the
// difference below is cache rebuild, not the write):
//
//	workload          Keys()      yrs keys() iterator, same size
//	read-only          5,198 ns    3,735 ns
//	16 reads/write    10,176 ns    3,735 ns
//	4 reads/write     17,521 ns    3,735 ns
//	1 read/write      23,439 ns    3,735 ns
//
// Two things follow. We are slower than the reference even in the best case, and
// the gap widens to ~6x as writes interleave, because yrs traverses its map on
// each call and so has nothing to invalidate. A cache that wins only in read-only
// bursts, costs ~32 KB per 2,000-key map, and carries priming thresholds and
// three-way invalidation is not obviously the right trade — a non-allocating
// traversal (a range-over-func iterator) would be flat across every column.
//
// Writes OVERWRITE existing keys so the live key count, and therefore the size of
// the result, stays constant. Only cache validity varies between these rows. An
// earlier version of this file appended NEW keys, which grew the map during the
// run and inflated the interleaved rows by counting a larger result rather than a
// colder cache.
//
// APPENDKEYS DOES NOT ESCAPE THIS, and its headline number is a read-burst figure.
// It removes the 32 KB copy when the caller reuses a buffer, but it cannot remove
// the O(n) rebuild a write forces. Measured on amd64 at 2,000 keys, caller reusing
// one cap-2000 buffer, the write amortised across the reads that follow it:
//
//	shape            AppendKeys reused        Keys      ratio
//	read-only              506.8 ns      10,721 ns      21.2x
//	16 reads/write       4,793          19,332           4.03x
//	4 reads/write       17,422          30,797           1.77x
//	1 read/write        25,666          38,409           1.50x
//
// AppendKeys itself degrades 9.5x / 34x / 51x from its warm-cache floor as writes
// get more frequent, and its advantage over Keys collapses from 21x to 1.5x. Quote
// the 20x only next to the shape it belongs to.
//
// THE ALLOCATION COLUMNS EXPLAIN IT, and the 1:1 row is the interesting one. At
// 16:1 the ~2,065 B/read is 32768/16 of cache clone plus the amortised write; at
// 4:1 it is 32768/4 plus the write. At 1:1 the 32 KB clone DISAPPEARS, because
// re-arming takes two consecutive reads: keysPrimed.Swap(true) returns the OLD
// value, so the first read after an invalidation only sets the flag and the second
// one stores (y_map.go Keys and AppendKeys). With a write before every read that
// second read never comes, the cache never arms, every read walks the map, and the
// priming machinery is pure overhead on that workload.
//
// Go rounds allocs/op to an integer, so the displayed 0 allocs at 16:1 and 4:1 does
// not mean no cache allocation — B/op is the honest column there.
func benchKeysWriteRatio(b *testing.B, readsPerWrite int) {
	m := benchMap(perfSmall)
	for i := 0; i < 8; i++ {
		_ = m.Keys() // prime; the cache arms on the second read
	}
	benchReleaseSinks(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if readsPerWrite > 0 && i%readsPerWrite == 0 {
			m.Set(mapKey(i%perfSmall), i)
		}
		benchSinkStrs = m.Keys()
	}
}

func BenchmarkMapKeysReadOnly(b *testing.B)       { benchKeysWriteRatio(b, 0) }
func BenchmarkMapKeysRead16PerWrite(b *testing.B) { benchKeysWriteRatio(b, 16) }
func BenchmarkMapKeysRead4PerWrite(b *testing.B)  { benchKeysWriteRatio(b, 4) }
func BenchmarkMapKeysWritePerRead(b *testing.B)   { benchKeysWriteRatio(b, 1) }

// The same interleaving without the read, so a reader can subtract the write's
// own cost rather than attributing all of it to the cache.
func benchMapWriteOnly(b *testing.B, readsPerWrite int) {
	m := benchMap(perfSmall)
	for i := 0; i < 8; i++ {
		_ = m.Keys()
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if readsPerWrite > 0 && i%readsPerWrite == 0 {
			m.Set(mapKey(i%perfSmall), i)
		}
	}
}

func BenchmarkMapWriteOnly16PerWrite(b *testing.B) { benchMapWriteOnly(b, 16) }
func BenchmarkMapWriteOnly4PerWrite(b *testing.B)  { benchMapWriteOnly(b, 4) }
func BenchmarkMapWriteOnlyPerRead(b *testing.B)    { benchMapWriteOnly(b, 1) }

// ToJson carries the largest cached projection (196,936 B at 2,000 keys), so it
// has the most to lose when a write invalidates it.
func benchToJsonWriteRatio(b *testing.B, readsPerWrite int) {
	m := benchMap(perfSmall)
	for i := 0; i < 16; i++ {
		_ = m.ToJson() // the JSON cache arms after yMapEntriesCacheThreshold reads
	}
	benchReleaseSinks(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if readsPerWrite > 0 && i%readsPerWrite == 0 {
			m.Set(mapKey(i%perfSmall), i)
		}
		benchSinkAny = m.ToJson()
	}
}

func BenchmarkMapToJsonReadOnly(b *testing.B)       { benchToJsonWriteRatio(b, 0) }
func BenchmarkMapToJsonRead16PerWrite(b *testing.B) { benchToJsonWriteRatio(b, 16) }
func BenchmarkMapToJsonRead4PerWrite(b *testing.B)  { benchToJsonWriteRatio(b, 4) }

// The uncached traversal, which is WHY the caches above exist and is worth
// understanding before changing any of them. Real documents exceed
// maxYMapCachedKeys (4096), so this is a production path and not only a
// diagnostic: past that threshold every Keys() call walks the whole map. The
// guard asserts the cache really is inert here, because a silently-armed cache
// would turn this row into another warm-cache measurement.
//
//	5,000 keys    58,103 ns    11.6 ns/key
//	10,000 keys  121,501 ns    12.2 ns/key
//	yrs, 2,000 keys, iterator only, no Vec:   1.87 ns/key
//
// Linear, and about 6.3x more expensive per key than the reference. Split
// further on a 5,000-key map:
//
//	iterate map[string]*Item, collect keys, no deref   10.34 ns/key
//	the same plus item.Deleted() on every entry        11.44 ns/key
//
// So the Deleted() dereference costs ~1.1 ns/key and the other 10.34 is
// iterating the Go map itself. That is the finding the rows above only hint at:
// the read caches are not making reads fast, they are masking a slow traversal,
// which is exactly why they win in read-only bursts and collapse once writes
// invalidate them.
//
// It also bounds where a fix can come from. Optimising the Deleted() check buys
// ~1 ns of 12. A better cache still rebuilds in O(n). What would be flat across
// every column is maintaining the live key set incrementally at O(1) per write
// rather than rebuilding it O(n) per invalidation — a representation change to
// YMap, not a tuning change.
func benchUncachedKeys(b *testing.B, n int) {
	m := perfDoc().GetMap("m")
	for j := 0; j < n; j++ {
		m.Set(fmt.Sprintf("k%d", j), j)
	}
	for i := 0; i < 4; i++ {
		_ = m.Keys()
	}
	if c := m.keysCache.Load(); c != nil {
		b.Fatalf("keys cache armed at n=%d (limit %d); this row would measure the "+
			"cache, not the traversal it exists to hide", n, maxYMapCachedKeys)
	}
	benchReleaseSinks(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchSinkStrs = m.Keys()
	}
}

func BenchmarkMapKeysUncached5000(b *testing.B)  { benchUncachedKeys(b, 5000) }
func BenchmarkMapKeysUncached10000(b *testing.B) { benchUncachedKeys(b, 10000) }
