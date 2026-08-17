---
description: "Task list for Phase 1 — Correctness Completeness"
---

# Tasks: Phase 1 — Correctness Completeness

**Input**: Design documents from `specs/002-phase1-correctness-completeness/`
**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/, quickstart.md

**Tests**: MANDATORY here (Constitution IV/XI/XIV + spec FR-033) — every behavior change ships a
regression test that fails before / passes after, ≥95% new-code coverage. Wire-touching changes
keep byte-parity fixtures green. Every Yjs-behavior reference is verified against the actual
`yjs@13.6.31` source and cited `file:line` (FR-035) — never training data.

**Paths**: single Go package at repo root (`package y_crdt`) + `protocol/` subpackage + `fuzz/`,
`v2_test_fixtures/`. Run all commands from the worktree root `…/y-crdt-phase1`.

## Format: `[ID] [P?] [Story] Description`
- **[P]** = parallelizable (different files, no incomplete-task dependency)
- **[USx]** = the user story (spec.md); Setup/Foundational/Polish carry no story label

---

## Phase 1: Setup (Shared Infrastructure)

- [x] T001 Re-verify the current latest Yjs online (`npm view yjs version`); confirm it is `13.6.31` (FR-035). If a newer version exists, STOP and surface it before pinning.
- [x] T002 [P] Install reference source in `fuzz/` (`cd fuzz && npm i yjs@13.6.31`) and bump the pin in `fuzz/package.json` 13.6.27 → 13.6.31.
- [x] T003 [P] Verify the current y-protocols version online (`npm view y-protocols version`; confirmed `1.0.7` on 2026-06-24, FR-035) and install/pin `y-protocols@1.0.7` where the awareness reference sequence is captured (alongside `fuzz/`). This is the reference for the awareness reaper (T039/T040).
- [x] T004 [P] Install reference source in `v2_test_fixtures/` (`cd v2_test_fixtures && npm i yjs@13.6.31`) and bump the pin in `v2_test_fixtures/package.json`.
- [x] T005 Regenerate `v2_test_fixtures/fixtures.json` against yjs@13.6.31 (`node v2_test_fixtures/generate.js`); diff vs the prior fixtures and investigate any byte change as a real finding (FR-004); keep `compatibility_v2_test.go` green.

**Checkpoint**: reference source installed and pinned at 13.6.31; existing byte-exact fixtures green.

---

## Phase 2: Foundational (Blocking Prerequisites)

**⚠️ Baseline must be captured before any fix so before/after comparisons are valid.**

- [x] T006 Capture the pre-phase baseline: `go test ./...` green, `golangci-lint run` clean, `bash fuzz/run-gate.sh` (strict flags off) green; record results in the PR description.
- [x] T007 Record the performance baseline for SC-012: if the Phase 0.3 bench gate exists, capture its numbers; if it does NOT exist, log that perf no-regression cannot be machine-verified this phase and note it explicitly (no silent gap).

**Checkpoint**: baseline captured. User stories may begin.

---

## Phase 3: User Story 1 — Regression oracle covers every Phase-1 surface (Priority: P1) 🎯 MVP/Foundational

**Goal**: Widen the cross-impl fuzz gate to all five surfaces so every later fix is provable and
non-regressable. **Blocks the strict-gate completion of US2, US3, US7, US8.**

**Independent Test**: `bash fuzz/run-gate.sh` with all `FUZZ_STRICT_*` off generates+decodes cases
across nested/subdocs/GC/snapshot/XML and is green on the current base (harness proven).

### Tests for User Story 1 (REQUIRED)
- [x] T008 [US1] Extend the Go consumer `fuzz_gate_test.go` to decode the new record fields (`subdocs`, `snapshotV1/V2`, `restoredState`, optional `postGcState`) and to assert nested-type convergence (strict ON immediately); keep `recover()` around each apply.

