# Implementation Plan: Full Parity Coverage & Awareness Reaper Redesign

**Branch**: `004-full-parity-coverage` | **Date**: 2026-08-13 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `/specs/004-full-parity-coverage/spec.md`

## Summary

Feature 003 proved the engine byte-exact with `yjs@13.6.31` across 1.1M seeds, then a review found
eleven defects in code that gate had been green over. The cause was not seed count but **generator
vocabulary**: all seven generators together emit five op kinds, three of the four wire formats have
no differential at all, undo has none, and direction A never runs this library's constructors.

This feature widens what is *checked* rather than rewriting what was checked. It adds an undo
differential, direction B, differentials for relative positions / sync / awareness, read-path
coverage, and per-surface fault injection — under a three-tier gate where **every surface is
enforced at PR time**. It also closes the ordering deviation those new differentials would
otherwise trip over, and splits presence handling into a plain type (no thread) and a managed type
(owns the reference's timer), which removes the 003 data race at its cause.

Two design consequences came out of Phase 0 research and are load-bearing:

1. **The undo ordering fix reaches `StructStore`, not the delete set.** The reference's restoration
   order originates in client first-insertion order in the struct store and is merely carried by
   `afterState` → the insertions delete set → `iterateDeletedStructs`. Go loses it at the source.
2. **The presence timer cannot simply be deleted.** The reference's interval performs local
   *renewal* — an outbound heartbeat — as well as reaping. Reaping can be lazy; renewal cannot.

## Technical Context

**Language/Version**: Go 1.24.3

**Primary Dependencies**: none new. Runtime: `mitchellh/copystructure` (pre-existing; Constitution
III marks it for eventual removal). Reference side for tests: `yjs@13.6.31` / `y-protocols@1.0.7`,
pinned by committed lockfiles and installed with `npm ci`.

**Storage**: N/A (library)

**Testing**: Go stdlib `testing`; Node-driven differential generators under `fuzz/`; deterministic
ClientID via the `WithClientID` `NewDoc` option

**Target Platform**: any Go 1.24 target; CI on ubuntu-latest with Go 1.24.x + Node LTS

**Project Type**: single-package Go library plus a `protocol/` subpackage

**Performance Goals**: no regression against a **freshly captured** baseline (the 003 baseline is
stale). Judged as in 003: a regression must be both benchstat-significant (p<0.05) **and** worse by
>3%.

**Constraints**: fast tier hard ceiling 10 minutes (≈7 min target) covering every realized
(surface, direction) cell; full tier gates merge; scale tier ≥1e6 seeds aggregate nightly with a
≥10k floor per cell. No library-owned goroutine unless explicitly requested.

**Scale/Scope**: **13 surfaces** (data-model.md is canonical) — `text`, `array`, `map`, `xml`, `applyDelta`, `update`, `undo`,
`relpos`, `sync`, `awareness`, `snapshot`, `gc`, `subdoc` — as enumerated in data-model.md, which is
the canonical registry list. Not every surface realizes both directions, so the matrix is counted in
realized (surface, direction) **cells**, which is the unit SC-001's floor and SC-001a's fast-tier
membership both use. 7 prioritized user stories; engine changes confined to struct-store ordering,
presence types, and the residual defect fixes.

## Constitution Check

*GATE: checked before Phase 0 and re-checked after Phase 1 design.*

| Principle | Assessment | Status |
|---|---|---|
| I. Yjs Wire Compatibility | The feature exists to widen wire/behaviour verification; adds three previously-unverified wire formats | ✅ Reinforces |
| II. Single-Package Library | Two additions assessed honestly. (a) Presence split adds a type, not a package. (b) The harness core is a NEW package — placed at `internal/oracle/` so it is **not** importable by consumers and the shipped public API stays unchanged; a top-level `oracle/` would have added verification machinery to the library's public surface. FR-009 makes the goroutine opt-in, which is this principle's own named example | ✅ Pass |
| III. Zero External Runtime Dependencies | No new dependency. FR-001a uses the in-repo insertion-ordered pattern (`objectData`), not a third-party ordered map (R4) | ✅ Pass |
| IV. Test-First Development | Each differential is written before the engine change it gates; FR-016 requires every defect become coverage before closing | ✅ Pass |
| V. Cross-Language Compatibility Tests | The entire feature; extends to relative positions, sync, awareness | ✅ Reinforces |
| VI. Root Cause Analysis | R1 traced the ordering to its origin rather than patching the symptom; the presence fix removes the cause rather than guarding it | ✅ Pass |
| VII. DRY | Five near-duplicate differential harnesses, and `mulberry32` copy-pasted across **seven** files (five `.mjs` plus `generate.js` and `ops.js`); new surfaces MUST share a harness rather than add a sixth copy | ⚠️ Addressed in design (below) |
| VIII. Lint on Completion | `golangci-lint run` zero violations, already enforced by CI and the pre-push hook | ✅ Pass |
| IX. No Legacy Code | `FUZZ_STRICT_GC`'s dead branch was ALREADY removed in `cbafa17` (verification only now, not feature work); the stale-encoding decoder hazard is resolved as documented+pinned rather than by deviating, because the snapshot format is provably indistinguishable in the reference too (FR-015, revised); the managed presence type is built correct rather than porting a known defect to fix later | ✅ Pass |
| X. No Busywork | Every surface added traces to a measured gap or a found defect | ✅ Pass |
| XI. Meaningful Tests Only | Fault injection makes each surface prove it can fail; FR-008 forbids an exempt category | ✅ Reinforces |
| XII. Latest Dependencies Always | Reference pinned deliberately for reproducibility (FR-020); pinning is the point, not staleness | ✅ Pass (justified) |
| XIII. No Assumptions | Phase 0 verified every reference claim against pinned source with file paths recorded | ✅ Pass |
| XIV. ≥95% Coverage on New & Touched Code | FR-018 measures over functions actually changed, correcting the 003 miss where only newly-added functions were checked | ✅ Pass |
| XV. Motivated Edits Only | Engine changes limited to struct-store ordering, presence types, and named residual defects | ✅ Pass |

**Principle VII — the one item needing a design answer.** Adding five-plus new differentials to a
harness that already contains five near-copies would multiply the duplication. The design below
requires a shared harness core (seeded RNG, corpus I/O, health reporting, fault injection, coverage
reporting) with per-surface modules supplying only generation and comparison. Existing generators
are migrated onto it rather than left alongside it — otherwise DRY degrades as a direct result of
this feature.

**No violations requiring justification.** The one deviation the constitution would otherwise flag
— an opt-in goroutine — is explicitly permitted by Principle II's own wording.

## Project Structure

### Documentation (this feature)

```text
specs/004-full-parity-coverage/
├── plan.md              # This file
├── research.md          # Phase 0 — reference-source findings (R1–R7)
├── data-model.md        # Phase 1 — entities
├── quickstart.md        # Phase 1 — how to run and validate each tier
├── contracts/           # Phase 1 — harness, surface and presence contracts
└── tasks.md             # Phase 2 — /speckit-tasks, NOT created here
```

### Source Code (repository root)

```text
.                                   # single Go package `y_crdt`
├── struct_store.go                 # + client first-insertion order (R1, FR-001a)
├── undo_manager.go                 # consult store order; drop the (client,clock) sort
├── awareness.go                    # split: plain type (exported fields, no goroutine)
├── awareness_managed.go            # NEW — managed type: owns ticker, renewal + reaping
├── y_xml_fragment.go               # FR-014 — render every child kind
├── snapshot.go                     # FR-015 — documented hazard + pinned parity (format has no marker)
├── y_text.go                       # FR-014d — content-kind whitelist -> reference behaviour
│                                   #   (plan's Risks table calls this the highest-risk change)
├── y_xml_element.go                # FR-014b — []uint8 attribute coercion
├── ws_shared_doc.go                # FR-013a — in-repo presence consumer -> managed type
├── protocol/                       # sync + awareness helpers (differential targets)
│
├── internal/oracle/                # NEW — shared harness core (Principle VII).
│   │                               # internal/ so it is NOT part of the public API (Principle II)
│   ├── surface.go                  # surface registry, direction, proven status
│   ├── faults.go                   # fault kinds + applicability per surface
│   └── coverage.go                 # API-surface-derived operation report (FR-005a)
│
├── *_diff_test.go                  # existing differentials, migrated onto the core
├── undo_diff_test.go               # NEW — US1
├── dir_b_diff_test.go              # NEW — US2
├── relpos_diff_test.go             # NEW — US3
├── sync_diff_test.go               # NEW — US3
├── awareness_diff_test.go          # NEW — US3
│
└── fuzz/
    ├── harness/                    # NEW — shared JS side: rng, hex, corpus emit
    ├── native_diff_*.mjs           # existing generators, migrated onto harness/
    ├── undo_gen.mjs                # NEW
    ├── dir_b_verify.mjs            # NEW — reference decodes + re-encodes
    ├── relpos_gen.mjs              # NEW
    ├── sync_gen.mjs                # NEW
    ├── awareness_gen.mjs           # NEW
    └── run-gate.sh                 # extended: --tier fast|full|scale
```

**Structure Decision**: the repository's *public* surface stays as it is — one importable package
plus `protocol/`. The harness core goes to `internal/oracle/`, which the Go toolchain makes
unimportable from outside the module, so surfaces get a shared core without verification machinery
becoming public API. `fuzz/harness/` does the same on the JavaScript side. Differential tests stay as `*_test.go` beside the code they verify, matching
the existing layout. `run-gate.sh` gains a `--tier` selector rather than a fourth entrypoint,
extending the pinned CLI contract established in 003 (T015).

## Phase Sequencing

**Numbering note**: this table uses *delivery* phases 0–7. `tasks.md` numbers its phases 1–10 and
`quickstart.md` refers to delivery phases. Mapping: delivery 0 → tasks 1–2; 1 → tasks 3; 2 → tasks
4; 3 → tasks 5; 4 → tasks 6–7; 5 → tasks 8; 6 → tasks 9; 7 → tasks 10.

Ordered so each phase is independently landable and no phase depends on a later one.

| Phase | Content | Gate |
|---|---|---|
| **0** | Fresh perf baseline; harness core extracted; existing generators migrated onto it | Existing differentials green through the new core; baseline recorded |
| **1 (US1)** | Undo differential + struct-store ordering + `(client,clock)` sort removed | Undo surface zero divergence incl. multi-participant; stack status compared |
| **2 (US2)** | Direction B across surfaces | Round-trip byte-identical; repeated-build determinism (SC-005) |
| **3 (US3)** | Relative position, sync, awareness differentials | Three wire formats zero divergence both directions |
| **4 (US4/US5)** | Op-vocabulary expansion incl. read paths; API-derived coverage report; fault injection every surface | SC-002 zero exempt surfaces; SC-003 report green |
| **5 (US6)** | Presence type split; managed type reproduces reference timer | No thread by default; renewal verified against a reference peer |
| **6 (US7)** | Residual defects; doc drift; coverage floor on touched functions | Each has a test failing before the fix |
| **7** | Tier wiring (fast/full/scale), scale run, perf re-check | SC-001, SC-001a, SC-009, SC-010 |

**Why Phase 0 comes first**: Principle VII. Extracting the shared harness *before* adding five
surfaces is the difference between one harness with eleven surfaces and eleven harnesses. Doing it
afterwards would mean rewriting the new code immediately.

**Why US1 precedes US2** despite both being P1: the undo differential is the highest-yield surface
(four found defects, zero coverage), and its engine change is confined to ordering. Direction B
touches every surface, so it benefits from landing on a settled harness.

## Risks

| Risk | Mitigation |
|---|---|
| StructStore client order may not agree between implementations for all streams (R1 prediction) | The US1 differential tests it directly; disagreement is a finding at low seed count, not a late surprise |
| Fast-tier ceiling (10 min) exceeded once 13 surfaces across their realized directions are enforced | Per-surface seed weighting is tunable; FR-022 fixes *presence* in the tier, not seed count |
| Ordering change to `StructStore` perturbs encoding paths | Encoding already sorts deliberately (`WriteStateVector` descending) and is byte-exact at 6.4M ops; the change adds order tracking without altering existing sort sites, and SC-010 re-runs all fixtures |
| Harness migration regresses existing green surfaces | Phase 0 gate is the existing differentials passing *through* the new core before any surface is added |
| FR-014d (content-kind whitelist → default case) changes countability and index arithmetic in the text layer | Highest-risk change in the residual phase despite its P3 grouping; gated on byte-exactness against the reference (SC-010) and on the text + applyDelta surfaces, not on unit tests alone |
| Delivery Phase 5 (tasks Phase 8) breaks existing awareness tests that use the removed auto-start API | A migration task (T060a) retires/ports every file matching the predicate `grep -rl 'NewAwareness(\|GetStates()\|GetMeta()'` — **15 files**, where a hand-written list named 8. The predicate is the spec, not the enumeration |

## Complexity Tracking

No constitutional violations requiring justification. The two structural additions are recorded
here for visibility rather than as exceptions:

| Addition | Why needed | Simpler alternative rejected because |
|---|---|---|
| `internal/oracle/` harness core package | Principle VII — five near-duplicate harnesses exist; this feature adds five-plus more surfaces | Copying the existing pattern again would take duplication from 5 to 11 as a direct result of a feature meant to improve verification. A top-level `oracle/` was rejected as it would publish the harness as API |
| Second presence type (`awareness_managed.go`) | FR-012 — makes "exported fields + library-owned writer" unrepresentable | One type with a documented contract was rejected in clarification: an unguarded concurrent map read aborts the Go process, so a comment is not a safety mechanism |
