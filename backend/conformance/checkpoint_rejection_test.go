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

const violationEnv = "GO_YJS_CHECKPOINT_VIOLATION"

// TestCheckpointSuiteRejectsViolations proves the suite CATCHES a broken store.
//
// Without this, CheckpointPersistence would only be known to accept the one
// implementation written alongside it, which says nothing about what it would
// reject. Each violation is a plausible mistake rather than an absurd one:
// forgetting to copy a borrowed slice, handing out an internal buffer,
// returning a zero value instead of ErrNotFound.
//
// It runs each case in a CHILD test process. The suite reports failures through
// *testing.T, and a Fatalf ends the calling goroutine, so a broken store cannot
// be checked in-process without either the failure propagating or the test
// binary needing to interpret its own partial state. A subprocess makes the
// question a plain exit code.
func TestCheckpointSuiteRejectsViolations(t *testing.T) {
	if os.Getenv(violationEnv) != "" {
		runViolationFixture(t)
		return
	}
	if len(backendtest.AllCheckpointViolations) == 0 {
		t.Fatal("no violations declared; this test would pass vacuously")
	}
	for _, violation := range backendtest.AllCheckpointViolations {
		t.Run(string(violation), func(t *testing.T) {
			command := exec.Command(os.Args[0], "-test.run=TestCheckpointSuiteRejectsViolations", "-test.v")
			command.Env = append(os.Environ(), violationEnv+"="+string(violation))
			output, err := command.CombinedOutput()
			if err == nil {
				t.Fatalf("the suite ACCEPTED a store violating %q; it is not checking that rule\n%s",
					violation, output)
			}
			if !strings.Contains(string(output), "FAIL") {
				t.Fatalf("child exited non-zero without a test failure, so the rejection is not the suite's doing:\n%s", output)
			}
		})
	}
}

func runViolationFixture(t *testing.T) {
	violation := backendtest.CheckpointViolation(os.Getenv(violationEnv))
	// The fence breach is only observable against a fenced store; the rest are
	// observable unfenced, and AcceptAnyFence is checked in both profiles.
	if violation == backendtest.AcceptAnyFence {
		conformance.CheckpointPersistenceFencing(t, func() persistence.CheckpointStore {
			return backendtest.NewBrokenCheckpointStore(violation, persistence.Fenced)
		})
		return
	}
	conformance.CheckpointPersistence(t, func() persistence.CheckpointStore {
		return backendtest.NewBrokenCheckpointStore(violation, persistence.Unfenced)
	})
}
