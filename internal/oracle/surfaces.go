package oracle

// Default returns the canonical surface registry — the single source of truth for "every surface".
//
// data-model.md mirrors this list; this is the code that enforces it. Any check phrased "for every
// surface" iterates Default() rather than a local list, because a surface covered in one place and
// forgotten in another is how the feature-003 gate stayed hollow.
//
// Tiers are keyed PER DIRECTION. A cell absent from the fast tier is rejected at registration, so
// a surface cannot be registered for direction A only and quietly leave direction B nightly-only.
//
// A surface is registered when its generator exists, NOT before: the entrypoint derives its run
// list from here, so registering a surface with no generator would make the gate fail on work not
// yet done. The four remaining canonical surfaces are added by the phases that build them —
// `undo` (T027) and `relpos`/`sync`/`awareness` (T042). CanonicalSurfaces below is the full target
// list, so the gap between target and registered is visible rather than implied.

// CanonicalSurfaces is the full 13-surface target from data-model.md. Registration catches up to
// it as each surface's generator lands; the difference is reported, never silently tolerated.
var CanonicalSurfaces = []string{
	"applyDelta", "array", "awareness", "gc", "map", "relpos", "snapshot",
	"subdoc", "sync", "text", "undo", "update", "xml",
}

// PendingDirections reports realized-direction gaps. Direction B is the feature's FR-002 target;
// until its generators exist, no surface may claim it.
func PendingDirections() []string {
	var out []string
	r := Default()
	for _, n := range r.Names() {
		if !r.RealizesDirection(n, DirB) {
			out = append(out, n+":B")
		}
	}
	return out
}

// Pending returns canonical surfaces not yet registered, so "every surface" claims stay honest
// while the feature is mid-flight.
func Pending() []string {
	have := map[string]bool{}
	for _, n := range Default().Names() {
		have[n] = true
	}
	var out []string
	for _, n := range CanonicalSurfaces {
		if !have[n] {
			out = append(out, n)
		}
	}
	return out
}

func Default() *Registry {
	r := NewRegistry()

	// Every cell runs in every tier; the tiers differ in seed VOLUME, not in membership. That is
	// the whole point of FR-022: seed counts per cell are negotiable, presence is not.
	all := TierSet{TierFast, TierFull, TierScale, TierUltimate}
	aOnly := func() map[Direction]TierSet {
		return map[Direction]TierSet{DirA: all}
	}
	// Direction B is realized by TestDirBDiff, which builds documents NATIVELY with this library
	// and has the reference decode and re-encode them. It covers the shared types plus the update
	// path, so those surfaces declare it; the rest stay direction-A only until their own
	// direction-B coverage exists. Declaring DirB where nothing exercises it would claim coverage
	// the harness cannot deliver.
	both := func() map[Direction]TierSet {
		return map[Direction]TierSet{DirA: all, DirB: all}
	}

	// Fault applicability is declared, never inferred. corrupt-expectation applies everywhere: any
	// comparable artefact can be perturbed. Order-sensitive kinds are declared only where order is
	// observable — a reorder on a commutative multiset legitimately still matches, so claiming it
	// applicable would make "100% detected" ill-defined.
	ordered := []FaultKind{FaultCorruptExpectation, FaultDropOp, FaultDupOp, FaultReorderOp}
	commutative := []FaultKind{FaultCorruptExpectation, FaultDropOp, FaultDupOp}
	attributed := []FaultKind{FaultCorruptExpectation, FaultDropOp, FaultDupOp, FaultReorderOp, FaultPermuteAttr}

	for _, s := range []Surface{
		// Shared types — sequence order is observable, so reorder applies.
		{Name: "text", Directions: []Direction{DirA, DirB}, Faults: attributed, Tiers: both()},
		{Name: "array", Directions: []Direction{DirA, DirB}, Faults: ordered, Tiers: both()},
		// Y.Map is a key->value store: setting distinct keys is commutative, so reorder is NOT
		// applicable and must not be counted as a detection.
		{Name: "map", Directions: []Direction{DirA, DirB}, Faults: commutative, Tiers: both()},
		{Name: "xml", Directions: []Direction{DirA, DirB}, Faults: attributed, Tiers: both()},
		{Name: "applyDelta", Directions: []Direction{DirA, DirB}, Faults: attributed, Tiers: both()},

		// Update/apply paths and the convergence invariant.
		{Name: "update", Directions: []Direction{DirA, DirB}, Faults: ordered, Tiers: both()},

		// Undo/redo — restoration order is observable (FR-001a), so reorder applies. Registered
		// only now that TestUndoDiff is green: registering it while red would have claimed
		// coverage the harness could not deliver.
		{Name: "undo", Directions: []Direction{DirA}, Faults: ordered, Tiers: aOnly()},

		// Relative positions — a wire format with zero coverage before this feature. Order of
		// encoded fields is observable, so reorder applies.
		{Name: "relpos", Directions: []Direction{DirA}, Faults: ordered, Tiers: aOnly()},

		// Sync protocol — message order is observable, so reorder applies.
		{Name: "sync", Directions: []Direction{DirA}, Faults: ordered, Tiers: aOnly()},

		// Awareness — presence updates for distinct clients are commutative, so reorder is NOT
		// applicable; claiming it would make "100% detected" ill-defined.
		{Name: "awareness", Directions: []Direction{DirA}, Faults: commutative, Tiers: aOnly()},

		// Snapshot has its own encoding; GC and subdoc are states reached rather than encodings, so
		// direction B for them is "this library builds it, the reference re-encodes".
		//
		// snapshot:B (T031) — this library encodes the snapshot, the reference decodes it,
		// re-encodes it to the same bytes, AND restores from it via createDocFromSnapshot. The
		// restore matters: re-encodability alone would pass a snapshot that round-trips as bytes
		// while being unusable to a consumer.
		//
		// gc:B (T032) — a gc-ENABLED document built and encoded here, applied and re-encoded
		// there. Direction A cannot reach this at all: there the reference collects before Go
		// sees any bytes, so this library's OWN gc decisions were never compared. Verified on
		// documents that actually collect (96 structs, 52 collected at seed 1), not on a build
		// that would have encoded identically with gc off.
		{Name: "snapshot", Directions: []Direction{DirA, DirB}, Faults: ordered, Tiers: both()},
		{Name: "gc", Directions: []Direction{DirA, DirB}, Faults: commutative, Tiers: both()},
		{Name: "subdoc", Directions: []Direction{DirA}, Faults: commutative, Tiers: aOnly()},
	} {
		if err := r.Register(s); err != nil {
			// A malformed registry is a programming error in the harness itself, and every later
			// "green" depends on it, so fail loudly at construction rather than run degraded.
			panic("oracle: default registry is invalid: " + err.Error())
		}
	}
	return r
}
