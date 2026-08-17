package crdt

import (
	"strconv"
	"strings"
	"testing"
)

var benchContentStringUTF16IndexSink *contentStringUTF16Index

func benchContentStringUTF16IndexBuild(b *testing.B, runes int) {
	source := strings.Repeat("界", runes)
	length := Number(runes)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchContentStringUTF16IndexSink = buildContentStringUTF16Index(source, length)
	}
}

func BenchmarkContentStringUTF16IndexBuildBelowThreshold64(b *testing.B) {
	benchContentStringUTF16IndexBuild(b, 64)
}

func BenchmarkContentStringUTF16IndexBuild128(b *testing.B) {
	benchContentStringUTF16IndexBuild(b, 128)
}

func BenchmarkContentStringUTF16IndexBuild2000(b *testing.B) {
	benchContentStringUTF16IndexBuild(b, 2000)
}

func BenchmarkContentStringUTF16IndexBuild8000(b *testing.B) {
	benchContentStringUTF16IndexBuild(b, 8000)
}

func BenchmarkContentStringUTF16IndexBuild32000(b *testing.B) {
	benchContentStringUTF16IndexBuild(b, 32000)
}

func newBenchStringDoc() *Doc {
	return newDoc("string-split-scale", false, defaultGCFilter, nil, false, WithClientID(1))
}

// buildCoalescedStringBenchTextIn builds the fixture inside a CALLER-OWNED
// document under a caller-chosen name.
//
// WHY THE DOCUMENT IS A PARAMETER. Every timed round consumes its fixture, so a
// fresh one is needed each time — but constructing a Doc costs far more than the
// deletes being measured, and at the smallest crossover sizes the timed body is a
// single delete. b.N is sized from that timed body alone, so it autoscaled into
// hundreds of thousands of Doc constructions: BenchmarkContentStringTailDelete-
// Crossover2 measured 68.8s of wall clock against 2.5s of timed work, a ratio of
// 28. Taking the document from the caller lets one construction serve a batch of
// rounds, each round still getting a virgin coalesced run under its own name so
// no tombstone from a previous round can change the shape being measured.
func buildCoalescedStringBenchTextIn(b *testing.B, doc *Doc, name string, runes int, unit string) (*YText, Number) {
	b.Helper()
	unitLength := stringLength(unit)
	wantLength := Number(runes) * unitLength
	text := doc.GetText(name)
	for j := 0; j < runes; j++ {
		text.Insert(text.Length(), unit, Object{})
	}
	if text.start == nil {
		b.Fatal("fixture produced no Item")
	}
	content, ok := text.start.content.(*contentString)
	if !ok || text.start.right != nil || text.start.isDeleted() || text.start.length != wantLength ||
		text.Length() != wantLength {
		b.Fatalf("fixture did not coalesce: content=%T right=%p deleted=%v itemLength=%d textLength=%d want=%d",
			text.start.content, text.start.right, text.start.isDeleted(), text.start.length, text.Length(), wantLength)
	}
	if unit == "a" && !content.hasASCIIWidth(wantLength) {
		b.Fatal("ASCII fixture did not reach the validated O(1) split path")
	}
	if unit != "a" && content.hasASCIIWidth(wantLength) {
		b.Fatal("non-ASCII fixture unexpectedly reached the ASCII split path")
	}
	return text, unitLength
}

// benchStringFixtureBatch is how many timed rounds share ONE timer-toggle pair.
//
// THE COST IS THE TOGGLE, NOT THE FIXTURE. b.StopTimer()/b.StartTimer() measures
// at about 53 microseconds per pair on this machine — established by running a
// benchmark whose body is nothing but the pair and differencing wall clock at two
// iteration counts, 1,548 ms at 20,000 pairs against 2,610 ms at 40,000. Against a
// timed body of roughly 2 microseconds at the smallest crossover size, toggling
// per iteration is twenty-five times the cost of the thing being measured, and
// b.N autoscales from the timed body alone: Crossover2 spent 68.8 s of wall clock
// on 2.5 s of measurement, about 33 s of which was toggling.
//
// An earlier attempt at this fix amortised Doc CONSTRUCTION instead and made the
// ratio worse — 28x to 37.9x — because the diagnosis was a guess. Building the
// fixture is 909 ns and the Doc inside it 274 ns, both far below the toggle.
// Preparing a batch under a single toggle pair is what actually removes the cost.
const benchStringFixtureBatch = 256

