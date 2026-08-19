package crdt

import "testing"

// Upper allocation budgets for the three mutation workloads.
//
// Ceilings, not expected values: exceeding one fails, coming in under one does
// not. An allocation improvement is not a defect.
//
// Measured through THIS test — the same workloads measured from a standalone
// harness report a different MapSet count, because subtest machinery shifts it
// by one. A budget has to be measured with the code that enforces it.
//
// This catches sustained allocation drift across workloads of 2000-10000
// operations, so a per-operation regression moves these by thousands. It will
// not catch a handful of extra allocations in total.
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
	// Warming all three before measuring any is load-bearing: without it MapSet
	// reports a different count alone than it does after the other workloads, so
	// the budgets would depend on subtest order.
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
