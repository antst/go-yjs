package crdt

import "testing"

type contentStringDeletePosition uint8

const (
	contentStringDeleteMiddle contentStringDeletePosition = iota
	contentStringDeleteRandom
)

func contentStringDeletedRunStats(text *YText) (items int, total, largest Number) {
	for item := text.start; item != nil; item = item.right {
		if !item.isDeleted() {
			continue
		}
		items++
		total += item.length
		largest = max(largest, item.length)
	}
	return items, total, largest
}

func TestContentStringDeletePositionRunShapes(t *testing.T) {
	const runes = 2000
	for _, tc := range []struct {
		name     string
		position string
	}{
		{name: "front", position: "front"},
		{name: "middle", position: "middle"},
		{name: "tail", position: "tail"},
		{name: "random", position: "random"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := newDoc("string-remerge-shape", false, defaultGCFilter, nil, false, WithClientID(1))
			text := doc.GetText("t")
			for i := 0; i < runes; i++ {
				text.Insert(text.Length(), "界", Object{})
			}
			if text.start == nil || text.start.right != nil || text.start.length != runes {
				t.Fatalf("fixture did not coalesce into one %d-unit item", runes)
			}

			state := uint64(1)
			for i := 0; i < runes/2; i++ {
				var index Number
				switch tc.position {
				case "front":
					index = 0
				case "middle":
					index = text.Length() / 2
				case "tail":
					index = text.Length() - 1
				case "random":
					state = state*6364136223846793005 + 1442695040888963407
					index = Number(state % uint64(text.Length()))
				}
				text.Delete(index, 1)
			}

			deletedItems, deletedLength, maxDeletedLength := contentStringDeletedRunStats(text)
			if deletedLength != runes/2 {
				t.Fatalf("deleted length = %d, want %d", deletedLength, runes/2)
			}
			if tc.position == "random" {
				if deletedItems <= 1 || maxDeletedLength >= deletedLength/4 {
					t.Fatalf("random deletes produced %d runs with max %d of %d units, want dispersed runs",
						deletedItems, maxDeletedLength, deletedLength)
				}
				return
			}
			if deletedItems != 1 || maxDeletedLength != deletedLength {
				t.Fatalf("%s deletes produced %d runs with max %d, want one run of %d units",
					tc.position, deletedItems, maxDeletedLength, deletedLength)
			}
		})
	}
}

// benchContentStringPositionDelete complements the front and tail ladders in
// perf_bench_content_string_split_test.go. Repeated middle deletion grows one
// adjacent tombstone run, while deterministic random deletion disperses the
// tombstones. Comparing those shapes separates the cost of remerging a growing
// run from the cost of deletion in general.
func benchContentStringPositionDelete(
	b *testing.B,
	runes int,
	unit string,
	position contentStringDeletePosition,
) {
	b.Helper()
	unitLength := stringLength(unit)
	var lastText *YText
	b.ReportAllocs()
	b.ResetTimer()
	for done := 0; done < b.N; {
		b.StopTimer()
		count := min(benchStringFixtureBatch, b.N-done)
		texts, fixtureUnitLength := prepareCoalescedFixtures(b, count, runes, unit)
		if fixtureUnitLength != unitLength {
			b.Fatalf("fixture unit length = %d, want %d", fixtureUnitLength, unitLength)
		}
		b.StartTimer()
		for _, text := range texts {
			state := uint64(1)
			for j := 0; j < runes/2; j++ {
				length := text.Length()
				var index Number
				switch position {
				case contentStringDeleteMiddle:
					index = length / 2
				case contentStringDeleteRandom:
					state = state*6364136223846793005 + 1442695040888963407
					index = Number(state%uint64(length/unitLength)) * unitLength
				default:
					b.Fatalf("unknown delete position %d", position)
				}
				text.Delete(index, unitLength)
			}
			lastText = text
			done++
		}
	}
	b.StopTimer()
	if got, want := lastText.Length(), Number(runes/2)*unitLength; got != want {
		b.Fatalf("length after deletes = %d, want %d", got, want)
	}
	deletedItems, deletedLength, maxDeletedLength := contentStringDeletedRunStats(lastText)
	if want := Number(runes/2) * unitLength; deletedLength != want {
		b.Fatalf("deleted length = %d, want %d", deletedLength, want)
	}
	if position == contentStringDeleteMiddle &&
		(deletedItems != 1 || maxDeletedLength != deletedLength) {
		b.Fatalf("middle deletes produced %d deleted items with max length %d, want one run of %d",
			deletedItems, maxDeletedLength, deletedLength)
	}
	if position == contentStringDeleteRandom && deletedItems <= 1 {
		b.Fatalf("random deletes produced %d deleted items, want dispersed runs", deletedItems)
	}
	b.ReportMetric(float64(deletedItems), "deleted_items")
	b.ReportMetric(float64(maxDeletedLength), "max_deleted_units")
}