### Implementation for User Story 1
- [x] T009 [US1] Verify reference behavior in `fuzz/node_modules/yjs/src/` for subdoc `toJSON`, `encodeSnapshot{,V2}`, and GC semantics; note `file:line` in the PR (FR-035).
- [x] T010 [US1] Add generator flags `FUZZ_FEATURES`, `FUZZ_GC`, `FUZZ_SNAPSHOT`, and strict flags `FUZZ_STRICT_{SUBDOCS,GC,SNAPSHOT,XML}` (default off) in `fuzz/generate.js` + `fuzz/ops.js`; keep the single mulberry32 generator (no fork).
- [x] T011 [US1] Add nested-type op generation (map.set/array.insert/xml.insert a new `Y.*` child + ops + deletes) in `fuzz/ops.js` (nested-strict ON).
- [x] T012 [P] [US1] Add subdocs op generation (`new Y.Doc({gc?,autoLoad?})`, run ops, sometimes delete) + emit sorted `{guid: toJSON}` in `fuzz/generate.js`.
- [x] T013 [P] [US1] Add GC generation (gc on/off; force enough deletes incl. nested types) + `postGcState` in `fuzz/generate.js`.
- [x] T014 [P] [US1] Add snapshot generation (`encodeSnapshot` V1 + `encodeSnapshotV2` + `createDocFromSnapshot` restored state, gc=false) in `fuzz/generate.js`.
- [x] T015 [P] [US1] Add XML breadth generation (deep element trees, `XmlText` formatting, `Y.XmlHook`) in `fuzz/ops.js`.
- [x] T016 [US1] Keep `fuzz/canonical.js` ↔ `fuzzCanon` (`fuzz_gate_test.go`) in lockstep for any new canonicalization.
- [x] T017 [US1] Run `bash fuzz/run-gate.sh` with all strict flags off → green on the current base (acceptance for 1.8).

**Checkpoint**: gate widened, harness proven; strict dimensions ready for each fix to flip.

---

## Phase 4: User Story 2 — Deleting nested types converges (Priority: P1)

**Goal**: `ContentType.Delete`/`GC` cascade so deleting a nested type tombstones+collects its
children, matching Yjs. **Depends on US1** (nested + GC generation gate it). (US2/US3 are
independent and unordered — see Dependencies & Execution Order.)

**Independent Test**: Array⊃Map, delete the Map → children `Deleted()`, `toJSON` empty; gc=true →
children become `GC` structs and the re-encoded update byte-matches Yjs.

### Tests for User Story 2 (REQUIRED)
- [x] T018 [P] [US2] Regression test in `content_type_cascade_test.go`: nested delete tombstones all children (linked-list + map), `toJSON` empty; gc=true → `GC` structs + identical re-encode; **include the absent-client boundary** (a tombstoned child whose client has no prior-state entry is treated as clock 0 — tombstone-vs-merge decision, spec Edge Cases) (fails before).

### Implementation for User Story 2
- [x] T019 [US2] Verify `ContentType.delete`/`gc` in `fuzz/node_modules/yjs/src/structs/ContentType.js`; cite `file:line` (FR-035). Confirm `BeforeState[absent]==0`, `MergeStructs` element type, `SetMap`/`SetStartItem` on the type interface (do NOT touch `ContentType.Copy` — already fixed by `82536f7`).
- [x] T020 [US2] Implement `ContentType.Delete` (walk `_start` + `_map`; live → `Delete(trans)`; tombstoned & `clock<BeforeState` → append `trans.MergeStructs`; `delete(trans.Changed,type)`) in `content_type.go`.
- [x] T021 [US2] Implement `ContentType.GC` (walk start + map via `.Left` → `item.GC(store,true)`; null start; reset map) in `content_type.go`.
- [x] T022 [US2] Flip `FUZZ_STRICT_GC=1`; run the gate (nested + GC) green; verify ≥95% coverage on the changed code and `golangci-lint run` clean.

**Checkpoint**: nested delete/GC converges with Yjs (SC-002).

---

## Phase 5: User Story 3 — Nested subdocuments functional (Priority: P1)

**Goal**: `ContentDoc.Integrate`/`Delete` feed the existing subdoc infrastructure. **Depends on US1**
(subdocs generation gates it). (US2/US3 are independent and unordered — see Dependencies &
Execution Order.)

**Independent Test**: insert a `Doc` into a Map/Array → in `GetSubdocs()`; autoLoad → `loaded`;
delete → removed + destroyed.

