package crdt

import (
	"os"
	"strconv"
	"testing"
)

// BenchmarkTextInsertRandomDense128k keeps the large, densely-fragmented shape beside the sparse
// paste-then-edit benchmark. The ordinary suite stops at 10k operations, below the fixed-marker
// cliff; this cumulative build is intentionally timed end to end and should normally run with
// -benchtime=1x. MARKER_BENCH_N permits scaling without changing the fixture.
func BenchmarkTextInsertRandomDense128k(b *testing.B) {
	target := 128_000
	if value := os.Getenv("MARKER_BENCH_N"); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil && parsed > 0 {
			target = parsed
		}
	}
	b.ReportAllocs()
	for iteration := 0; iteration < b.N; iteration++ {
		doc := newDoc("marker-bench", false, defaultGCFilter, nil, false, WithClientID(1))
		text := doc.GetText("t")
		state := uint32(42 + iteration)
		for i := 0; i < target; i++ {
			state = state*1664525 + 1013904223
			text.Insert(Number(state%uint32(text.Length()+1)), "x", Object{})
		}
	}
}
