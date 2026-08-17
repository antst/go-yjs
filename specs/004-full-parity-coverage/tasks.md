# Tasks: Full Parity Coverage & Awareness Reaper Redesign

**Feature**: `004-full-parity-coverage` | **Spec**: [spec.md](./spec.md) | **Plan**: [plan.md](./plan.md)
**Design**: [research.md](./research.md) · [data-model.md](./data-model.md) ·
[contracts/](./contracts/) · [quickstart.md](./quickstart.md)

**Tests**: MANDATORY per Constitution IV (Test-First), XI (Meaningful Tests Only), XIV (≥95% on new
**and touched** code). Every behaviour change ships its tests in the same change. Every fix ships a
regression test that fails before and passes after. Wire-affecting changes assert byte-equality
against the reference (Principle V).

**Organization**: by user story, priority order. Phase 2 (harness core) blocks every story — see the
note there for why it cannot be deferred.

**Format**: `- [ ] [ID] [P?] [Story?] description (path)`. `[P]` = parallelizable (different files,
no incomplete dependency).

**Phase numbering**: this file uses phases 1–10. `plan.md` uses *delivery* phases 0–7 and
`quickstart.md` refers to those. Mapping: delivery 0 → phases 1–2 · 1 → 3 · 2 → 4 · 3 → 5 ·
4 → 6–7 · 5 → 8 · 6 → 9 · 7 → 10.

---

## Phase 1: Setup

- [X] T001 Install the pinned reference from committed lockfiles (`cd fuzz && npm ci`) and record the resolved `yjs` + `lib0` versions in `specs/004-full-parity-coverage/research.md` — `npm ci` not `npm install`, since the lockfile is what pins `lib0`, the encoder every byte comparison rests on (FR-020, C-H6)
- [X] T002 Capture a FRESH performance baseline into `specs/004-full-parity-coverage/baseline.txt` via `go test -run '^$' -bench BenchmarkDocOps -benchmem -count=6 .`, INCLUDING the `goos:/goarch:/pkg:/cpu:` header — the 003 baseline is stale, and a missing header makes benchstat silently print two tables with no `vs base` column (SC-009)
- [X] T003 [P] Verify `benchstat` is available (`go install golang.org/x/perf/cmd/benchstat@latest`) and confirm it emits a `vs base` column against `baseline.txt` before it is relied on (SC-009)

## Phase 2: Foundational — shared harness core ⚠️ BLOCKS ALL STORIES

**Why this cannot be deferred**: five near-duplicate differential harnesses exist today. This
feature adds five-plus surfaces. Building them the same way takes duplication from 5 to 11 as a
direct result of a feature meant to improve verification (Constitution VII). Extracting first costs
one migration; extracting later costs rewriting every new surface.

### Tests for the harness core (REQUIRED — write FIRST, ensure they FAIL) ⚠️

> Constitution IV applies to the harness itself, not only to engine code. The harness is what every
> later "green" rests on; an untested registry invariant is the same class of hazard as an
> unexercised surface.

- [X] T004 [P] Write registry-invariant tests in `internal/oracle/surface_test.go` — registration with an EMPTY fault set must fail, and registration WITHOUT the `fast` tier must fail (C-H1.1, C-H1.2)
- [X] T005 Write the corpus-guard test in `internal/oracle/surface_test.go` — absent, empty, AND zero-case corpora each fail and name the surface; the set-but-empty case is the one 003 left open where `cases=0` passed green (C-H2.1)
- [X] T006 [P] Write the coverage-derivation test as `package oracle_test` in `internal/oracle/coverage_test.go` (external test package — an in-package test cannot construct shared types without the import cycle R7 forbids) — a public operation added to a shared type appears as unexercised WITHOUT editing any list, which is FR-005a's whole anti-staleness property (C-H4.1)
- [X] T007 [P] Write the fault-applicability test in `internal/oracle/faults_test.go` — an inapplicable kind is skipped explicitly, never silently counted as passing (C-H3.3)

### Implementation for the harness core