### Tests for User Story 3 (REQUIRED)
- [x] T023 [P] [US3] Regression test in `content_doc_subdocs_test.go`: integrate → added(+loaded if autoLoad); delete → removed+destroyed; same-txn add+delete → withdrawn; **assert (gc=true) that collecting a subdoc-bearing item leaves the subdoc content untouched (FR-011 no-op) — subdoc-GC is intentionally guarded unit-only, NOT by `FUZZ_STRICT_GC`** (fails before).

### Implementation for User Story 3
- [x] T024 [US3] Verify `ContentDoc.integrate`/`delete` + `createDocFromOpts` `shouldLoad` in `fuzz/node_modules/yjs/src/structs/ContentDoc.js`; cite `file:line` (FR-035).
- [x] T025 [US3] Implement `ContentDoc.Integrate` and `ContentDoc.Delete`; keep `GC` a no-op; in `content_doc.go`.
- [x] T026 [US3] Set `ShouldLoad = shouldLoad || autoLoad` in `ReadContentDoc`/`NewDoc` (`content_doc.go`/`doc.go`).
- [x] T027 [US3] Flip `FUZZ_STRICT_SUBDOCS=1`; gate green; ≥95% coverage; `golangci-lint run` clean.

**Checkpoint**: subdocuments add/load/remove/destroy and converge with Yjs (SC-003).

---

## Phase 6: User Story 4 — Undo/redo safe and complete (Priority: P1)

**Goal**: Close all 7 UndoManager gaps (G1/G2 correctness-critical). Gate-independent (behavioral +
cross-impl tests). Internal order **G4 → G1**, then G2/G3/G5/G6/G7.

**Gap↔FR map**: G1→FR-012, G2→FR-013, G3→FR-014, G4 (fields enabling G1/G3)→FR-012/FR-014,
G5→FR-015, G6→FR-016, G7→FR-017; FR-018 (flag-reset) and FR-019 (single code path) are
cross-cutting constraints, not gaps.

**Independent Test**: 2 clients set the same map key; undo preserves the remote write by default,
overwrites only with `IgnoreRemoteMapChanges`; final state byte-identical to Yjs.

### Tests for User Story 4 (REQUIRED)
- [x] T028 [P] [US4] Regression tests in `undo_manager_phase1_test.go`: G1 remote-clobber default vs opt-in; G2 whole-doc scope; G3 stopCapturing→2 items; G5 coalesce→added+updated, Clear→stack-cleared; G6 redo-discard unpins (GC reclaims); G7 Destroy→no new items; **FR-018**: an undo/redo op that errors partway still resets `undoing`/`redoing` to false; **FR-019**: drive `RedoItem` via *both* the undo-redo entry and the seed-left entry and assert identical resulting state (test-defends the single unified code path, not just the review-confirm in T063) (each fails before). Replay in Yjs where convergence matters.

