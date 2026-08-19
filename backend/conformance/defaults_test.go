package conformance_test

import (
	"testing"

	"github.com/antst/go-yjs/backend/conformance"
	"github.com/antst/go-yjs/backend/hub"
	"github.com/antst/go-yjs/backend/internal/backendtest"
	"github.com/antst/go-yjs/backend/memory"
	"github.com/antst/go-yjs/backend/persistence"
)

func TestInProcessRegistryConformance(t *testing.T) {
	conformance.Memory(t, func() memory.Registry { return memory.NewRegistry() })
}

func TestInProcessHubConformance(t *testing.T) {
	conformance.Hub(t, func() hub.Hub { return hub.NewInProcess() })
}

func TestPersistenceContractFixture(t *testing.T) {
	conformance.Persistence(t, func() persistence.Store { return backendtest.NewStore() })
	conformance.PersistenceCompaction(t, func() persistence.CompactingStore { return backendtest.NewStore() })
	conformance.PersistenceFencing(t, func() persistence.Store { return backendtest.NewFencedStore() })
	conformance.PersistenceFenceUpgrade(t, func() (persistence.Store, persistence.Store) {
		return backendtest.NewFenceUpgradePair()
	})
}

func TestCheckpointPersistenceContractFixture(t *testing.T) {
	conformance.CheckpointPersistence(t, func() persistence.CheckpointStore {
		return backendtest.NewCheckpointStore()
	})
	conformance.CheckpointPersistenceFencing(t, func() persistence.CheckpointStore {
		return backendtest.NewFencedCheckpointStore()
	})
}

func TestPersistenceConcurrencyContractFixture(t *testing.T) {
	conformance.PersistenceConcurrency(t, func() persistence.Store { return backendtest.NewStore() })
	conformance.PersistenceCompactionConcurrency(t, func() persistence.CompactingStore {
		return backendtest.NewStore()
	})
	conformance.PersistenceFencingConcurrency(t, func() persistence.Store { return backendtest.NewFencedStore() })
	conformance.CheckpointPersistenceConcurrency(t, func() persistence.CheckpointStore {
		return backendtest.NewCheckpointStore()
	})
	conformance.CheckpointPersistenceConcurrency(t, func() persistence.CheckpointStore {
		return backendtest.NewFencedCheckpointStore()
	})
}

// A store that supports ONE codec is a conforming store, and every checkpoint
// suite has to accept it. Nothing else in this repository exercises that path,
// so before this the ErrUnsupportedEncoding skip was carried by the suites
// without a single implementation to prove it worked — and the deletion suites,
// which hardcoded V1, failed such a store by construction.
func TestBareUpdateCheckpointContractFixture(t *testing.T) {
	for _, codec := range []persistence.CheckpointEncoding{persistence.EncodingV1, persistence.EncodingV2} {
		t.Run(map[persistence.CheckpointEncoding]string{
			persistence.EncodingV1: "v1-only", persistence.EncodingV2: "v2-only",
		}[codec], func(t *testing.T) {
			conformance.CheckpointPersistence(t, func() persistence.CheckpointStore {
				return backendtest.NewBareUpdateCheckpointStore(codec, persistence.Unfenced)
			})
			conformance.CheckpointPersistenceFencing(t, func() persistence.CheckpointStore {
				return backendtest.NewBareUpdateCheckpointStore(codec, persistence.Fenced)
			})
			conformance.CheckpointPersistenceDeletion(t, func() persistence.DeletingCheckpointStore {
				return backendtest.NewBareUpdateCheckpointStore(codec, persistence.Unfenced)
			})
			conformance.CheckpointPersistenceDeletionFencing(t, func() persistence.DeletingCheckpointStore {
				return backendtest.NewBareUpdateCheckpointStore(codec, persistence.Fenced)
			})
			conformance.CheckpointPersistenceConcurrency(t, func() persistence.CheckpointStore {
				return backendtest.NewBareUpdateCheckpointStore(codec, persistence.Unfenced)
			})
		})
	}
}

func TestPersistenceDeletionContractFixture(t *testing.T) {
	conformance.PersistenceDeletion(t, func() persistence.DeletingStore { return backendtest.NewStore() })
	conformance.PersistenceDeletionFencing(t, func() persistence.DeletingStore { return backendtest.NewFencedStore() })
	conformance.CheckpointPersistenceDeletion(t, func() persistence.DeletingCheckpointStore {
		return backendtest.NewCheckpointStore()
	})
	conformance.CheckpointPersistenceDeletionFencing(t, func() persistence.DeletingCheckpointStore {
		return backendtest.NewFencedCheckpointStore()
	})
}

func TestClusterContractFixture(t *testing.T) {
	conformance.Cluster(t, func() conformance.ClusterHarness {
		coordinator := backendtest.NewCoordinator()
		return conformance.ClusterHarness{Coordinator: coordinator, Advance: coordinator.Advance}
	})
}
