# Implementation Plan: Phase 1 — Correctness Completeness

**Branch**: `002-phase1-correctness-completeness` | **Date**: 2026-06-24 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `specs/002-phase1-correctness-completeness/spec.md`

## Summary

Close the remaining Yjs conformance gaps so the library is behaviorally and wire-compatible
with Yjs **13.6.31** across the features that are currently partial/non-functional: nested
subdocuments, deletion/GC cascade of nested types, rich-text formatting + embedded types, V2
snapshots, the UndoManager, automatic presence expiry, and XML string serialization. First
**widen the cross-impl fuzz gate** (the regression oracle) to cover all these surfaces, then
land each fix flipping its own strict assertion so fix-and-proof land atomically.

Technical approach: faithful, minimal ports of the corresponding Yjs logic, verified per-fix
against the actual `yjs@13.6.31` source (FR-035). No module/package rename (excluded). No
performance *improvement* (Phase 2), but a no-regression line is held. Detailed root-cause /
fix-approach analysis per work item lives in [research.md](./research.md); behavioral API
contracts in [contracts/](./contracts/); validation in [quickstart.md](./quickstart.md).

## Technical Context

**Language/Version**: Go 1.24 (`go.mod` → `go 1.24.3`)

**Primary Dependencies**: Runtime — stdlib only (+ existing `mitchellh/copystructure`; its
removal is Phase 2, not this phase). Tests — Go stdlib `testing` (deterministic ClientID via the `WithClientID` `NewDoc` option;
`bytedance/mockey` already removed — not in `go.mod`). Cross-impl harness — Node.js + `yjs@13.6.31` + `y-protocols@1.0.7` (test-time only, fixture
generation). **This phase adds no new runtime dependency** (the awareness fix uses stdlib
`sync`).

**Storage**: N/A (library)

**Testing**: `go test ./...` (unit + `-race`); cross-impl JS↔Go fuzz gate (`fuzz/`, Node +
`yjs@13.6.31`); byte-exact reference fixtures (`compatibility_v2_test.go`, `v2_test_fixtures/`);
coverage via `go test -coverprofile`.

**Target Platform**: Any Go 1.24+ platform. Interop target: JavaScript Yjs / y-protocols.

**Project Type**: Single-package Go library (`package y_crdt` at repo root) + `protocol/`
subpackage. No structural change this phase.

**Performance Goals**: No measurable regression vs. the pre-phase baseline (SC-012). Performance
*improvement* is explicitly out of scope (Phase 2).

**Constraints**: Byte-exact V1+V2 wire parity with Yjs 13.6.31 (non-negotiable); ≥95%
statement coverage on new/changed code; awareness path race-free (`-race`); no goroutine/listener
leaks; every Yjs-behavior claim verified against actual pinned source.

**Scale/Scope**: 8 work items (1.1–1.8; 10 sub-items counting the 1.3 and 1.7 A/B splits) mapped to 9 user stories, across ~12 core files + the fuzz harness; ≈15–19
engineer-days per the Phase-1 plan.

**Reference**: the on-branch gap-by-gap RCA is [research.md](./research.md), derived from
`YGO-PHASE1-PLAN.md` / `YGO-DEVELOPMENT-PLAN.md` (planning docs in the main checkout, untracked —
not on this branch). All `file:line`
references in `research.md` are **re-verified against locally-installed `yjs@13.6.31` at
implementation time** per FR-035.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| Principle | How this plan satisfies it | Enforcing gate |
|---|---|---|
| I. Yjs Wire Compatibility (NON-NEGOTIABLE) | No fix may diverge V1/V2 bytes; snapshot-V2 + XML gain new byte-parity fixtures | Cross-impl fuzz gate green + byte-exact fixtures (FR-032) |
| IV. Test-First / XI. Meaningful Tests / XIV. ≥95% New-Code Coverage | Each fix ships regression-first tests; coverage measured per change | `go test -coverprofile` ≥95% on new code (FR-033) |
| V. Cross-Language Compatibility Tests | Work item 1.8 widens the gate to all five surfaces; each fix flips its strict flag | Widened gate, all `FUZZ_STRICT_*` ON at phase end (SC-001) |
| VI. Root Cause Analysis (NON-NEGOTIABLE) / XV. Motivated Edits | Every fix cites root cause `file:line` + what/why/how; no speculative edits | Review + research.md per-item RCA (FR-034) |
| VII. DRY — Single Source of Truth | One `xmlAttrValueString` helper for B1–B4; one fuzz generator; shared DS-encoder narrowing for snapshot/SV | Design (research.md); review |
| VIII. Lint on Completion | `golangci-lint run` clean before each commit | Lint gate |
| XII. Latest Deps / XIII. No Assumptions / FR-035 | Reference verified online (yjs **13.6.31**, 2026-06-24); behavior verified against actual source, cited `file:line` | Per-fix source-verification gate |
| III. Zero External Runtime Deps | No new runtime dependency; awareness uses stdlib `sync` | Dependency review |
| II. Single-Package Library | No restructure; the awareness reaper goroutine is the constitution's sanctioned exception | Design |
| IX. No Legacy Code | Replaces `ContentDoc`/`ContentType` TODO stubs and the commented awareness timer with real implementations | Review |

