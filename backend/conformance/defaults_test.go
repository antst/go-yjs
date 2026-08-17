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

func TestClusterContractFixture(t *testing.T) {
	conformance.Cluster(t, func() conformance.ClusterHarness {
		coordinator := backendtest.NewCoordinator()
		return conformance.ClusterHarness{Coordinator: coordinator, Advance: coordinator.Advance}
	})
}