### Implementation for User Story 4
- [x] T029 [US4] Verify `UndoManager.js`, `redoItem`, and `Item.isDeletedByUndoStack` (incl. the #757 fix present in 13.6.31) in `fuzz/node_modules/yjs/src/`; cite `file:line` (FR-035).
- [x] T030 [US4] G4: add fields/options `IgnoreRemoteMapChanges`, `CaptureTransaction`, `CaptureTimeout` (field), `CurrStackItem` in `undo_manager.go`.
- [x] T031 [US4] G1: add `isDeletedByUndoStack` (`item.go`); extend `RedoItem` to the 6-arg form + map branch + #757 in `item.go`/`undo_manager.go`; **reconcile with the merged seed-left (`44bb5f2`) into a single code path** (FR-019).
- [x] T032 [US4] G2: make `Scopes` hold `IAbstractType|*Doc`, store `doc`, add the `==doc` disjuncts, fix `GetDoc()` in `undo_manager.go`.
- [x] T033 [P] [US4] G3: add the `lastChange>0` capture-coalesce guard in `undo_manager.go`.
- [x] T034 [P] [US4] G5: track `didAdd`; emit `stack-item-updated` (coalesce) and `stack-cleared` in `undo_manager.go`.
- [x] T035 [US4] G6: `Clear(clearUndo,clearRedo)` + `CanUndo`/`CanRedo` + `keepItem(_,false)` on redo discard in `undo_manager.go`.
- [x] T036 [US4] G7: `Destroy()` (unregister `afterTransaction` + tracked origins) + `AddToScope`/tracked-origin APIs; `defer` reset of `undoing`/`redoing` (FR-018) in `undo_manager.go`.
- [x] T037 [US4] Verify ≥95% coverage on changed undo code; `golangci-lint run` clean; existing undo tests green.

**Checkpoint**: undo preserves concurrent remote writes, supports doc scope, leaks nothing (SC-004).

---

## Phase 7: User Story 5 — Presence expiry & self-renewal (Priority: P2)

**Goal**: Re-enable the awareness reaper (auto-start on `NewAwareness`, stopped by `Destroy()`),
mutex-guarded, with renew-local + reap-remote. Gate-independent; concurrency is the risk.

**Independent Test**: injected/shrunk clock — stale remote reaped + `removed` event; local renews
(not reaped); `-race` clean; create/destroy N → stable goroutine count.

### Tests for User Story 5 (REQUIRED)
- [x] T038 [P] [US5] Tests in `awareness_reaper_test.go`: stale-remote reaped + `removed` event; local-renew advances clock; reaper auto-active without explicit start; `Destroy()` stops it; run under `-race`; goroutine-leak check (fails before).
- [x] T039 [P] [US5] Cross-language expiry-parity test in `awareness_reaper_test.go`: drive the reaper against a captured y-protocols awareness sequence (reference `awareness.test.js` at `y-protocols@1.0.7`) and assert the same client is expired + the same `removed`/`update` payload is emitted (Constitution V; closes the cross-language gap).

### Implementation for User Story 5
- [x] T040 [US5] Verify the reaper behavior in `y-protocols@1.0.7` `awareness.js` (renew at `outdatedTimeout/2`, reap at `outdatedTimeout`, interval `outdatedTimeout/10`) and confirm its timestamp unit; cite `file:line` (FR-035).
- [x] T041 [US5] Add a `sync.Mutex` across all `States`/`Meta` mutators and make `GetStates` return a copy in `awareness.go`.
- [x] T042 [US5] Implement the stoppable reaper goroutine + `time.Ticker(OutdatedTimeout/10)`; **auto-start in `NewAwareness` — this is Principle II's sanctioned awareness-cleanup goroutine exception, paired with the `Destroy()` teardown in T043**; gather renew/remove decisions under the lock then release before emitting; **reconcile the time-unit mismatch to the single basis T040 confirms in the source (expected milliseconds — `Date.now()`; T040's finding governs if it differs)** — align the timeout comparison and `GetUnixTime` to it, in `awareness.go`.
- [x] T043 [US5] Implement `Destroy()` to stop the reaper (async stopCh signal; not joined, to avoid a self-deadlock); no goroutine leak.
- [x] T044 [US5] Verify ≥95% coverage; `go test ./... -race` green; `golangci-lint run` clean.

**Checkpoint**: live node stays visible to peers; stale presence expires; race-free (SC-005, SC-010).

---

## Phase 8: User Story 6 — Rich-text formatting & embedded types (Priority: P2)

**Goal**: Restore the `InsertText` negation pre-pass (1.3A) and fix the `YTextEvent` delta
(guards + `*ContentType` case, 1.3B). Gate-independent (existing text fuzz + unit). Do 1.3A first.

**Independent Test**: bold range + unattributed delta insert → separate unformatted op matching Yjs.

### Tests for User Story 6 (REQUIRED)
- [x] T045 [P] [US6] Tests in `y_text_phase1_test.go`: (1.3A) unattributed insert in a bold run is unformatted, incl. via `ApplyDelta`/`InsertEmbed`; (1.3B) op mix → no `{insert:""}`/`{delete:0}`/`{retain:0}`; nested `YMap` in a `YText` → one insert op. Cross-impl `toDelta()`/`event.delta` (fails before).