- [X] T008 Create the surface registry in `internal/oracle/surface.go` — name, realized directions, applicable fault kinds, **per-cell tier membership** (`Tiers` keyed by direction, NOT a single per-surface set), derived proven status (data-model "Surface", C-H1)
- [X] T009 Enforce registry invariants in `internal/oracle/surface.go`: an empty fault set FAILS, and **any realized (surface, direction) cell** without `fast` FAILS — iterate cells, not surfaces. A per-surface check passes while direction B is silently nightly-only, which is the exact 003 failure mode this feature exists to close (C-H1.1, C-H1.2, FR-008, FR-022, SC-001a)
- [X] T010 [P] Create the fault-kind model in `internal/oracle/faults.go` — the five kinds, per-(surface,kind) applicability, and detection recorded against the REFERENCE not against this library's own output (C-H3, FR-007)
- [X] T011 [P] Create the API-derived coverage report in `internal/oracle/coverage.go` — the CALLER passes instances of the shared types and the harness reflects over their method sets (research R7: `internal/oracle` must NOT import the root package, or the in-package differential tests create an import cycle). Apply FR-005a's inclusion predicate (content read/mutate in; lifecycle, observation registration and the `IAbstractContent` family out), fail on any missing (C-H4, FR-005a)
- [X] T012 [P] Create the shared JS harness module in `fuzz/harness/index.mjs` — one seeded `mulberry32`, one `hex`, corpus emit with the `emitted=`/`dropped=` health line on stderr (C-H2.2, C-H5.1); today these are copy-pasted five times
- [X] T013 Add the corpus guard to `internal/oracle/surface.go`: a corpus that is absent, empty, OR yields zero cases fails and names the surface — 003 closed the absent case and left set-but-empty open, where `cases=0` passed green (C-H2.1)
- [X] T014 Migrate every file carrying a `mulberry32` copy onto the shared harness — SIX definitions across seven referencing files (`generate.js` consumes `ops.js`'s). Needs a CJS core (`fuzz/harness/core.cjs`) plus an ESM face, since the `.mjs` generators and `ops.js` differ in module system; migrating only the `.mjs` files would leave copies behind — `fuzz/native_diff_{gen,arr,map,xml,delta}.mjs` PLUS `fuzz/generate.js` and `fuzz/ops.js`. Migrating only the five `.mjs` files would leave two copies behind, so the extraction meant to end duplication would not (Constitution VII)
- [X] T015 Migrate `native_diff_test.go`, `native_arr_diff_test.go`, `native_map_diff_test.go`, `native_xml_diff_test.go`, `native_delta_diff_test.go` and `fuzz_gate_test.go` onto the `internal/oracle/` registry so "every surface" iterates the registry rather than a local list (C-H1.3)
- [X] T016 Extend `fuzz/run-gate.sh` with `--tier fast|full|scale`, retaining the pinned `--seeds/--surface/--dir` contract and the legacy positional form (FR-021)
- [X] T016a **Remove the `--dir B|both` hard-error** in `fuzz/run-gate.sh` (it exits 2 citing 003 T013, which THIS feature implements) — without this, quickstart §5's `--tier fast --dir B` and every direction-B cell are unreachable from the entrypoint (FR-002, FR-021)
- [X] T016b **Derive `ALL_SURFACES` in `fuzz/run-gate.sh` from the `internal/oracle` registry** instead of the hardcoded shell literal `"text array map xml applyDelta update"` — a literal cannot list `undo relpos sync awareness snapshot gc subdoc`, so registering a surface while the CLI silently never runs it is the 003 hollow gate in a new place. Deriving it makes registry/CLI drift impossible rather than merely noticed (FR-022, C-H1.3)
- [X] T016c Register the SIX EXISTING surfaces (`text`, `array`, `map`, `xml`, `applyDelta`, `update`) plus `snapshot`, `gc`, `subdoc` in `internal/oracle/surface.go` with realized directions, fault kinds and per-cell `fast` membership. The registry declares 13 surfaces but only `undo` (T027) and the three wire surfaces (T042) had registration tasks — under T009's invariant the other nine would either fail the suite or silently never register, which is the coverage-claim gap this feature exists to close (C-H1, FR-008, FR-022)
- [X] T017 Add the per-tier coverage report to `fuzz/run-gate.sh` and `internal/oracle/surface.go` — each run names the **(surface, direction) cells** covered and the volume **per cell**, with per-surface totals as a derived rollup. Cell granularity is required because SC-001's ≥10k floor and T071b's mechanical check are per cell and have no per-surface number to read (FR-023, C-H2.3, SC-001)

**Checkpoint**: existing differentials pass THROUGH the new core with no regression, and an
artificially emptied corpus now fails. Gate for every later phase.

---

## Phase 3: US1 — Undo/redo verified against the reference (P1) 🎯 MVP

**Goal**: convert the least-verified subsystem into a gated one.
**Independent test**: op streams interleaving edits with undo/redo/clear reach zero divergence,
including multi-participant streams and observable stack status.

### Tests for User Story 1 (REQUIRED — write FIRST, ensure they FAIL) ⚠️

- [X] T018 [US1] Write the undo differential in `undo_diff_test.go` against the registry — MUST fail before the ordering fix, since the current `(client, clock)` sort diverges on multi-participant streams (C-S1.1)
- [X] T019 [US1] Write stack-status comparison in `undo_diff_test.go` — undo/redo availability plus stack-change events. A phantom undo entry and a lost redo alter NO encoded bytes, so a bytes-only comparison cannot reach either; both occurred in 003 (C-S1.2, FR-001b)
- [X] T020 [US1] Write regression tests in `review_round_invariants_test.go` for the THREE undo defects already fixed in the tree — phantom entry from an empty transaction, redo lost to an empty transaction, tombstone resurrection on redo — each failing when its fix is reverted (FR-016, SC-008). The fourth known defect (`AddToScope` silently dropping slice arguments) is NOT fixed and is handled by T064; the constructor typed-slice panic it is sometimes confused with is already fixed and is covered by the existing `TestUndoManagerAcceptsConcreteTypedSliceScope`

### Implementation for User Story 1

- [X] T021 [US1] Add client first-insertion order tracking to `StructStore` in `struct_store.go` — an insertion-order client slice beside `Clients`, using the in-repo pattern from `type_define.go`'s `objectData`, NOT a third-party ordered map (R1, R4, Constitution III)
- [X] T022 [US1] Expose the store's ordered client list for the undo path in `struct_store.go`, leaving `GetStateVector`'s returned map shape unchanged so encoding paths are untouched (R1)
- [X] T023 [US1] In `undo_manager.go`, build the insertions delete set by iterating the store's insertion order and **remove** the `(client, clock)` sort at the redo-candidate site — it is a canonical order, not the reference's, and its justification ("the gate does not exercise undo") is removed by T018 (FR-001a)
- [X] T024 [US1] Update the deviation comment block in `undo_manager.go` to record that the order now follows the reference, replacing the 003 rationale rather than leaving contradictory text (Constitution IX)
- [X] T025 [P] [US1] Add the undo generator `fuzz/undo_gen.mjs` on `fuzz/harness/` — edits interleaved with undo/redo/clear, scope restriction, capture coalescing, and multi-participant streams (C-S1.1, C-S1.3)
- [X] T026 [US1] Ensure `fuzz/undo_gen.mjs` emits capture-window-coalesced cases — without a coalescing window `Content.Copy` is never reached, which is how tombstone resurrection survived 003 (C-S1.4)
- [X] T026a [P] [US1] Add the "undo across a document whose subdocuments were removed in the same session" case to `fuzz/undo_gen.mjs` — a spec edge case with no coverage otherwise (spec Edge Cases)
- [X] T026b [P] [US1] Add the "undo of an operation whose target was concurrently modified by a remote peer" case to `fuzz/undo_gen.mjs` — the last spec edge case with no dedicated coverage; T025's multi-participant streams only imply it (spec Edge Cases)
- [X] T027 [US1] Register the `undo` surface in `internal/oracle/surface.go` with its realized directions, applicable fault kinds, and `fast` membership for EVERY realized cell (C-H1, U3)

**Checkpoint**: US1 independently shippable. Undo surface at zero divergence; reverting T023 turns
it red.

---

## Phase 4: US2 — The library's own output is verified (P1)

**Goal**: exercise this library's constructors, which direction A structurally cannot reach.
**Independent test**: library-built documents round-trip byte-identically through the reference.

### Tests for User Story 2 (REQUIRED — write FIRST) ⚠️

- [X] T028 [US2] Write the direction-B differential in `dir_b_diff_test.go` — this library constructs and encodes, the reference decodes and re-encodes, bytes compared (FR-002, C-S0)
- [X] T029 [US2] Write the repeated-build determinism test in `dir_b_diff_test.go` — one hundred identical builds MUST produce one distinct encoding; before the 003 prelim fix the equivalent produced four across forty (SC-005)

### Implementation for User Story 2

- [X] T029a [US2] Include a container populated BEFORE attachment in the direction-B cases (`dir_b_diff_test.go`) — US2 AS3 requires it and US2 claims to be independently shippable, but the only prelim coverage was T046/T046a in Phase 6 (US2 AS3)
- [X] T030 [US2] Add `fuzz/dir_b_verify.mjs` on `fuzz/harness/` — reads library-generated updates, applies via `applyUpdateV2`, re-encodes via `encodeStateAsUpdateV2`, emits bytes for comparison (R2)
- [X] T031 [P] [US2] Extend direction B to snapshots in `fuzz/dir_b_verify.mjs` + `dir_b_diff_test.go` — library snapshot encoded here, `decodeSnapshotV2` + `encodeSnapshotV2` there, plus `createDocFromSnapshot` to prove semantic usability not just re-encodability (R2)
- [X] T032 [US2] Extend direction B to GC in `fuzz/dir_b_verify.mjs` + `dir_b_diff_test.go` — a gc-enabled document built and encoded by this library, applied and re-encoded by the reference (R2)
- [X] T033 [US2] As part of T032's acceptance, verify the GC surface still asserts a real invariant once direction B lands — the dead `typeof doc.gc === 'function'` branch was ALREADY removed in `cbafa17`, and `fuzz/generate.js` now builds `postGcState` from an independent gc=false replay. This is a verification step; the removal it previously described is done (C-S0)
- [X] T034 [US2] Register direction `B` on every applicable surface in `internal/oracle/surface.go` (C-H1)

**Checkpoint**: direction B green across surfaces; FR-006 (003's unmet requirement) closed.

---

## Phase 5: US3 — Every wire format verified (P1)

**Goal**: close the three encoded formats with zero differential coverage, where a divergence is a
silent interop break with real third-party clients.

### Tests for User Story 3 (REQUIRED — write FIRST) ⚠️

- [X] T035 [P] [US3] Write the relative-position differential in `relpos_diff_test.go` — encode here/resolve there and the reverse, including anchors whose target is later deleted then garbage-collected (C-S2)
- [X] T036 [US3] Write the sync-protocol differential in `sync_diff_test.go` — a full exchange driven message-by-message with each side alternately producing and consuming (C-S3)
- [X] T037 [US3] Write the awareness differential in `awareness_diff_test.go` — comparing emitted change/update payloads, not merely the resulting presence map (C-S4, FR-004)

### Implementation for User Story 3

- [X] T038 [P] [US3] Add `fuzz/relpos_gen.mjs` using `createRelativePositionFromTypeIndex`, `encodeRelativePosition`, `decodeRelativePosition`, `createAbsolutePositionFromRelativePosition`, `compareRelativePositions` (R3)
- [X] T039 [P] [US3] Add `fuzz/sync_gen.mjs` using `y-protocols/sync.js` `writeSyncStep1/2`, `writeUpdate`, `readSyncMessage` (R3)
- [X] T040 [P] [US3] Add `fuzz/awareness_gen.mjs` using `y-protocols/awareness.js` `encodeAwarenessUpdate`, `applyAwarenessUpdate`, `removeAwarenessStates` (R3)
- [X] T041 [US3] Add the "one side holds state the other cannot yet interpret" case to `sync_diff_test.go` (C-S3.3, spec edge case)
- [X] T042 [US3] Register `relpos`, `sync`, `awareness` surfaces in `internal/oracle/surface.go` with fault kinds, `fast` tier membership, **and BOTH directions** — T034 registered direction B in Phase 4, before these surfaces existed, so without this they land direction-A-only while FR-004 and the Phase-5 checkpoint both require both (FR-004)

**Checkpoint**: all three wire formats at zero divergence in both directions.

---

## Phase 6: US4 — The oracle exercises the whole public surface (P2)

### Tests for User Story 4 (REQUIRED — write FIRST) ⚠️

- [X] T042a [US4] Implement the exercised-op link in `internal/oracle/coverage.go` and `fuzz/harness/index.mjs` per research R7: generators emit the op names they produced, and a declared op-name → Go-method mapping is checked for exhaustiveness IN BOTH DIRECTIONS (an op with no mapped method, or a method no op maps to, fails). Without this, `Exercised` has no defined source and SC-003 rests on an undefined JS→Go join (FR-005a)
- [X] T043 [US4] Write the operation-coverage test in `oracle_coverage_test.go` — derived from the exported method set, failing on any operation no generator invokes (FR-005a, C-H4)

### Implementation for User Story 4

- [X] T044 [P] [US4] Add the six uncovered operations to `fuzz/ops.js` and the `fuzz/native_diff_*.mjs` generators — `InsertEmbed`, `YMap.Clear`, `YArray.Unshift`, `YArray.Splice`, `YXmlFragment.InsertAfter`, `RemoveAttribute` (FR-005)
- [X] T044a [US4] Add the matching Go-side replay/compare arms for every op T044 and T045 introduce — each differential has its OWN `case "insert"/"delete"/"format"` dispatch (`native_diff_test.go`, `native_arr_diff_test.go`, `native_map_diff_test.go`, …), so adding ops on the JS side alone leaves the Go replay unable to handle them and the new ops silently unexercised (FR-005)
- [X] T045 [US4] Add read/serialization operations to `fuzz/ops.js` and the `fuzz/native_diff_*.mjs` generators — delta, string and JSON rendering, attribute and value getters. Two known defects sit in read paths, so a mutating-only vocabulary excludes a proven class (FR-005)
- [X] T046 [US4] Remove the single-key restriction in `fuzz/native_diff_arr.mjs` and generate multi-key prelim maps — the comment notes a single key is order-independent so it stays byte-exact — a precaution that leaves prelim key ordering unexercised, i.e. the harness was shaped around a defect (FR-006, C-H5.2)
- [X] T046a [P] [US4] Add the "container populated before attachment and then NEVER attached" case to the generators — T046 covers multi-key prelim on an attached container, not the never-attached path (spec Edge Cases)
- [X] T047 [US4] Add a machine-readable narrowing marker (e.g. a required `NARROWING:` annotation) that generators must use to declare any deliberate coverage restriction, and have the coverage report enumerate them — US4 AS3 says "when the suite runs", which a one-time manual audit cannot satisfy and which would decay exactly like the hand-maintained list FR-005a forbids (FR-006, C-H5.2)
- [X] T047a [US4] Audit every `fuzz/*.mjs` and `fuzz/*.js` generator for existing narrowings and annotate each with the T047 marker, recording them in `specs/004-full-parity-coverage/research.md` (FR-006)

**Checkpoint**: SC-003 green; no operation unexercised.

---

## Phase 7: US5 — The gate proves itself on every surface (P2)

### Tests for User Story 5 (REQUIRED — write FIRST) ⚠️

- [X] T048 [US5] Write the fault-injection suite in `oracle_selftest_test.go` asserting 100% detection on EVERY registered surface, replacing the 003 version which covered one type and compared this library against itself (FR-007, SC-002)
- [X] T049 [US5] Write the "no exempt surface" test in `oracle_selftest_test.go` — a registered surface with zero applicable faults FAILS, since an unfaultable surface is an underbuilt harness (FR-008, C-H1.1)

### Implementation for User Story 5

- [X] T050 [US5] Implement `corrupt-expectation` in `internal/oracle/faults.go` as the universal kind — it generalises to any comparable artefact (bytes, rendered text, delta, event payload), which is what makes zero exempt surfaces achievable (R5)
- [X] T051 [P] [US5] Declare per-(surface, kind) applicability in `internal/oracle/faults.go` so an inapplicable kind is skipped explicitly, never silently counted as passing (C-H3.3)
- [X] T052 [US5] Wire detection through the reference comparison path in `internal/oracle/faults.go`, not this library's own output (FR-007, C-H3.1)

**Checkpoint**: SC-002 green with zero exempt surfaces.

---

## Phase 8: US6 — Awareness starts no thread unless asked (P2)

### Tests for User Story 6 (REQUIRED — write FIRST) ⚠️

- [X] T053 [P] [US6] Write plain-type tests in `awareness_test.go` — no goroutine created, expiry judged on access, no disposal call required, ten thousand created/discarded leave zero residual threads (C-P1, SC-006)
- [X] T054 [P] [US6] Write managed-type tests in `awareness_managed_test.go` — renewal AND reaping match the reference, same events and payloads, stopping leaves nothing behind (C-P2, SC-006)
- [X] T055 [US6] Write the renewal-parity test in `awareness_diff_test.go` — an otherwise-idle client is NOT dropped by a reference peer across a full timeout, the behaviour that makes removing the timer outright impossible (SC-006a, C-S4.3)
- [X] T056 [US6] Write race tests for both types under `-race` in `awareness_test.go` (SC-007, C-P3.2)
- [X] T056a [US6] Write the timeout-removal ordering test in `awareness_managed_test.go` asserting the emitted client order MATCHES THE REFERENCE (`y-protocols` `removeAwarenessStates`) — not merely that it is deterministic, since a sorted-but-different order satisfies "deterministic" and fails the awareness differential, repeating exactly the mistake FR-001a exists to correct. **Written here, before T058 implements it** — the previous arrangement had the fix in Phase 8 and its test in Phase 9, which is test-after-implementation (Constitution IV, FR-014c)

### Implementation for User Story 6

- [X] T057 [US6] Convert `awareness.go` to the plain type — re-export the state fields, remove the ticker/goroutine/stop channels, judge expiry on access (C-P1, FR-013)
- [X] T058 [US6] Create the managed type in `awareness_managed.go` — owns the ticker started only by an explicit call, reproduces the reference interval in full (renewal at half-timeout, reaping at full), accessors copying under its lock, AND removed-client ordering that MATCHES THE REFERENCE's (do NOT port the current Go-map ranging, and do NOT merely sort for determinism — FR-014c rejects that bar explicitly) (C-P2, FR-011a, FR-014c)
- [X] T059 [US6] Ensure `awareness.go` and `awareness_managed.go` make "exported fields plus a library-owned writer" unrepresentable — no single value exposes both (FR-012a, C-P3.1)
- [X] T060 [P] [US6] Update `protocol/awareness.go` comments and the non-test call sites identified by the T060a predicate to the new type names — bounded by that predicate rather than left open-ended (Constitution IX)
- [X] T059a [US6] Add the "presence entry expiring while an update for the same client is being applied" case to `awareness_diff_test.go` and `awareness_managed_test.go` — a spec edge case with no coverage otherwise (spec Edge Cases)
- [X] T060a [US6] Migrate or retire every file exercising the auto-started awareness API that T057 removes. Identify them by PREDICATE, not enumeration: `grep -rl 'NewAwareness(\|GetStates()\|GetMeta()' --include='*.go' .` — currently 15 files, of which an earlier hand-written list named only 8. **Phase 8 does not compile until all are migrated**, and an enumeration goes stale the moment a file is added, which is the same decay FR-005a rejects for the coverage report (Constitution IX)
- [X] T060b [US6] Decide and apply `WSSharedDoc`'s presence mode in `ws_shared_doc.go` — it is PRODUCTION code in the root package that builds `NewAwareness(sd.Doc)` and gets auto-reaping today. After T057 it would become thread-free with no local renewal, so reference peers would drop it: a websocket-backed shared document is exactly the interop case FR-011 warns about. **Default: switch it to `ManagedAwareness`, but the timer MUST be started by an explicit call on `WSSharedDoc` (e.g. `StartPresence()`), NOT by its constructor** — `NewWSSharedDoc` is a root-package constructor, so auto-starting would hand a consumer a library-owned goroutine without the explicit request FR-009 and Constitution II require, since its peers are by definition remote. Note its `On("update", ...)` handler then runs on the managed type's goroutine, so the handler must be safe to call off the app goroutine (FR-013a, FR-011a, Constitution II)
- [X] T061 [US6] Document the plain type's parity limitation at its declaration in `awareness.go` — no local renewal, so a quiet client is dropped by reference peers; parity claims attach to the managed type (FR-011, C-P2.5)

**Checkpoint**: no library-owned goroutine by default; managed type verified against a reference peer.

---

## Phase 9: US7 — Residual defects and documentation drift (P3)

- [X] T062 [P] [US7] Write a failing test then fix `YXmlFragment.ToString` in `y_xml_fragment.go` to render children of every supported kind — it currently drops non-XML children where the reference coerces each child; reuse the existing `xmlAttrValueString` coercion (FR-014)
- [X] T063 [P] [US7] Write a failing test then fix `DecodeSnapshotV2` in `snapshot.go` to error rather than silently reinterpret data written in the other encoding (FR-015)
- [X] T064 [P] [US7] Write a failing test in `undo_manager_phase1_test.go` then fix `AddToScope` in `undo_manager.go` to accept slice arguments — it currently `continue`s past anything that is not a shared type, so a slice is silently dropped. Distinct from the constructor typed-slice panic (already fixed); the normalization went into `NewUndoManager` instead of the shared primitive, the inverse of the reference (FR-014a)
- [X] T065 [P] [US7] Write a failing test then add the `[]uint8` case to `xmlAttrValueString` in `y_xml_element.go` — a binary attribute currently renders `"[1 2 3]"` where the reference gives `"1,2,3"`
- [X] T067pre [US7] Decide the countability semantics BEFORE the fix and record it in `specs/004-full-parity-coverage/research.md`: read the reference's content classes and state whether the Go equivalent is a `default:` arm or `IsCountable()`. The task previously said "or", which is two different semantics (FR-014d, Constitution XIII)
- [X] T067a [US7] Write FAILING tests first in `y_text_content_kind_test.go` for the currently-uncounted content kinds — index arithmetic and `ToDelta` output matching the reference, plus byte-exactness (SC-010). This was the ONLY residual task with no test, on the change its own requirement calls highest-risk (Constitution IV, XIV, FR-014d)
- [X] T067 [US7] Apply the T067pre decision to the content-kind whitelists in `y_text.go` — `ItemTextListPosition.Forward`, `FindNextPosition` and sibling `switch` sites (locate by symbol) — so uncounted content kinds stop shifting every later index (FR-014d)
- [X] T067b [US7] Gate T067 on the `text` AND `applyDelta` differentials at full-tier volume, not only the global Phase-10 run — an index-arithmetic change is precisely what those surfaces exist to catch (FR-014d, SC-010)
- [X] T068 [P] [US7] Correct stale reference-version claims found by PREDICATE, not enumeration: `grep -rn '13\.6\.2[0-9]' --include='*.go' --include='*.md' . | grep -v node_modules | grep -v '^./specs/00[123]'` — currently 9 files outside prior-feature specs (prior specs legitimately record what was true then) where a hand-written pair named 2. Distinguish genuine historical provenance (a comment about what 13.6.27 did) from stale claims about the CURRENT pin; change only the latter (FR-017)

**Checkpoint**: no identified-but-unfixed defect remains.

---

## Phase 10: Polish & Cross-Cutting

- [X] T069 Wire the three tiers into `.github/workflows/oracle.yml`, and update its `push.branches` list — it still names `003-oracle-and-value-rep` — fast on pull request, full on merge to mainline, scale on schedule (FR-021)
- [X] T070 Assert in `.github/workflows/oracle.yml` that the fast-tier job covers every realized (surface, direction) cell the registry declares, so a cell cannot silently become nightly-only — the 003 failure mode. The in-code invariant is T009; this is the CI half (FR-022, SC-001a)
- [X] T071 Update `.githooks/pre-push` to run the fast tier through the new entrypoint, keeping the corpus non-empty assertions and un-suppressed generator stderr
- [X] T071a Measure fast-tier wall-clock in CI and tune per-surface seed weights to land under the 10-minute hard ceiling (≈7 min target), adjusting weights and never cell membership (SC-001a)
- [X] T071b Add a mechanical per-cell floor check to the scale tier in `internal/oracle/surface.go` and `fuzz/run-gate.sh` — SC-001's ≥10,000-seed floor per realized cell currently has neither an in-code invariant nor a CI assertion (unlike fast-tier membership, which has both), so it rests on a human reading recorded volumes (SC-001)
- [X] T072 [P] Run the scale tier: ≥1e6 seeds aggregate across surface × direction with the ≥10,000-seed floor per realized cell, zero divergence; record per-surface volumes in `research.md` (SC-001)
- [X] T073 [P] `ORACLE_REQUIRED=1 go test ./... -race -count=1` clean across the repo root and `protocol/` (SC-007)
- [X] T074 [P] Re-run all byte-exact V1+V2 reference fixtures in `compatibility_test.go` and `compatibility_v2_test.go` — no interoperability regression (SC-010)
- [X] T075 [P] Re-run `BenchmarkDocOps` (`bench_test.go`) against `specs/004-full-parity-coverage/baseline.txt` with benchstat `-count=6`; pass rule is no metric BOTH significant (p<0.05) AND worse by >3% (SC-009)
- [X] T076 [P] `golangci-lint run` zero violations across the repo root and `protocol/` (Constitution VIII)
- [X] T077 Measure coverage via `go test -coverprofile` over the functions this feature TOUCHED across the repo root and `protocol/`, not only newly added ones, against the ≥95% floor — the 003 miss was measuring only added functions (FR-018, Constitution XIV)
- [X] T078 Finalize `research.md` (scale results, per-surface volumes), `data-model.md`, `quickstart.md`, `contracts/`
- [X] T078a Verify FR-016 bar (b) for every defect fixed in this feature: map each to the registered surface differential that would now catch a recurrence, and record in `specs/004-full-parity-coverage/research.md` any defect where no surface can reach it — as an explicit limit of the oracle's reach rather than an unstated one (FR-016)
- [X] T079 Record the honest coverage statement in `specs/004-full-parity-coverage/research.md`: which surfaces are proven, in which directions, and what remains outside the oracle's reach

---

## Dependencies

- **Phase 1 → Phase 2 → all stories.** Phase 2 is a hard prerequisite: it is the DRY extraction, and
  deferring it means rewriting every surface added afterwards.
- **T002 (fresh baseline) before any production change** (T021 onward), or SC-009 has no clean
  reference point.
- **T018–T020 (tests) before T021–T027 (implementation)** — Constitution IV. T018 must FAIL first;
  if it passes before the ordering fix, the differential is not exercising multi-participant order.
- **US1 → US2**: both P1, but US1's engine change is confined to ordering while direction B touches
  every surface, so US2 benefits from landing on a settled harness.
- **US4 depends on Phase 2** (`internal/oracle/coverage.go`) and lands best after US1–US3 exist, so the
  report covers the full surface set.
- **US5 depends on every surface being registered** (US1–US3), since it asserts across all of them.
- **US6 is independent** of US1–US5 and may land any time after Phase 2.
- **T042 must register BOTH directions** for the three wire surfaces — T034's direction-B pass runs
  in Phase 4, before those surfaces exist.
- **T060a before the rest of Phase 8 completes** — the existing awareness tests exercise the API
  T057 removes, so the package does not compile until they are migrated.
- **`[P]` means different files.** Tasks writing the same file are sequential even when logically
  independent — e.g. the paired test tasks that both land in one `_test.go`. Note also T062 reads
  `xmlAttrValueString`, which T065 changes: run T062 before T065 or expect a conflict.
- **T060 runs AFTER T060a**, not before: it updates the call sites T060a's predicate identifies, so
  it cannot precede the task that produces the list.
- **Phase 10 gates run last.**

## Parallel opportunities

- T003 alongside T001–T002.
- T010, T011, T012 in parallel (different files) once T008 exists.
- T019, T020 alongside T018.
- T031, T032 in parallel after T030.
- T035–T040 largely parallel — three independent surfaces, separate files.
- T044, T045 in parallel.
- T054 alone in parallel; T053 and T056 both write `awareness_test.go`, so they are sequential.
- T062–T068: parallel EXCEPT T062 before T065 (both touch `xmlAttrValueString`), and the T067* chain is sequential (decide → test → apply → gate).
- T072–T076 parallel in polish.

## Implementation strategy

**MVP = Phase 1 + Phase 2 + US1.** The harness core plus the undo differential is independently
valuable: it converts the least-verified subsystem — four found defects, zero coverage — into a
gated one, and it does so on a foundation the remaining surfaces reuse rather than duplicate.

**Natural split point if this proves too large**: after Phase 5 (US3). Harness + undo + direction B
+ wire formats is a coherent "close the coverage gaps" feature; presence redesign (US6) and residual
defects (US7) would become 005. Deciding this before implementation is cheaper than mid-flight.

## Definition of Done

SC-001 (zero divergence at scale, both directions) **and** SC-002 (100% fault detection, zero exempt
surfaces) **and** SC-003 (full operation coverage) **and** SC-004 (undo zero divergence incl. stack
status) **and** SC-005 (build determinism) **and** SC-006/SC-006a (no default thread; renewal parity)
**and** SC-007 (race-free) **and** SC-008 (every defect has a reverting test) **and** SC-009 (no perf
regression) **and** SC-010 (fixtures pass).

Correctness sign-off is the oracle — but scoped honestly: that claim holds for surfaces in the
registry, which is exactly why FR-008 and FR-022 forbid a surface being absent from fault injection
or from the fast tier.
