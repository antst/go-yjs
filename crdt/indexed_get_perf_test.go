package crdt

import "testing"

// BenchmarkArrayGetRandomDecoded100k pins the load-then-read shape that rules out fixing the
// concurrent-Get race by abandoning cache population. The source is deliberately fragmented by
// random inserts, then decoded so neither the writer's marker cache nor any Go object identity is
// inherited. Setup is excluded from the timed region.
func BenchmarkArrayGetRandomDecoded100k(b *testing.B) {
	source := perfDoc()
	arr := source.GetArray("a")
	rng := perfRand()
	for i := 0; i < perfHundredK; i++ {
		arr.Insert(rng.intn(arr.GetLength()+1), ArrayAny{i})
	}
	update, err := EncodeStateAsUpdateV2(source, nil)
	if err != nil {
		b.Fatal(err)
	}
	sink := perfSinkDoc()
	_ = ApplyUpdateV2(sink, update, nil)
	decoded := sink.GetArray("a")
	if len(decoded.searchMarker) != 0 {
		b.Fatalf("decoded write-marker cache starts at %d, want 0", len(decoded.searchMarker))
	}

	rng = perfRand()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = decoded.Get(rng.intn(decoded.GetLength()))
	}
}