### Implementation for User Story 6
- [x] T046 [US6] Verify `YText.insertText` negation + the delta loop + `ContentType`/`ContentEmbed` handling in `fuzz/node_modules/yjs/src/types/YText.js`; cite `file:line` (FR-035).
- [x] T047 [US6] 1.3A: restore the negation pre-pass at the top of `InsertText` using the `Null` sentinel in `y_text.go`.
- [x] T048 [US6] 1.3B: gate each `addOp` branch (emit only when produced) and add `case *ContentType:` alongside `*ContentEmbed` in `GetDelta`, `DeleteText`, and `ToDelta` in `y_text.go`.
- [x] T049 [US6] Verify ≥95% coverage; `golangci-lint run` clean; existing text tests green.

**Checkpoint**: text deltas match Yjs; no formatting bleed; nested types present (SC-006).

---

## Phase 9: User Story 7 — Snapshots round-trip V1 & V2 (Priority: P2)

**Goal**: Fix the snapshot V1/V2 split and narrow the state-vector encoders to DS. **Depends on US1**
(snapshot generation gates it).

**Independent Test**: `DecodeSnapshotVx(EncodeSnapshotVx(s))==s` (V1+V2, multi-client); Go V2 bytes
== Yjs fixture; decode a Yjs V2 snapshot.

### Tests for User Story 7 (REQUIRED)
- [x] T050 [P] [US7] Tests in `snapshot_v2_test.go`: V1 and V2 round-trip (multi-client DS+SV); Go `EncodeSnapshotV2` bytes == Yjs fixture; decode a Yjs-produced V2 snapshot; V1 default unchanged (fails before).

### Implementation for User Story 7
- [x] T051 [US7] Verify `Snapshot.js` (`encodeSnapshotV2`/`decodeSnapshotV2` V2-default, V1 explicit) in `fuzz/node_modules/yjs/src/`; cite `file:line` (FR-035).
- [x] T052 [US7] Narrow `WriteStateVector`/`ReadStateVector` to `DSEncoder`/`DSDecoder` in `merge.go` (and update `delete_set.go`/callers as needed; confirm no other caller breaks).
- [x] T053 [US7] Add `EncodeSnapshotV1`/`EncodeSnapshotV2` + `DecodeSnapshotV1`/`DecodeSnapshotV2`; make `EncodeSnapshot`/`DecodeSnapshot` alias V1; route decode through the matching DS decoder in `snapshot.go`.
- [x] T054 [US7] Flip `FUZZ_STRICT_SNAPSHOT=1` (gc=false); gate green; ≥95% coverage; `golangci-lint run` clean.

**Checkpoint**: V1 and V2 snapshots interoperate with Yjs (SC-007).

---

## Phase 10: User Story 8 — XML string serialization parity (Priority: P3)

**Goal**: One shared `xmlAttrValueString` helper routes both XML `ToString`s; fixes B1–B4.
**Depends on US1** (XML breadth gates it).

**Independent Test**: parity table (`YXmlElement`/`YXmlText`/`YXmlFragment`, object/bool/num marks,
embed, nil attr, Hook) → `ToString()` byte-equal to Yjs.

### Tests for User Story 8 (REQUIRED)
- [x] T055 [P] [US8] Tests in `xml_tostring_parity_test.go`: the parity table → byte-equal to a Yjs fixture; per-bug cases B1–B4 (fails before).

### Implementation for User Story 8
- [x] T056 [US8] Verify `YXmlText.toString`/`YXmlElement.toString` value interpolation in `fuzz/node_modules/yjs/src/types/`; cite `file:line` (FR-035). Confirm Yjs does NOT entity-escape (do not add escaping).
- [x] T057 [US8] Implement the shared `xmlAttrValueString(any) string` helper (`nil→"null"`, primitives `%v`, arrays comma-joined, objects `[object Object]`); route `YXmlElement.ToString` and `YXmlText.ToString` through it; drop the Object-only node guard. In `y_xml_element.go`/`y_xml_text.go`.
- [x] T058 [US8] Flip `FUZZ_STRICT_XML=1`; gate green; ≥95% coverage; `golangci-lint run` clean.

**Checkpoint**: XML `ToString` byte-identical to Yjs (SC-008).

---

## Phase 11: User Story 9 — Deterministic ordering guard (Priority: P3)

**Goal**: Add a regression guard for the already-correct deterministic emission order (no code fix).

