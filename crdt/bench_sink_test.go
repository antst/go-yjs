package crdt

import (
	"runtime"
	"testing"
)

// Typed sinks that keep a benchmark's result live so the compiler cannot delete
// the work that produced it.
//
// The benchmarks previously wrote `_ = f.GetFirstChild()`. That is safe only
// while the callee refuses to inline: GetFirstChild costs 116 against a budget of
// 80, so the call survives and the measurement is honest. The moment anyone makes
// such a method inlineable -- exactly what an optimization attempt on these rows
// would try -- the assignment becomes dead and the compiler may erase the body,
// manufacturing an enormous improvement out of nothing. That is the worst failure
// mode available to a performance harness: it fires precisely when someone is
// optimizing, reports success, and the change ships.
//
// This is not hypothetical here. BenchmarkArrayRange once reported 0.98 ns/op for
// walking 2,000 items, which was not a fast traversal but no traversal, and the
// existing `var benchSink int` below the XML fixtures exists because of it.
//
// COST, measured rather than assumed, on a 2.27 ns operation:
//
//	_ = expr                       2.266-2.279 ns   free, and UNSOUND
//	package-level sink             2.820-2.938 ns   +0.6 ns, sound
//	local var + runtime.KeepAlive  2.127-2.476 ns   free, but weaker
//
// The package-level store is chosen despite being the most expensive. The local
// form is free because it is also weaker: KeepAlive keeps the final value live
// but does not make each iteration observable, so a pure inlined callee could be
// hoisted out of the loop entirely -- which is precisely the failure being
// guarded against. The store's cost is a GC write barrier for pointer-bearing
// results, and that write barrier is exactly what makes the iteration an
// observable side effect the compiler may not remove.
//
// So sub-10 ns rows now carry roughly 0.6 ns of harness overhead. That is
// deliberate and must be remembered when comparing them against yrs, whose
// black_box is nearly free. A row of this speed should be read as an upper bound
// on our cost, not a precise figure -- and a harness that is honest and slightly
// pessimistic beats one that is precise until the moment somebody optimizes it.
//
// Interface-typed sinks are used only where the expression already yields an
// interface, so nothing is boxed and the allocs/op column stays truthful.
// benchReleaseSinks drops everything the sinks are holding and collects, so a
// benchmark cannot leave its result graph live for the ones that follow.
//
// This is the other half of the sink pattern and it was missing. A package-level
// sink defeats dead-code elimination precisely BECAUSE the value stays reachable
// after the loop — which means the last iteration's result is still live when the
// next benchmark starts, and every allocation-heavy benchmark after it pays GC
// scan cost on that graph.
//
// Measured: MergeDeleteSets leaves its sparse-8192 result behind (~1.8 MB across
// ~24,700 objects), and an unrelated MapSet run later in the same process goes
// from 422k to 588k ns/op — 39% slower, with byte-identical allocation counts.
// That is not MapSet getting slower; it is MapSet being measured through someone
// else's live heap. It invalidated two full four-way comparison runs before it
// was found, because the effect is invisible to isolated canaries: a fresh
// process has nothing retained.
//
// A residual remains and is NOT a defect in this helper: a benchmark running
// after any others is still about 6% slower than one running first (measured
// 421,622 vs 445,915 ns), because the process heap has grown. debug.FreeOSMemory
// was tried and made no measurable difference (444,904 ns), so it is not used.
// That residual applies to every Go row equally, but the reference harnesses run
// in separate processes, so it biases four-way ratios slightly against us.
//
// Clearing in Cleanup does not reintroduce the DCE hazard. The in-loop stores
// still happen and the compiler cannot prove nothing observes them; only the
// retention after the benchmark is removed.
func benchReleaseSinks(b *testing.B) {
	b.Cleanup(func() {
		benchSinkString = ""
		benchSinkBool = false
		benchSinkErr = nil
		benchSinkAny = nil
		benchSinkObj = Object{}
		benchSinkStrs = nil
		benchSinkOps = nil
		benchSinkRelPos = nil
		benchSinkAbsPos = nil
		runtime.GC()
	})
}

var (
	benchSinkString string
	benchSinkBool   bool
	benchSinkErr    error
	benchSinkAny    any
	benchSinkObj    Object
	benchSinkStrs   []string
	benchSinkOps    []EventOperator
	benchSinkRelPos *RelativePosition
	benchSinkAbsPos *AbsolutePosition
)