**Result: PASS — no violations.** Performance is correctness-only with a no-regression line
(VI: correctness before performance), consistent with the gates-first philosophy.

## Project Structure

### Documentation (this feature)

```text
specs/002-phase1-correctness-completeness/
├── plan.md              # This file
├── research.md          # Phase 0 — per-item root cause / fix approach / decisions
├── data-model.md        # Phase 1 — entities & state transitions touched
├── quickstart.md        # Phase 1 — how to validate each fix + the gate
├── contracts/           # Phase 1 — behavioral API + fuzz-gate contracts
│   ├── phase1-api-contracts.md
│   └── fuzz-gate-contract.md
└── tasks.md             # Phase 2 output (/speckit-tasks — NOT created here)
```

### Source Code (repository root)

No restructure (single-package library; see the "keep it flat" rationale — the core type graph
is circular, so the core stays one Go package). Files touched, by work item:

```text
.                                   # package y_crdt (repo root)
├── content_doc.go                  # 1.1 subdocs: ContentDoc.Integrate/Delete (+ ShouldLoad)
├── content_type.go                 # 1.2 GC cascade: ContentType.Delete/GC (port from yjs)
├── y_text.go                       # 1.3 InsertText negation pre-pass; YTextEvent delta guards + ContentType case
├── snapshot.go                     # 1.4 snapshot V1/V2 split (Encode/DecodeSnapshotV1/V2)
├── merge.go, delete_set.go         # 1.4 narrow WriteStateVector/ReadStateVector to DS encoders
├── undo_manager.go, item.go        # 1.6 UndoManager G1–G7 (RedoItem 6-arg, isDeletedByUndoStack, doc scope, …)
├── awareness.go, protocols.go      # 1.7A reaper goroutine + mutex + unit fix + Destroy teardown
├── y_xml_text.go, y_xml_element.go # 1.7B XML serialization via shared xmlAttrValueString helper
│
├── protocol/                       # subpackage — unchanged this phase
├── fuzz/                           # 1.8 widen generator + harness; bump pin → yjs@13.6.31
│   ├── generate.js, ops.js, canonical.js, run-gate.sh, package.json
│   └── (Go consumer: fuzz_gate_test.go at repo root)
└── v2_test_fixtures/               # regenerate against yjs@13.6.31
```

**Structure Decision**: Keep the existing flat single package + `protocol/` subpackage.
Phase 1 changes are surgical edits to existing files plus new `*_test.go` files; no new packages,
no moves. (Rename/identity work is Phase 0.1, explicitly excluded.)

## Complexity Tracking

> No constitution violations — table intentionally empty.

## Execution Order (from the Phase-1 plan)

```
1.8  WIDEN FUZZ GATE (all 5 surfaces; nested-strict ON, others OFF)   ← FIRST, gates everything
       │   (each fix below flips its own FUZZ_STRICT_* flag)
   ┌───┴──────────────────────────────┬───────────────────────────────┐
   ▼  serial on the gate              ▼  parallel (gate-independent)
  1.1 subdocs   (+SUBDOCS strict)      1.3A/1.3B  YText prelim + YEvent delta
  1.2 GC cascade (+GC strict)          1.6  UndoManager (G4→G1, then G2/G3/G5/G6/G7)
  1.4 snapshot  (+SNAPSHOT strict)     1.7A awareness reaper (+ mutex; -race)
  1.7B XML      (+XML strict)          1.5  ordering regression test (anytime)
```

**Phase boundaries**: This `/speckit-plan` run produces design artifacts only. Task breakdown is
`/speckit-tasks`; implementation is `/speckit-implement`. Definition of Done = spec §"Definition
of Done — Phase 1" (all gaps fixed/scoped, all `FUZZ_STRICT_*` ON, byte-parity green, `-race`
clean, no leaks, upstream reconciliation honored).