**Independent Test**: encode a ≥3-key/≥2-client `YMap` 100× → byte-identical, == Yjs fixture.

### Tests for User Story 9 (REQUIRED)
- [x] T059 [P] [US9] Regression test in `object_order_guard_test.go`: build a ≥3-key, ≥2-client `YMap`; `EncodeStateAsUpdate{,V2}`/`EncodeStateVector`/delete-set; assert byte-identical across 100 runs and == a checked-in Yjs fixture (SC-009).

**Checkpoint**: ordering regression guarded.

---

## Phase 12: Polish & Phase-1 Definition of Done

- [x] T060 Run the full gate with ALL strict flags ON (`FUZZ_FEATURES=nested,subdocs,xmlhook FUZZ_GC=both FUZZ_SNAPSHOT=1 FUZZ_STRICT_*=1 bash fuzz/run-gate.sh`) → green, single + concurrent, V1 + V2 (SC-001).
- [x] T061 [P] `go test ./... -race` green; `go test ./... -coverprofile cover.out` → ≥95% on all new/changed code (SC-011); `golangci-lint run` clean. (Roll-up of the per-story coverage checks T022/T027/T037/T044/T049/T054/T058.)
- [x] T062 [P] Confirm existing byte-exact fixtures (`compatibility_test.go`, `compatibility_v2_test.go`) stay green — no wire divergence (FR-032).
- [x] T063 Confirm no merged upstream fix was reverted/re-implemented and the seed-left/`redoItem` overlap is a single code path (FR-019).
- [x] T064 SC-012: re-run the perf baseline comparison (if the bench gate exists) → no regression; otherwise record the limitation explicitly as a **conscious, accepted Phase-1 gap** — SC-012 is unmeasurable without the Phase 0.3 bench gate; this is documented, not a silent omission.
- [x] T065 [P] Update `CLAUDE.md` "Recent Changes" / package doc notes for the closed gaps (no new doc files unless requested).
- [x] T066 Confirm each merged fix PR documents its root cause (`file:line`) + what/why/how and contains no speculative edits (Constitution VI/XV; FR-034) — a per-PR review gate.

---

## Dependencies & Execution Order

- **Setup (T001–T005)** → **Foundational (T006–T007)** → user stories.
- **US1 (T008–T017) is the blocking prerequisite** for the strict-gate completion of **US2, US3,
  US7, US8** (their strict-flag tasks T022/T027/T054/T058 require the widened gate).
- **US2 and US3 are independent** (distinct files: `content_type.go` vs `content_doc.go`; both
  depend only on US1). The phase numbering follows spec priority, NOT a hard order — the plan's
  soft preference is subdocs (US3/1.1) before GC cascade (US2/1.2), but either order is valid.
- **US4, US5, US6, US9** are gate-independent — they need only Setup+Foundational and may run in
  parallel with US1 and with each other.
- **Within US4**: T030 (G4) → T031 (G1); others (T032–T036) follow; T028 tests first.
- **Within US6**: 1.3A (T047) before 1.3B (T048).
- **Polish (T060–T066)** depends on all stories complete.

## Parallel Opportunities

- Setup: T002 ‖ T003 ‖ T004.
- US1 generation: T012 ‖ T013 ‖ T014 ‖ T015 (different generation concerns) after T010/T011.
- After US1 lands: **US2 ‖ US3 ‖ US7 ‖ US8** (different files: `content_type.go`,
  `content_doc.go`, `snapshot.go`/`merge.go`, `y_xml_*.go`).
- **US4 ‖ US5 ‖ US6 ‖ US9** can proceed from the start (independent files).
- All `[P]` test tasks within a story run together.

## Implementation Strategy

- **MVP / first increment**: Setup + Foundational + **US1 (the gate)** — the regression oracle that
  makes every subsequent fix provable. Then the P1 correctness fixes **US2, US3, US4**.
- **Incremental delivery**: each user story is its own PR that flips at most one `FUZZ_STRICT_*`
  flag (gate stories) so fix + proof land atomically; gate-independent stories land behind their
  own unit/cross-impl tests.
- **Stop-and-validate** at each Checkpoint; never start a fix without its regression test failing
  first and its Yjs reference verified against actual `yjs@13.6.31` source.
