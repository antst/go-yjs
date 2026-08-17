package crdt

import (
	"fmt"
	"sync"
	"testing"
)

// deltaForInternalRead BORROWS the canonical cached ops instead of cloning them. That is safe only
// while every internal consumer treats them as read-only, and the failure mode is the worst kind
// available here: a renderer that mutated a borrowed op would corrupt the CACHE, so the next public
// ToDelta — a completely different call, on behalf of a completely different caller — would return
// wrong data and keep doing so until something invalidated it.
//
// Nothing else can see that. The differential never calls ToDelta on a live document mid-render,
// and the renderer's own output would look correct at the moment it was produced.

func xmlFixture(t *testing.T, doc *Doc, spans int) *YXmlText {
	t.Helper()
	xt := NewYXmlText()
	f := doc.GetXmlFragment("x")
	f.Insert(0, ArrayAny{xt})
	for i := 0; i < spans; i++ {
		xt.Insert(xt.Length(), fmt.Sprintf("s%d", i%10), Object{})
	}
	// Alternating marks, including an OBJECT-valued one, whose inner keys have their own ordering
	// rule in the renderer.
	for i := 0; i < spans; i++ {
		a := newObject()
		switch i % 4 {
		case 0:
			a.Set("bold", true)
		case 1:
			a.Set("italic", true)
		case 2:
			// Multi-wrapper: two marks over one span means two nested elements, whose ORDER the
			// renderer decides.
			a.Set("bold", true)
			a.Set("underline", true)
		default:
			inner := newObject()
			inner.Set("zeta", 1)
			inner.Set("alpha", 2)
			inner.Set("mid", "m")
			a.Set("link", inner)
		}
		at := Number((i * 2) % maxInt(1, int(xt.Length())-2))
		xt.Format(at, 2, a)
	}
	return xt
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// An internal render must not disturb what a later public ToDelta returns — values or ORDER.
func TestInternalRenderDoesNotCorruptTheDeltaCache(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	xt := xmlFixture(t, doc, 40)

	before := deltaSemantic(xt.ToDelta(nil, nil, nil))
	beforeRender := xt.ToString()

	// Many internal reads, each borrowing the cached ops.
	for i := 0; i < 200; i++ {
		if got := xt.ToString(); got != beforeRender {
			t.Fatalf("render %d changed: the renderer is not deterministic over a quiescent document\n"+
				" first %q\n now   %q", i, beforeRender, got)
		}
	}

	if after := deltaSemantic(xt.ToDelta(nil, nil, nil)); after != before {
		t.Fatalf("public ToDelta changed after internal renders borrowed the cached ops\n"+
			" before %s\n after  %s", before, after)
	}
}

// The same document rendered with the read cache DISABLED must produce identical output. The
// borrowed path only exists when a cache is present, so this is the check that the optimisation
// did not change semantics rather than only speed.
func TestInternalRenderMatchesCacheDisabled(t *testing.T) {
	for seed := 0; seed < 40; seed++ {
		cached := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
		plain := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1), WithReadCache(false))
		a := xmlFixture(t, cached, 6+seed%12)
		b := xmlFixture(t, plain, 6+seed%12)

		if x, y := a.ToString(), b.ToString(); x != y {
			t.Fatalf("seed %d: cached and cache-disabled renders differ\n cached %q\n plain  %q", seed, x, y)
		}
		if x, y := deltaSemantic(a.ToDelta(nil, nil, nil)), deltaSemantic(b.ToDelta(nil, nil, nil)); x != y {
			t.Fatalf("seed %d: deltas differ\n cached %s\n plain  %s", seed, x, y)
		}
	}
}

// Concurrent internal reads of a quiescent document borrow the same cached ops. Under the stated
// contract this must be safe and must agree; run with -race.
func TestConcurrentInternalRendersAgree(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	xt := xmlFixture(t, doc, 30)
	want := xt.ToString()
	wantDelta := deltaSemantic(xt.ToDelta(nil, nil, nil))

	var wg sync.WaitGroup
	var mu sync.Mutex
	var bad []string
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				if got := xt.ToString(); got != want {
					mu.Lock()
					bad = append(bad, "render: "+got)
					mu.Unlock()
					return
				}
				if got := deltaSemantic(xt.ToDelta(nil, nil, nil)); got != wantDelta {
					mu.Lock()
					bad = append(bad, "delta: "+got)
					mu.Unlock()
					return
				}
			}
		}()
	}
	wg.Wait()
	if len(bad) > 0 {
		t.Fatalf("%d concurrent readers disagreed; first: %s", len(bad), bad[0])
	}
}