// prepareCoalescedFixtures builds a batch of virgin fixtures in one document.
// Each round gets its own name so no tombstone from a previous round can change
// the shape being measured.
func prepareCoalescedFixtures(b *testing.B, count, runes int, unit string) ([]*YText, Number) {
	b.Helper()
	doc := newBenchStringDoc()
	texts := make([]*YText, 0, count)
	var unitLength Number
	for k := 0; k < count; k++ {
		text, length := buildCoalescedStringBenchTextIn(b, doc, "t"+strconv.Itoa(k), runes, unit)
		texts = append(texts, text)
		unitLength = length
	}
	return texts, unitLength
}

// Tail deletion isolates the cost of converting a UTF-16 offset into a UTF-8
// byte offset inside a coalesced contentString. The fixture is built one rune at
// a time so the tail-append path must coalesce it into one live Item, matching
// the long-run shape that makes repeated edits expensive.
//
// Deleting from the tail is intentional. A front deletion splits at offset 1
// and lets splitStringUTF16 stop immediately; deleting the last rune splits at
// a near-end offset and therefore scans almost the whole remaining run. Across
// n/2 deletions, a non-ASCII run presents 3*n*n/8 + O(n) preceding runes to the
// offset mapper. The validated-ASCII arm performs the identical Y.Text edits
// but maps every offset with a byte slice instead of a scan.
func benchContentStringTailDelete(b *testing.B, runes int, unit string) {
	b.ReportAllocs()
	b.ResetTimer()
	for done := 0; done < b.N; {
		b.StopTimer()
		count := min(benchStringFixtureBatch, b.N-done)
		texts, unitLength := prepareCoalescedFixtures(b, count, runes, unit)
		b.StartTimer()
		for _, text := range texts {
			for j := 0; j < runes/2; j++ {
				text.Delete(text.Length()-unitLength, unitLength)
			}
			done++
		}
	}
}

// Front deletion is the positional control for the tail workload. It performs
// the same number of Y.Text edits on the same sole-run fixture, but every split
// is at the first rune. splitStringUTF16 can stop immediately there, so a linear
// CJK result means the quadratic is caused by offset mapping rather than by all
// non-ASCII mutation machinery.
func benchContentStringFrontDelete(b *testing.B, runes int, unit string) {
	b.ReportAllocs()
	b.ResetTimer()
	for done := 0; done < b.N; {
		b.StopTimer()
		count := min(benchStringFixtureBatch, b.N-done)
		texts, unitLength := prepareCoalescedFixtures(b, count, runes, unit)
		b.StartTimer()
		for _, text := range texts {
			for j := 0; j < runes/2; j++ {
				text.Delete(0, unitLength)
			}
			done++
		}
	}
}

// Random split resets one contentString to the same immutable backing string
// before every operation. This isolates position mapping from Y.Text item-list
// fragmentation: a real sequence of random edits creates shorter runs, which
// would make later splits cheaper and obscure whether arbitrary offsets still
// need an index. The deterministic LCG presents n/2 uniformly distributed
// offsets, so the average non-ASCII scan remains proportional to n while the
// ASCII control remains O(1) per split. The sampled index is built once outside
// the timer; its one-time construction cost has its own benchmark above.
func benchContentStringRandomSplit(b *testing.B, runes int, unit string) {
	source := strings.Repeat(unit, runes)
	content := contentString{value: source}
	length := content.contentLength()
	source = content.value
	if !content.hasASCIIWidth(length) {
		content.utf16Index = buildContentStringUTF16Index(source, length)
	}
	utf16Index := content.utf16Index
	var right contentString

	benchReleaseSinks(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		state := uint64(1)
		for j := 0; j < runes/2; j++ {
			state = state*6364136223846793005 + 1442695040888963407
			offset := Number(state%uint64(length-1)) + 1
			content.value = source
			content.utf16Index = utf16Index
			content.spliceWithLengthInto(offset, length, &right)
			benchSinkString = right.value
		}
	}
}

