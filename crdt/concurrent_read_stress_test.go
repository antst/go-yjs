package crdt

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"sync"
	"testing"
)

// Concurrent-read behaviour under load.
//
// WHY THIS BECAME NECESSARY. Read caching changes the concurrency character of reads. Before it,
// ToString and ToDelta only READ the item chain, so any number of goroutines could call them
// simultaneously on a quiescent document with no shared writes at all. With a cache they also
// STORE, which turns every reader into a writer of shared state. That is a genuine change in the
// contract, and the library documents no concurrency model to fall back on.
//
// The caches use atomic.Pointer and atomic.Bool, which is the right primitive and makes the store
// itself race-free. This test exists to confirm that empirically under -race rather than by reading
// the declarations, and to pin the behaviour so a later "the atomic is overhead, use a plain field"
// change fails here instead of in a consumer's server.
//
// WHAT IS AND IS NOT CLAIMED. Concurrent READS of a quiescent document are what this asserts.
// Concurrent read-with-write is NOT claimed safe by this test and is not safe in the reference
// implementation either -- Yjs is single-threaded and a Go consumer must serialise mutations
// itself. Establishing the read side is what matters, because one-writer-many-readers is the shape
// a Go server actually takes.
//
// Run with -race for the data-race dimension. The value assertions hold without it.

func concurrentReaders() int {
	if v := os.Getenv("CONCURRENT_READERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	if n := runtime.NumCPU() * 2; n > 8 {
		return n
	}
	return 8
}

func concurrentReadRounds() int {
	if v := os.Getenv("CONCURRENT_ROUNDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 500
}

func buildReadFixture() *Doc {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	txt := doc.GetText("t")
	txt.Insert(0, "the quick brown fox jumps over the lazy dog", Object{})
	for i := 0; i < 12; i++ {
		attr := newObject()
		attr.Set("bold", i%2 == 0)
		txt.Format(i*2, 3, attr)
	}
	arr := doc.GetArray("a")
	m := doc.GetMap("m")
	for i := 0; i < 30; i++ {
		arr.Insert(arr.GetLength(), ArrayAny{i})
		m.Set(fmt.Sprintf("k%d", i), i)
	}
	return doc
}

// TestConcurrentReadsAgree hammers one quiescent document from many goroutines. Every reader must
// observe the same value the single-threaded read produced, and -race must stay silent.
func TestConcurrentReadsAgree(t *testing.T) {
	doc := buildReadFixture()
	want := readAll(doc) // established single-threaded, before any concurrency

	readers, rounds := concurrentReaders(), concurrentReadRounds()
	var wg sync.WaitGroup
	errs := make(chan string, readers)

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				if got := readAll(doc); got != want {
					errs <- fmt.Sprintf("reader %d iter %d: divergent read\n want %s\n got  %s",
						r, i, want, got)
					return
				}
			}
		}(r)
	}
	wg.Wait()
	close(errs)
	n := 0
	for e := range errs {
		n++
		if n <= 3 {
			t.Error(e)
		}
	}
	t.Logf("CONCURRENT_READS readers=%d rounds=%d reads=%d failures=%d",
		readers, rounds, readers*rounds, n)
}

// TestConcurrentReadsRacePrimeThePrimer targets the narrow window the deferred cache opens: the
// FIRST reads of a document, where several goroutines can reach the priming path simultaneously.
// Each iteration uses a brand-new document so that window is re-entered every time rather than
// being passed once at startup.
func TestConcurrentReadsRacePrimeThePrimer(t *testing.T) {
	readers := concurrentReaders()
	rounds := concurrentReadRounds() / 5

	for i := 0; i < rounds; i++ {
		doc := buildReadFixture()
		want := readAll(doc)

		var wg sync.WaitGroup
		var mu sync.Mutex
		var bad []string
		// Every goroutine starts on the same document with a cold cache, so they contend for the
		// priming path rather than arriving after one of them has already won it.
		start := make(chan struct{})
		for r := 0; r < readers; r++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for k := 0; k < 4; k++ {
					if got := readAll(doc); got != want {
						mu.Lock()
						bad = append(bad, got)
						mu.Unlock()
						return
					}
				}
			}()
		}
		close(start)
		wg.Wait()
		if len(bad) > 0 {
			t.Fatalf("round %d: %d readers diverged while priming\n want %s\n got  %s",
				i, len(bad), want, bad[0])
		}
	}
	t.Logf("CONCURRENT_PRIME rounds=%d readers=%d", rounds, readers)
}

// TestConcurrentEncodeUnderLoad drives the process-global encoder pool from many goroutines on
// DISTINCT documents, which is the shape a server takes when several rooms serialise at once. The
// pool is shared across all of them, so this is where retention and reset behaviour meet real
// contention.
func TestConcurrentEncodeUnderLoad(t *testing.T) {
	readers, rounds := concurrentReaders(), concurrentReadRounds()/2
	var wg sync.WaitGroup
	errs := make(chan string, readers)

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			doc := buildReadFixture()
			want := readAll(doc)
			for i := 0; i < rounds; i++ {
				enc, err := EncodeStateAsUpdateV2(doc, nil)
				if err != nil {
					errs <- fmt.Sprintf("worker %d: encode: %v", r, err)
					return
				}
				fresh := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(Number(500+r)))
				_ = ApplyUpdateV2(fresh, enc, nil)
				if got := readAll(fresh); got != want {
					errs <- fmt.Sprintf("worker %d iter %d: encode/apply round-trip diverged\n"+
						" want %s\n got  %s", r, i, want, got)
					return
				}
			}
		}(r)
	}
	wg.Wait()
	close(errs)
	n := 0
	for e := range errs {
		n++
		if n <= 3 {
			t.Error(e)
		}
	}
	t.Logf("CONCURRENT_ENCODE workers=%d rounds=%d failures=%d", readers, rounds, n)
}
