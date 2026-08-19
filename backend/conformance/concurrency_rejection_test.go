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

// TestConcurrencySuiteRejectsViolations proves the concurrency suites CATCH a
// store that is correct alone and wrong when callers overlap.
//
// This matters more here than for the sequential suites. A concurrency test that
// never actually produces the damaging interleaving passes against a broken
// store and reports the rule as enforced — the failure mode is a green result,
// not a red one. The only way to know the suites check anything is to hand them
// stores that break each rule and require them to fail.
//
// Each planted defect drives its own interleaving through an internal
// rendezvous rather than hoping the scheduler produces it, so a case that
// reports "accepted" means the suite missed a violation that definitely
// occurred, not that the race failed to happen this time.
//
// Runs in a CHILD test process for the same reason as the checkpoint rejection
// suite: a Fatalf inside a conformance suite ends the calling goroutine, so a
// broken store cannot be judged in-process without the failure propagating.
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
	}
	var cases []backendtest.ConcurrencyViolation
	for _, group := range groups {
		if len(group) == 0 {
			t.Fatal("a violation list is empty; this test would pass vacuously")
		}
		cases = append(cases, group...)
	}
	for _, violation := range cases {
		t.Run(string(violation), func(t *testing.T) {
			command := exec.Command(os.Args[0], "-test.run=TestConcurrencySuiteRejectsViolations", "-test.v")
			command.Env = append(os.Environ(), concurrencyViolationEnv+"="+string(violation))
			output, err := command.CombinedOutput()
			if err == nil {
				t.Fatalf("the suite ACCEPTED a store violating %q under concurrency; it is not checking that rule\n%s",
					violation, output)
			}
			if !strings.Contains(string(output), "FAIL") {
				t.Fatalf("child exited non-zero without a test failure, so the rejection is not the suite's doing:\n%s", output)
			}
		})
	}
}

func runConcurrencyViolationFixture(t *testing.T) {
	violation := backendtest.ConcurrencyViolation(os.Getenv(concurrencyViolationEnv))
	for _, compaction := range backendtest.AllCompactionConcurrencyViolations {
		if violation == compaction {
			conformance.PersistenceCompactionConcurrency(t, func() persistence.CompactingStore {
				return backendtest.NewBrokenConcurrentStore(violation, persistence.Unfenced)
			})
			return
		}
	}
	for _, fenced := range backendtest.AllFencedConcurrencyViolations {
		if violation == fenced {
			conformance.PersistenceFencingConcurrency(t, func() persistence.Store {
				return backendtest.NewBrokenConcurrentStore(violation, persistence.Fenced)
			})
			return
		}
	}
	for _, checkpoint := range backendtest.AllCheckpointConcurrencyViolations {
		if violation == checkpoint {
			conformance.CheckpointPersistenceConcurrency(t, func() persistence.CheckpointStore {
				return backendtest.NewBrokenConcurrentCheckpointStore(violation, persistence.Unfenced)
			})
			return
		}
	}
	conformance.PersistenceConcurrency(t, func() persistence.Store {
		return backendtest.NewBrokenConcurrentStore(violation, persistence.Unfenced)
	})
}