// Repeated front split is the primitive-level control for FrontDelete. Unlike
// the Y.Text workload it never creates or merges Items, so it distinguishes the
// mapper's cost at offset one from any transaction-cleanup cost caused by the
// deleted fragments themselves.
func benchContentStringFrontSplit(b *testing.B, runes int, unit string) {
	source := strings.Repeat(unit, runes)
	content := contentString{value: source}
	length := content.contentLength()
	source = content.value
	unitLength := stringLength(unit)
	var right contentString

	benchReleaseSinks(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := 0; j < runes/2; j++ {
			content.value = source
			content.spliceWithLengthInto(unitLength, length, &right)
			benchSinkString = right.value
		}
	}
}

func BenchmarkContentStringTailDeleteASCII1000(b *testing.B) {
	benchContentStringTailDelete(b, 1000, "a")
}
func BenchmarkContentStringTailDeleteASCII2000(b *testing.B) {
	benchContentStringTailDelete(b, 2000, "a")
}
func BenchmarkContentStringTailDeleteASCII4000(b *testing.B) {
	benchContentStringTailDelete(b, 4000, "a")
}
func BenchmarkContentStringTailDeleteASCII8000(b *testing.B) {
	benchContentStringTailDelete(b, 8000, "a")
}
func BenchmarkContentStringTailDeleteASCII16000(b *testing.B) {
	benchContentStringTailDelete(b, 16000, "a")
}
func BenchmarkContentStringTailDeleteASCII32000(b *testing.B) {
	benchContentStringTailDelete(b, 32000, "a")
}

func BenchmarkContentStringTailDeleteCJK1000(b *testing.B) {
	benchContentStringTailDelete(b, 1000, "界")
}
func BenchmarkContentStringTailDeleteCJK2000(b *testing.B) {
	benchContentStringTailDelete(b, 2000, "界")
}
func BenchmarkContentStringTailDeleteCJK4000(b *testing.B) {
	benchContentStringTailDelete(b, 4000, "界")
}
func BenchmarkContentStringTailDeleteCJK8000(b *testing.B) {
	benchContentStringTailDelete(b, 8000, "界")
}
func BenchmarkContentStringTailDeleteCJK16000(b *testing.B) {
	benchContentStringTailDelete(b, 16000, "界")
}
func BenchmarkContentStringTailDeleteCJK32000(b *testing.B) {
	benchContentStringTailDelete(b, 32000, "界")
}

// The main powers-of-two curve starts at 1,000 runes, where CJK is already
// slower. These small sub-benchmarks locate the crossover without adding a
// separate top-level guard process for every tiny fixture.
func benchContentStringTailDeleteCrossoverSize(b *testing.B, runes int) {
	b.Run("ASCII", func(b *testing.B) {
		benchContentStringTailDelete(b, runes, "a")
	})
	b.Run("CJK", func(b *testing.B) {
		benchContentStringTailDelete(b, runes, "界")
	})
}

func BenchmarkContentStringTailDeleteCrossover2(b *testing.B) {
	benchContentStringTailDeleteCrossoverSize(b, 2)
}
func BenchmarkContentStringTailDeleteCrossover4(b *testing.B) {
	benchContentStringTailDeleteCrossoverSize(b, 4)
}
func BenchmarkContentStringTailDeleteCrossover8(b *testing.B) {
	benchContentStringTailDeleteCrossoverSize(b, 8)
}
func BenchmarkContentStringTailDeleteCrossover16(b *testing.B) {
	benchContentStringTailDeleteCrossoverSize(b, 16)
}
func BenchmarkContentStringTailDeleteCrossover32(b *testing.B) {
	benchContentStringTailDeleteCrossoverSize(b, 32)
}
func BenchmarkContentStringTailDeleteCrossover64(b *testing.B) {
	benchContentStringTailDeleteCrossoverSize(b, 64)
}
func BenchmarkContentStringTailDeleteCrossover128(b *testing.B) {
	benchContentStringTailDeleteCrossoverSize(b, 128)
}
func BenchmarkContentStringTailDeleteCrossover256(b *testing.B) {
	benchContentStringTailDeleteCrossoverSize(b, 256)
}
func BenchmarkContentStringTailDeleteCrossover512(b *testing.B) {
	benchContentStringTailDeleteCrossoverSize(b, 512)
}