func BenchmarkContentStringMiddleDeleteASCII2000(b *testing.B) {
	benchContentStringPositionDelete(b, 2000, "a", contentStringDeleteMiddle)
}
func BenchmarkContentStringMiddleDeleteASCII8000(b *testing.B) {
	benchContentStringPositionDelete(b, 8000, "a", contentStringDeleteMiddle)
}
func BenchmarkContentStringMiddleDeleteASCII32000(b *testing.B) {
	benchContentStringPositionDelete(b, 32000, "a", contentStringDeleteMiddle)
}
func BenchmarkContentStringMiddleDeleteCJK1000(b *testing.B) {
	benchContentStringPositionDelete(b, 1000, "界", contentStringDeleteMiddle)
}
func BenchmarkContentStringMiddleDeleteCJK2000(b *testing.B) {
	benchContentStringPositionDelete(b, 2000, "界", contentStringDeleteMiddle)
}
func BenchmarkContentStringMiddleDeleteCJK4000(b *testing.B) {
	benchContentStringPositionDelete(b, 4000, "界", contentStringDeleteMiddle)
}
func BenchmarkContentStringMiddleDeleteCJK8000(b *testing.B) {
	benchContentStringPositionDelete(b, 8000, "界", contentStringDeleteMiddle)
}
func BenchmarkContentStringMiddleDeleteCJK16000(b *testing.B) {
	benchContentStringPositionDelete(b, 16000, "界", contentStringDeleteMiddle)
}
func BenchmarkContentStringMiddleDeleteCJK32000(b *testing.B) {
	benchContentStringPositionDelete(b, 32000, "界", contentStringDeleteMiddle)
}

func BenchmarkContentStringRandomDeleteASCII2000(b *testing.B) {
	benchContentStringPositionDelete(b, 2000, "a", contentStringDeleteRandom)
}
func BenchmarkContentStringRandomDeleteASCII8000(b *testing.B) {
	benchContentStringPositionDelete(b, 8000, "a", contentStringDeleteRandom)
}
func BenchmarkContentStringRandomDeleteASCII32000(b *testing.B) {
	benchContentStringPositionDelete(b, 32000, "a", contentStringDeleteRandom)
}
func BenchmarkContentStringRandomDeleteCJK1000(b *testing.B) {
	benchContentStringPositionDelete(b, 1000, "界", contentStringDeleteRandom)
}
func BenchmarkContentStringRandomDeleteCJK2000(b *testing.B) {
	benchContentStringPositionDelete(b, 2000, "界", contentStringDeleteRandom)
}
func BenchmarkContentStringRandomDeleteCJK4000(b *testing.B) {
	benchContentStringPositionDelete(b, 4000, "界", contentStringDeleteRandom)
}
func BenchmarkContentStringRandomDeleteCJK8000(b *testing.B) {
	benchContentStringPositionDelete(b, 8000, "界", contentStringDeleteRandom)
}
func BenchmarkContentStringRandomDeleteCJK16000(b *testing.B) {
	benchContentStringPositionDelete(b, 16000, "界", contentStringDeleteRandom)
}
func BenchmarkContentStringRandomDeleteCJK32000(b *testing.B) {
	benchContentStringPositionDelete(b, 32000, "界", contentStringDeleteRandom)
}
