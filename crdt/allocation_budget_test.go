package crdt

import "testing"

// Upper allocation budgets for the three mutation workloads.
//
// These are measured ceilings, not targets: exceeding one fails, coming in under
// one does not. An improvement is not a defect, and failing CI to force
// bookkeeping would train people to edit the number rather than read it.
//
// Measured THROUGH THIS TEST, which matters: measuring the same workloads from a
// standalone harness gave MapSet 1872, because subtest machinery and the
// harness's own output shift the count by one. A budget has to be measured with
// the code that enforces it.
//
// 15 samples per workload with the pinned toolchain — 10 processes running all
// three, 5 running each alone. Text and Array were identical every time; MapSet
// was 1873 in every full run and 1872 once out of five alone. That residual
// variation is why these are ceilings rather than expected values: an upper
// budget absorbs it without needing a tolerance, and an allocation improvement
// is not a defect.
//
// The warm-up below is load-bearing. Without it MapSet reports 1871 alone and
// 1873 after the other workloads, so the number would depend on subtest order.
//
// This catches sustained allocation drift, which this library has actually
// shipped — a 43x regression, and four individually small ones that accumulated
// past an immediate-parent comparison. It will not catch a handful of extra
// allocations in total; the workloads are 2000 to 10000 operations each, so a
// per-operation regression moves these numbers by thousands.
//
// If one fails, diagnose before editing. Raise a ceiling only for a change whose
// extra allocations are understood and intended.
const (
	budgetTextAppendLarge       = 30
	budgetArrayInsertSequential = 1773
	budgetMapSet                = 1873
)

func TestMutationWorkloadAllocationBudgets(t *testing.T) {
	// The workloads are the ones the benchmarks run, not copies: a second copy
	// would let this budget certify code the benchmarks no longer execute.
	//
	// Every workload runs once before any is measured, so each measurement
	// starts from the same process state and the numbers above do not depend on
	// subtest order or on running one subtest with -run.
	warm := []func(){
		func() { workloadTextAppend(perfLarge) },
		workloadArrayInsertSequential,
		workloadMapSet,
	}
	for _, run := range warm {
		run()
	}

	for _, workload := range []struct {
		name   string
		budget float64
		run    func()
	}{
		{"TextAppendLarge", budgetTextAppendLarge, func() { workloadTextAppend(perfLarge) }},
		{"ArrayInsertSequential", budgetArrayInsertSequential, workloadArrayInsertSequential},
		{"MapSet", budgetMapSet, workloadMapSet},
	} {
		t.Run(workload.name, func(t *testing.T) {
			if got := testing.AllocsPerRun(3, workload.run); got > workload.budget {
				t.Errorf("%s allocates %.0f times per run, budget %.0f (%+.0f). "+
					"Diagnose before raising the budget.",
					workload.name, got, workload.budget, got-workload.budget)
			}
		})
	}
}