func BenchmarkContentStringFrontDeleteASCII2000(b *testing.B) {
	benchContentStringFrontDelete(b, 2000, "a")
}
func BenchmarkContentStringFrontDeleteASCII8000(b *testing.B) {
	benchContentStringFrontDelete(b, 8000, "a")
}
func BenchmarkContentStringFrontDeleteASCII32000(b *testing.B) {
	benchContentStringFrontDelete(b, 32000, "a")
}
func BenchmarkContentStringFrontDeleteCJK2000(b *testing.B) {
	benchContentStringFrontDelete(b, 2000, "界")
}
func BenchmarkContentStringFrontDeleteCJK8000(b *testing.B) {
	benchContentStringFrontDelete(b, 8000, "界")
}
func BenchmarkContentStringFrontDeleteCJK32000(b *testing.B) {
	benchContentStringFrontDelete(b, 32000, "界")
}

func BenchmarkContentStringRandomSplitASCII2000(b *testing.B) {
	benchContentStringRandomSplit(b, 2000, "a")
}
func BenchmarkContentStringRandomSplitASCII1000(b *testing.B) {
	benchContentStringRandomSplit(b, 1000, "a")
}
func BenchmarkContentStringRandomSplitASCII4000(b *testing.B) {
	benchContentStringRandomSplit(b, 4000, "a")
}
func BenchmarkContentStringRandomSplitASCII8000(b *testing.B) {
	benchContentStringRandomSplit(b, 8000, "a")
}
func BenchmarkContentStringRandomSplitASCII16000(b *testing.B) {
	benchContentStringRandomSplit(b, 16000, "a")
}
func BenchmarkContentStringRandomSplitASCII32000(b *testing.B) {
	benchContentStringRandomSplit(b, 32000, "a")
}
func BenchmarkContentStringRandomSplitCJK1000(b *testing.B) {
	benchContentStringRandomSplit(b, 1000, "界")
}
func BenchmarkContentStringRandomSplitCJK2000(b *testing.B) {
	benchContentStringRandomSplit(b, 2000, "界")
}
func BenchmarkContentStringRandomSplitCJK4000(b *testing.B) {
	benchContentStringRandomSplit(b, 4000, "界")
}
func BenchmarkContentStringRandomSplitCJK8000(b *testing.B) {
	benchContentStringRandomSplit(b, 8000, "界")
}
func BenchmarkContentStringRandomSplitCJK16000(b *testing.B) {
	benchContentStringRandomSplit(b, 16000, "界")
}
func BenchmarkContentStringRandomSplitCJK32000(b *testing.B) {
	benchContentStringRandomSplit(b, 32000, "界")
}

func BenchmarkContentStringFrontSplitASCII2000(b *testing.B) {
	benchContentStringFrontSplit(b, 2000, "a")
}
func BenchmarkContentStringFrontSplitASCII8000(b *testing.B) {
	benchContentStringFrontSplit(b, 8000, "a")
}
func BenchmarkContentStringFrontSplitASCII32000(b *testing.B) {
	benchContentStringFrontSplit(b, 32000, "a")
}
func BenchmarkContentStringFrontSplitCJK2000(b *testing.B) {
	benchContentStringFrontSplit(b, 2000, "界")
}
func BenchmarkContentStringFrontSplitCJK8000(b *testing.B) {
	benchContentStringFrontSplit(b, 8000, "界")
}
func BenchmarkContentStringFrontSplitCJK32000(b *testing.B) {
	benchContentStringFrontSplit(b, 32000, "界")
}
