# Data Model: Full Parity Coverage & Awareness Reaper Redesign

Entities the feature introduces or changes. Verification entities (Surface, Direction, Fault,
CoverageReport, Tier) live in the `internal/oracle/` harness core; presence entities are engine types.

---

## Surface

A distinct behavioural area under comparison. The registry of surfaces is what makes "every
surface" mechanically checkable in FR-008 and FR-022.

| Field | Meaning | Rules |
|---|---|---|
| `Name` | Canonical identifier (`text`, `array`, `map`, `xml`, `applyDelta`, `undo`, `relpos`, `sync`, `awareness`, `snapshot`, `gc`, `subdoc`, `update`) | Unique; used in tier reports and CLI selectors |
| `Directions` | Which directions are realized | Non-empty. `A` = reference generates, this library replays; `B` = this library generates, reference decodes and re-encodes |
| `Faults` | Fault kinds applicable to this surface | **MUST be non-empty** (FR-008) — an empty set is a harness gap that fails the suite, not an exemption |
| `Proven` | Whether every applicable fault was detected on the last run | Derived, never hand-set. No "unproven but accepted" state exists |
| `Tiers` | Tiers each realized **(surface, direction) CELL** runs in — keyed per cell, NOT per surface | Every cell **MUST include `fast`** (SC-001a) |

**Invariant**: a surface with `len(Faults) == 0`, or any realized cell without `fast` in its
`Tiers`, fails the suite by construction. Both encode a 003 failure — a surface counted as covered
without being exercised, and a gate enforced only nightly.

**Why tier membership is per-CELL, not per-surface**: at surface granularity a surface registered
fast-tier for direction A only would satisfy the in-code invariant while violating SC-001a, and
direction B would be silently nightly-only. That hazard is not hypothetical — it was caught by hand
for the three wire surfaces (which is why T042 must register both directions explicitly), and a
model that needs hand-catching is the model that let the 003 gate stay hollow. Keying `Tiers` per
cell makes the invariant enforce what SC-001a requires.

---

## Direction

Which implementation generates and which verifies.

| Value | Generates | Verifies | Notes |
|---|---|---|---|
| `A` | reference | this library replays natively, compares encoded state | Every surface today |
| `B` | this library | reference decodes and re-encodes, compares bytes | New (FR-002); the only way to detect non-canonical output |

**Rule**: `B` is not a mirror of `A`. It exercises this library's *constructors*, which `A`
structurally cannot reach because `A` only ever applies encoded updates.

---

## Fault

A deliberate corruption proving a surface would report divergence.

| Field | Meaning |
|---|---|
| `Kind` | `drop-op`, `dup-op`, `reorder-op`, `permute-attr`, `corrupt-expectation` |
| `AppliesTo` | Surfaces where the kind is meaningful |
| `Detected` | Whether the oracle reported divergence when injected |

**Rules**:
- Every surface has ≥1 applicable kind (FR-008). `corrupt-expectation` generalises to anything with
  a comparable artefact, so no surface is inherently unfaultable.
- `reorder-op` / `dup-op` apply only to order-dependent / non-idempotent cases, so "100% detected"
  stays well defined — a reorder on a commutative multiset legitimately still matches.
- Detection MUST be by comparison **against the reference**, not against this library's own output
  (FR-007). The 003 self-test compared Go to Go and therefore proved encoder sensitivity, not
  cross-implementation detection.

---

## CoverageReport

Which public operations the generators actually invoke.

| Field | Meaning |
|---|---|
| `Operations` | Every public operation producing observable output, **derived from the exported method set** |
| `Exercised` | Which were invoked by at least one generator |
| `Missing` | The difference — non-empty fails the suite |

**Rules**:
- Includes mutating *and* read/serialization operations — delta, string and JSON rendering,
  attribute and value getters (FR-005). Two known defects sit in read paths.
- **Derived, never hand-maintained** (FR-005a): a newly added public operation appears in
  `Missing` until covered. A curated list decays silently, which is the failure mode being avoided.

---

## Tier

