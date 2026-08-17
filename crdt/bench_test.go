package crdt

import "testing"

// BenchmarkDocOps is a representative, pinned document-ops workload (text insert / format / delete
// + apply-delta with multi-key attributes, then encode). It establishes the baseline for the
// dedicated performance phase (removing reflect/copystructure from the hot attribute paths). The
// US2 value-rep consolidation is a behavior-preserving refactor (one method call + one predicate),
// so it holds this line rather than moving it (SC-007).
func BenchmarkDocOps(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		// Build attrs fresh each iteration: insertText's negation pre-pass mutates the passed
		// attribute Object, so sharing one across iterations would poison the measured workload.
		fmtAttr := newObject()
		fmtAttr.Set("bold", true)
		fmtAttr.Set("color", "red")
		delta := []EventOperator{
			NewRetainDeltaOp(5, fmtAttr),
			NewTextDeltaOp("QZ", fmtAttr),
			NewRetainDeltaOp(8, Object{}),
			NewTextDeltaOp("Y", Object{}),
		}
		doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
		tx := doc.GetText("t")
		tx.Insert(0, "0123456789012345678901234567890123456789", Object{}) // 40 chars
		for j := 0; j < 30; j++ {
			tx.Format((j*3)%35, 4, fmtAttr)
		}
		tx.Delete(5, 10)
		tx.Insert(3, "xyz", Object{})
		tx.ApplyDelta(delta, true)
		if _, err := EncodeStateAsUpdate(doc, nil); err != nil {
			b.Fatal(err)
		}
	}
}
