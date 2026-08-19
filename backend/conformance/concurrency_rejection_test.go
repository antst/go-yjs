package conformance_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/antst/go-yjs/backend/conformance"
	"github.com/antst/go-yjs/backend/internal/backendtest"
	"github.com/antst/go-yjs/backend/persistence"
)

const concurrencyViolationEnv = "GO_YJS_CONCURRENCY_VIOLATION"

// concurrencyMarkers names, for each planted defect, the text the assertion that
// is supposed to catch it prints.
//
// Requiring only "FAIL" would repeat the defect this whole exercise is about: a
// child that failed for ANY reason would be counted as proof the rule is
// enforced, so a store broken in two ways could certify a rule the suite never
// checks. These run against the canonical entrypoints, which contain dozens of
// other assertions — several of which a broken store may also trip — so the
// marker is what distinguishes "the suite caught this" from "something failed".
var concurrencyMarkers = map[backendtest.ConcurrencyViolation]string{
	backendtest.DuplicateRevisionUnderRace:     "revisions are not unique under concurrency",
	backendtest.LostAppendUnderRace:            "an acknowledged append was lost under concurrency",
	backendtest.CompactionDiscardsRacingAppend: "and is not in the tail; a compaction discarded a concurrent append",
	// Distinct from the marker above on purpose: this defect also drops the
	// tail, so a shared marker would let either assertion vouch for both rules.
	backendtest.CompactionAdvancesCheckpointRevision: "claiming to cover it",
	backendtest.FenceValidatedThenReleased:           "a superseded owner wrote after its successor",
	backendtest.TornCheckpointWrite:                  "the checkpoint was torn across two saves",
	backendtest.LoadFailsUnderConcurrentSaves:        "a load must not fail because a save is in flight",
	backendtest.CheckpointFenceValidatedThenReleased: "a superseded owner wrote after its successor",
}

// TestConcurrencySuiteRejectsViolations proves the shipped suites CATCH a store
// that is correct alone and wrong when callers overlap.
//
// This matters more than for the sequential rules. A concurrency test that never
// produces the damaging interleaving passes against a broken store and reports
// the rule as enforced — the failure mode is a green result, not a red one. The
// only way to know the suites check anything is to hand them stores that break
// each rule and require them to fail AT THE RIGHT ASSERTION.
//
// Each planted defect drives its own interleaving through an internal
// rendezvous rather than hoping the scheduler produces it, so a case reported as
// accepted means the suite missed a violation that definitely occurred.
//
// Runs in a CHILD test process: a Fatalf inside a conformance suite ends the
// calling goroutine, so a broken store cannot be judged in-process without the
// failure propagating into this test.
func TestConcurrencySuiteRejectsViolations(t *testing.T) {
	if os.Getenv(concurrencyViolationEnv) != "" {
		runConcurrencyViolationFixture(t)
		return
	}
	groups := [][]backendtest.ConcurrencyViolation{
		backendtest.AllStoreConcurrencyViolations,
		backendtest.AllCompactionConcurrencyViolations,
		backendtest.AllFencedConcurrencyViolations,
		backendtest.AllCheckpointConcurrencyViolations,
		backendtest.AllFencedCheckpointConcurrencyViolations,
	}
	var cases []backendtest.ConcurrencyViolation
	for _, group := range groups {
		if len(group) == 0 {
			t.Fatal("a violation list is empty; this test would pass vacuously")
		}
		cases = append(cases, group...)
	}
	for _, violation := range cases {
		marker, named := concurrencyMarkers[violation]
		if !named {
			t.Fatalf("violation %q has no expected failure marker; add one or it is only known to fail somehow", violation)
		}
		t.Run(string(violation), func(t *testing.T) {
			command := exec.Command(os.Args[0], "-test.run=TestConcurrencySuiteRejectsViolations", "-test.v")
			command.Env = append(os.Environ(), concurrencyViolationEnv+"="+string(violation))
			output, err := command.CombinedOutput()
			if err == nil {
				t.Fatalf("the suite ACCEPTED a store violating %q under concurrency; it is not checking that rule\n%s",
					violation, output)
			}
			if !strings.Contains(string(output), marker) {
				t.Fatalf("the suite failed, but not at the assertion for %q (wanted %q). Failing for another reason is not proof the rule is checked:\n%s",
					violation, marker, output)
			}
		})
	}
}

func runConcurrencyViolationFixture(t *testing.T) {
	violation := backendtest.ConcurrencyViolation(os.Getenv(concurrencyViolationEnv))
	// Deliberately the CANONICAL entrypoints, not the private concurrency
	// helpers: what has to be proven is that the suite a consumer actually runs
	// rejects these stores.
	for _, compaction := range backendtest.AllCompactionConcurrencyViolations {
		if violation == compaction {
			conformance.PersistenceCompaction(t, func() persistence.CompactingStore {
				return backendtest.NewBrokenConcurrentStore(violation, persistence.Unfenced)
			})
			return
		}
	}
	for _, fenced := range backendtest.AllFencedConcurrencyViolations {
		if violation == fenced {
			conformance.PersistenceFencing(t, func() persistence.Store {
				return backendtest.NewBrokenConcurrentStore(violation, persistence.Fenced)
			})
			return
		}
	}
	for _, fenced := range backendtest.AllFencedCheckpointConcurrencyViolations {
		if violation == fenced {
			conformance.CheckpointPersistenceFencing(t, func() persistence.CheckpointStore {
				return backendtest.NewBrokenConcurrentCheckpointStore(violation, persistence.Fenced)
			})
			return
		}
	}
	for _, checkpoint := range backendtest.AllCheckpointConcurrencyViolations {
		if violation == checkpoint {
			conformance.CheckpointPersistence(t, func() persistence.CheckpointStore {
				return backendtest.NewBrokenConcurrentCheckpointStore(violation, persistence.Unfenced)
			})
			return
		}
	}
	conformance.Persistence(t, func() persistence.Store {
		return backendtest.NewBrokenConcurrentStore(violation, persistence.Unfenced)
	})
}