| Tier | Trigger | Scope | Budget |
|---|---|---|---|
| `fast` | every change proposal | **every realized (surface, direction) cell**, small per-cell seed share | **hard ceiling 10 min**, ≈7 min target |
| `full` | before acceptance into mainline | every realized cell, **≥1,000 seeds per cell** | ≤30 min |
| `scale` | scheduled | ≥1e6 seeds aggregate, **≥10,000-seed floor per realized cell** | nightly window |

**Rule**: each tier reports which surfaces it ran and at what volume (FR-023), so a surface
dropping out is visible rather than inferred from green.

**As built.** Both tier properties are enforced mechanically rather than by reading a log:

- *Cell membership* (`fast` covers every realized cell) — `Register` rejects a cell whose TierSet
  omits `fast`, and CI plus `.githooks/pre-push` diff `surfaces -cells` against
  `surfaces -cells -tier fast`. Three checks, all derived from the registry; none is a hand-kept
  list that could drift.
- *Seed floor* — `oracle.TierFloor` / `oracle.CheckCellVolume`, called by `fuzz/run-gate.sh` BEFORE
  the run. A scale run that would under-serve a cell fails instead of reporting success. `fast` and
  `full` deliberately have floor 0: `fast` is bounded by a wall-clock ceiling, which a seed floor
  would directly contradict.

**Measured volumes** (defaults in `run-gate.sh`, from the T071a curve of ~11 s fixed + 0.82 ms/seed
across all 13 surfaces in both directions):

| Tier | aggregate seeds | per cell | measured wall clock |
|---|---|---|---|
| `fast` | 20 000 | 1 538 | ~24 s (was 2 000 aggregate — the 200-seed floor — because nobody had measured it) |
| `full` | 200 000 | 15 384 | ~3 min |
| `scale` | 1 000 000 | 76 923 | **877 s measured, zero divergences** |

---

## StructStore client ordering *(engine change)*

Reproduces the reference's undo restoration order (R1, FR-001a).

| Aspect | Reference | Current Go | Change |
|---|---|---|---|
| Store clients | JS `Map` — first-insertion order | `map[Number]*[]IAbstractStruct` — randomized | Add an insertion-order client slice |
| State vector | `Map`, order inherited | `map[Number]Number` — randomized | Order consulted from the store where needed |
| Undo insertions | delete set inherits afterState order | rebuilt then sorted `(client, clock)` | Consult store order; **remove the sort** |

**Rules**:
- The order tracked is **first insertion into the struct store**, matching where the reference's
  order originates. Ordering the delete set alone does not reproduce it.
- Existing deliberate sorts on encoding paths (e.g. `WriteStateVector` descending) are **not**
  changed — they are byte-exact today and serve a different purpose.
- The `(client, clock)` sort in the undo path is removed, not supplemented. It was a canonical order
  chosen to escape map randomness, and it is not the reference's.

---

## Presence: plain type *(engine change)*

| Aspect | Rule |
|---|---|
| Threads | Never starts one |
| State fields | **Exported** — no library-owned writer exists, so concurrency is caller-scheduled and caller-owned, ordinary for a Go data structure |
| Expiry | Stale remote entries judged expired **on access**, matching the reference's judgement of presence |
| Renewal | **None** — documented parity limitation (FR-011): a quiet client is dropped by reference peers |
| Disposal | None required |

## Presence: managed type *(engine change)*

| Aspect | Rule |
|---|---|
| Threads | Owns one ticker, started only by an explicit call |
| State access | **Accessors only**, copying under its lock |
| Behaviour | Reproduces the reference interval in full — periodic local renewal **and** reaping — emitting the same events with the same payloads |
| Parity | **Presence parity claims attach to this type** (FR-011a) |
| Disposal | Stopping leaves no threads, timers, or registered callbacks |

**Invariant**: exported state fields and a library-owned writer never coexist on one value
(FR-012a). Enforced by the type split rather than documentation, because an unguarded concurrent map
read aborts the Go process rather than raising a recoverable error.
