package crdt

import (
	"os"
	"strconv"
	"testing"
)

// BenchmarkTextInsertRandomFormattedDense is the rich-text counterpart of the dense plain-text
// scaling benchmark. The first character opens one bold run; every later nil-attribute insert must
// inherit that run, so positioning must recover CurrentAttributes as well as a visible index.
// FORMATTED_MARKER_BENCH_N permits scaling without changing the fixture.
func BenchmarkTextInsertRandomFormattedDense(b *testing.B) {
	target := 16_000
	if value := os.Getenv("FORMATTED_MARKER_BENCH_N"); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil && parsed > 1 {
			target = parsed
		}
	}
	b.ReportAllocs()
	for iteration := 0; iteration < b.N; iteration++ {
		doc := newDoc("formatted-scale", false, defaultGCFilter, nil, false, WithClientID(1))
		text := doc.GetText("t")
		attrs := newObject()
		attrs.Set("bold", true)
		text.Insert(0, "x", attrs)
		state := uint32(42 + iteration)
		for i := 1; i < target; i++ {
			state = state*1664525 + 1013904223
			// Position zero is before the opening bold marker and deliberately has different
			// inheritance semantics. Sample [1,length] so every edit remains inside this run.
			text.Insert(1+Number(state%uint32(text.Length())), "x", Object{})
		}
	}
}

// BenchmarkTextFormatChurnIndexed pins the opposite workload: once a format mutation invalidates
// the index, format-dense churn must stay on the original linked path instead of paying tree
// maintenance for an accelerator it cannot reuse.
func BenchmarkTextFormatChurnIndexed(b *testing.B) {
	b.ReportAllocs()
	for iteration := 0; iteration < b.N; iteration++ {
		b.StopTimer()
		doc := newDoc("formatted-churn-index", false, defaultGCFilter, nil, false, WithClientID(1))
		text := doc.GetText("t")
		text.Insert(0, "x", boldAttr(true))
		state := uint32(42 + iteration)
		for i := 1; i < 4_000; i++ {
			state = state*1664525 + 1013904223
			text.Insert(1+Number(state%uint32(text.Length())), "x", Object{})
		}
		if _, index := ownedListPositionIndex(text); index == nil {
			b.Fatal("fixture did not activate the formatted position index")
		}
		b.StartTimer()
		for i := 0; i < 1_000; i++ {
			attrs := newObject()
			switch i % 3 {
			case 0:
				attrs.Set("bold", true)
			case 1:
				attrs.Set("italic", true)
			case 2:
				attrs.Set("bold", Null)
			}
			state = state*1664525 + 1013904223
			text.Format(Number(state%3980), 20, attrs)
		}
	}
}
