package oracle_test

import (
	"testing"

	"github.com/antst/go-yjs/crdt"
	"github.com/antst/go-yjs/internal/oracle"
)

// T006 — coverage derivation. This test is `package oracle_test` deliberately: `internal/oracle`
// must NOT import the root package (the in-package differential tests are `package y_crdt`, so
// that would be an import cycle in the test binary — research R7). The CALLER passes instances
// and the harness reflects over their method sets, so the dependency points one way only.

func TestOperationsAreDerivedFromTheTypeNotAList(t *testing.T) {
	doc := crdt.NewDoc("g", crdt.WithGC(false), crdt.WithClientID(1))
	rep := oracle.NewCoverageReport()
	rep.DeriveFrom("text", doc.GetText("t"))

	ops := rep.Operations("text")
	if len(ops) == 0 {
		t.Fatal("no operations derived — the report must come from the type's method set")
	}
	// Content operations are included; a newly added public method appears here without anyone
	// editing a list, which is FR-005a's whole anti-staleness property.
	for _, want := range []string{"Insert", "Delete", "Format", "ToDelta", "ToString"} {
		if !hasOp(ops, want) {
			t.Errorf("operation %q missing — mutating AND read/serialization ops are in scope "+
				"(two known defects sit in read paths)", want)
		}
	}
}

// The inclusion predicate must be decidable in code, not argued: lifecycle, observation
// registration and the internal content family are not user-facing content operations.
func TestInclusionPredicateExcludesNonContentOperations(t *testing.T) {
	doc := crdt.NewDoc("g", crdt.WithGC(false), crdt.WithClientID(1))
	rep := oracle.NewCoverageReport()
	rep.DeriveFrom("text", doc.GetText("t"))

	ops := rep.Operations("text")
	for _, excluded := range []string{"Integrate", "CallObserver", "Copy", "Clone", "Write"} {
		if hasOp(ops, excluded) {
			t.Errorf("operation %q included; lifecycle/observation/content-internal methods are "+
				"not user-facing content operations", excluded)
		}
	}
}

func TestUnexercisedOperationFailsTheReport(t *testing.T) {
	doc := crdt.NewDoc("g", crdt.WithGC(false), crdt.WithClientID(1))
	rep := oracle.NewCoverageReport()
	rep.DeriveFrom("text", doc.GetText("t"))
	rep.MarkExercised("text", "Insert")

	missing := rep.Missing()
	if len(missing) == 0 {
		t.Fatal("marking one operation exercised should leave the rest missing")
	}
	if err := rep.Validate(); err == nil {
		t.Fatal("Validate must fail while any operation is unexercised (SC-003)")
	}
}

func TestFullyExercisedReportPasses(t *testing.T) {
	doc := crdt.NewDoc("g", crdt.WithGC(false), crdt.WithClientID(1))
	rep := oracle.NewCoverageReport()
	rep.DeriveFrom("text", doc.GetText("t"))
	for _, op := range rep.Operations("text") {
		rep.MarkExercised("text", op)
	}
	if err := rep.Validate(); err != nil {
		t.Fatalf("fully exercised report failed: %v", err)
	}
}

func hasOp(ops []string, name string) bool {
	for _, o := range ops {
		if o == name {
			return true
		}
	}
	return false
}
