//go:build structstoreoracle

package crdt

import "testing"

func TestStructStoreOracleRejectsProductionPositionDrift(t *testing.T) {
	list := newClientStructList(3)
	first := list.Append(&abstractStructBase{id: GenID(7, 0), length: 1})
	list.Append(&abstractStructBase{id: GenID(7, 1), length: 1})
	list.Append(&abstractStructBase{id: GenID(7, 2), length: 1})

	defer func() {
		if recovered := recover(); recovered == nil {
			t.Fatal("shadow oracle accepted a struct inserted at the wrong production position")
		}
	}()
	// Clock 3 belongs at the end. Supplying the first cursor deliberately makes
	// the production operation place it after clock 0; the shadow independently
	// derives the end position from the clock and must reject the disagreement.
	list.InsertAfter(first, &abstractStructBase{id: GenID(7, 3), length: 1})
}
