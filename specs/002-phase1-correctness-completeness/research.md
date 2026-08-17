# Phase 0 Research: Phase 1 — Correctness Completeness

Source basis: this `research.md` is the on-branch RCA, derived from `YGO-PHASE1-PLAN.md`
(gap-by-gap analysis, root causes re-verified against Yjs source — that planning doc lives in the
main checkout, not this branch) plus the clarified `spec.md`. **All `file:line` citations below are re-verified against the
locally-installed `yjs@13.6.31` source at implementation time (FR-035); they are the design
basis, not a substitute for that verification.** No `NEEDS CLARIFICATION` remained after the
clarify sessions.

---

## Cross-cutting decisions

### CC-1 — Reference version & source-of-truth protocol
- **Decision**: Pin the entire suite (existing fixtures, fuzz generator, new fixtures) to a
  single reference, **Yjs 13.6.31**, verified current on 2026-06-24 via npm registry +
  GitHub releases. Install `yjs@13.6.31` locally (in `fuzz/` / `v2_test_fixtures/`) and read its
  actual `src/*.js` for every behavior claim, cited `file:line`. Re-verify "still latest?" before
  the first implementation commit.
- **Rationale**: Constitution XII/XIII + FR-035 forbid relying on training data or docs; one
  reference version removes "which Yjs are we compatible with?" ambiguity. Wire is stable across
  13.6.27→.31, so regenerated fixtures should be byte-identical — any diff is a real finding.
- **Alternatives**: keep mixed 13.6.27/13.6.31 (rejected — ambiguous reference); trust
  `YGO-PHASE1-PLAN.md` citations without re-verifying (rejected — violates FR-035).

### CC-2 — Gate-first sequencing
- **Decision**: Land 1.8 (widened generators + harness, all five surfaces) first with only the
  nested-type strict assertion ON; every later fix flips exactly one `FUZZ_STRICT_*` flag in its
  own PR.
- **Rationale**: The gate is the regression oracle (Constitution V). Fix-and-proof land
  atomically; Phase 2's store rewrite then cannot silently regress any fixed surface.
- **Alternatives**: fix first, widen gate later (rejected — nothing proves the fix, and the plan
  warns Phase 2 could silently re-break it).

### CC-3 — Test discipline
- **Decision**: Each fix is regression-first (a test that fails before, passes after), ≥95%
  new-code coverage, meaningful (Constitution IV/XI/XIV). Wire-touching fixes also assert
  byte-parity and keep existing fixtures green.
- **Rationale**: spec FR-033; correctness library where untested = unverified.

### CC-4 — No new runtime dependency
- **Decision**: Phase 1 introduces no new runtime dep. The awareness reaper uses stdlib
  `sync.Mutex` + `time.Ticker`. `copystructure`/`mockey` removal stays Phase 2/3.
- **Rationale**: Constitution III; keep this phase scoped to correctness.

---

## Per-item research

### R-1.8 — Widen the cross-impl fuzz gate (prerequisite)
- **Root cause / gap**: today the gate exercises only 4 flat root types under insert/delete/
  format/set (`fuzz/ops.js`), asserting JSON convergence across V1/V2 apply. It never runs the
  code each Phase-1 fix touches (nested types, subdocs, GC cascade, snapshots, deep/Hook XML).
- **Decision**: Extend the single deterministic generator (`fuzz/generate.js`, `ops.js`,
  mulberry32 seed) — not fork it — with `FUZZ_FEATURES=nested,subdocs,xmlhook`,
  `FUZZ_GC=on|off|both`, `FUZZ_SNAPSHOT=1`, and assertion-strength flags
  (`FUZZ_STRICT_{SUBDOCS,GC,SNAPSHOT,XML}`) defaulting OFF. Add record fields (`subdocs`,
  `snapshotV1/V2`, `restoredState`, optional `postGcState`); keep `canonical.js` ↔ `fuzzCanon`
  in lockstep. Nested-types land strict immediately (cheapest, highest value; guards 1.1/1.2).
- **Rationale**: one generator reuses the V1/V2 dual-apply machinery; strict-off lets 1.8 merge
  green, proving harness wiring before grading any fix.
- **Alternatives**: separate scripts per feature (rejected — DRY/maintenance); assert TextDelta
  (currently parsed-not-asserted) — deferred, structural JSON convergence is sufficient here.
- **Acceptance**: generator emits all five areas; Go consumer decodes/compares; strict-off green
  on current base. Contract: see `contracts/fuzz-gate-contract.md`.

### R-1.1 — Subdocs (`ContentDoc` lifecycle)
- **Root cause**: `content_doc.go` — `ContentDoc.Integrate`/`Delete` are empty TODOs; the
  surrounding subdoc infra (`Doc.SubDocs`/`Load`/`Destroy`, `transaction.SubdocsAdded/Removed/
  Loaded`, cleanup/destroy) already exists but is never fed. `.GC` correctly stays a no-op
  (matches yjs).
- **Decision**: Port yjs `ContentDoc.integrate/delete` faithfully: `Integrate` sets
  `doc.Item`, adds to `SubdocsAdded`, and (if `ShouldLoad`) `SubdocsLoaded`; `Delete` removes
  from `SubdocsAdded` if present else adds to `SubdocsRemoved`. Fix `ReadContentDoc`/`NewDoc` so
  `ShouldLoad = shouldLoad || autoLoad` (so an autoLoad subdoc loads on integrate).
- **Rationale**: two-method completion on built infra; scoping subdocs out would leave dead code
  and break a real Yjs feature consumers may use (FR-008–011).
- **Alternatives**: document "subdocs unsupported" (rejected — higher net cost + dead code).

### R-1.2 — `ContentType.Delete` / GC cascade
- **Root cause**: `content_type.go` — `ContentType.Delete` is fully commented out; `.GC` is an
  empty TODO. So deleting a nested `YMap`/`YArray`/`YText` does not tombstone its children, and
  GC of a tombstoned nested type never replaces children with `GC` structs. Cascade machinery
  (`TryGcDeleteSet`, `ReplaceStruct`, `Item.GC(store,parentGCd=true)`, `trans.MergeStructs/
  BeforeState/Changed`) is all present.
- **Decision**: Port yjs `ContentType.delete` (walk `_start` linked list **and** `_map`; live
  child → `item.Delete(trans)`; tombstoned child with `clock < beforeState[client]` → append to
  `trans.MergeStructs`; finally `delete(trans.Changed, type)`) and `ContentType.gc` (walk start +
  map calling `item.GC(store,true)`, then null start / reset map).
- **Rationale**: faithful port restores convergence + correct GC (FR-005–007).
- **Note (reconciliation)**: `82536f7` already fixed `ContentType.Copy` (a *different* method);
  do not conflate. Verify `BeforeState[absent]==0`, `MergeStructs` element type, `SetMap`/
  `SetStartItem` exist on the type interface.

### R-1.3 — YText prelim + YTextEvent delta
- **1.3A root cause**: `y_text.go` `InsertText` dropped the yjs `currentAttributes` negation
  pre-pass ("golang no need" — incorrect): an unattributed insert inside a formatted run inherits
  formatting. The top-level `Insert` path partially masks it but carries values forward and is
  bypassed by `ApplyDelta`/`InsertEmbed`.
- **Decision (1.3A)**: restore the pre-pass at the top of `InsertText` using the existing `Null`
  sentinel: for each key in `currPos.CurrentAttributes` not present in incoming `attributes`, set
  `attributes[key] = Null`.
- **1.3B root cause**: `addOp` appends every op unconditionally (no `deleteLen>0`/non-empty-insert/
  `retain>0` guards) → spurious `{insert:""}`/`{delete:0}`/`{retain:0}`; and the content
  type-switch lacks a `*ContentType` case (so a nested type in a YText is absent from the delta) —
  same omission in `DeleteText` and `ToDelta`.
- **Decision (1.3B)**: gate each `addOp` branch (emit only when an op was produced) mirroring yjs;
  add `case *ContentType:` alongside `*ContentEmbed` in `GetDelta`, `DeleteText`, `ToDelta`, using
  `item.Content.GetContent()[0]` as payload.
- **Rationale**: FR-024–026; self-contained in `y_text.go`. Do 1.3A before 1.3B.

### R-1.4 — Snapshot V1/V2 split
- **Root cause**: `snapshot.go` — `EncodeSnapshot` passes a V1 encoder and there is no path that
  produces V2 bytes; `DecodeSnapshotV2` hardcodes the V1 decoder (a V2 snapshot cannot
  round-trip). `WriteStateVector`/`ReadStateVector` (`merge.go`) take the wide
  `UpdateEncoder`/`Decoder` whereas yjs uses DS encoders; distinct `DSEncoderV1`/`DSEncoderV2`
  already exist.
- **Decision**: Narrow `WriteStateVector`/`ReadStateVector` to `DSEncoder`/`DSDecoder` (they use
  only varint framing both DS encoders expose). Expose explicit `EncodeSnapshotV1/V2` +
  `DecodeSnapshotV1/V2`; `EncodeSnapshot`/`DecodeSnapshot` alias V1 (preserves the only safe
  wire-compatible default). Ensure `ReadDeleteSet`/`ReadStateVector` decode through the same
  DS-version decoder.
- **Rationale**: matches yjs's V2-default `encodeSnapshotV2`/`decodeSnapshotV2` and V1 explicit
  pair (FR-027–029). Narrowing the SV signatures is the DRY enabler.
- **Note**: snapshots require gc=false (`snapshot.go` rejects gc'd docs).

### R-1.5 — Deterministic ordering (already resolved → guard only)
- **Finding**: every yjs-deterministic encode path already sorts in Go (`WriteClientsStructs`,
  `WriteStateVector` descending; `WriteDeleteSet` descending; `IntegrateStructs` ascending);
  `_map` order is a non-issue (serialized via clock-ordered struct store, not `_map` walk).
  Resolved by the v2 hardening (`19c9e84`).
- **Decision**: no code change — add a regression **guard** test only: build a ≥3-key, ≥2-client
  `YMap`, encode update/SV/DS, assert byte-identical to a checked-in yjs fixture, run **100×** to
  defeat Go's randomized map iteration.
- **Rationale**: FR-031/SC-009; cheap insurance against a nondeterministic-order regression.

### R-1.6 — UndoManager completeness (G1–G7)
- **Root cause**: port of an older/smaller yjs UndoManager. Present: stack-by-pointer (`d6f92a3`),
  seed-left (`44bb5f2`). Remaining 7 gaps (two correctness-critical):
  - **G1 (HIGH)**: `RedoItem` missing `ignoreRemoteMapChanges` + undo/redo-stack conflict
    detection + the #757 cross-parent-left drop; `isDeletedByUndoStack`/`ignoreRemoteMapChanges`
    don't exist. → silent clobber of a concurrent remote map write on undo.
  - **G2 (HIGH)**: doc-level scope unsupported (`Scopes` can't hold a `*Doc`; `==doc` disjuncts
    missing; `GetDoc()` would nil-panic).
  - **G3**: capture-coalesce missing `lastChange>0` guard.
  - **G4**: missing fields/options `IgnoreRemoteMapChanges`, `CaptureTransaction`,
    `CaptureTimeout` (field), `CurrStackItem`.
  - **G5**: missing `stack-item-updated`/`stack-cleared` events; always emits `added`.
  - **G6**: `Clear` not selective; redo-discard skips `keepItem(false)` → GC-suppression leak;
    no `CanUndo`/`CanRedo`.
  - **G7**: no `Destroy`/lifecycle unbind (listener leak); no `AddToScope`/tracked-origin APIs.
  - plus a panic-safety nit: reset `undoing`/`redoing` via `defer`.
- **Decision**: add `isDeletedByUndoStack` (mirror yjs `Item.js`); extend `RedoItem` to the 6-arg
  form + port the map branch + #757 (**reconcile with `44bb5f2` seed-left → single code path**,
  G1 + FR-019); change `Scopes` to `IAbstractType|*Doc` + `==doc` disjuncts (G2); add the guard
  (G3); add fields/options (G4); `didAdd` tracking + new events (G5); selective
  `Clear(clearUndo,clearRedo)` + `CanUndo`/`CanRedo` + `keepItem(false)` on redo discard (G6);
  `Destroy()` unregistering the `afterTransaction` handler + tracked origins + `AddToScope` (G7).
  Internal order: **G4 → G1**, then G2/G3/G5/G6/G7.
- **Rationale**: FR-012–019; G1/G2 are data-loss/capability-critical; G6/G7 are real leaks.

### R-1.7A — Awareness reaper
- **Root cause**: `awareness.go` — the `time.AfterFunc(OutdatedTimeout/10, …)` reaper body is
  commented out; `RemoveAwarenessStates` is implemented but only called by a test. Symptom: stale
  presence never expires, and (no local renewal) correct peers reap this live node after the
  timeout. Latent unit bug in the dead comparison (`Duration` ns vs non-ns `lastUpdated`).
- **Decision**: replace with a stoppable goroutine + `time.Ticker(OutdatedTimeout/10)` that
  **auto-starts on `NewAwareness`** (clarified) and is stopped by `Destroy()` (signalled async via stopCh; NOT joined — a join self-deadlocks if an observer calls Destroy from the reaper goroutine; no leak). Per tick:
  (1) renew local if `now-meta[self].lastUpdated >= OutdatedTimeout/2`; (2) reap remotes with
  `now-lastUpdated >= OutdatedTimeout` via `RemoveAwarenessStates(..., "timeout")`. Add a
  `sync.Mutex` across all `States`/`Meta` mutators (`SetLocalState`/`ApplyAwarenessUpdate`/
  `RemoveAwarenessStates`/`GetStates`); make `GetStates` return a copy; gather decisions under the
  lock then release before emitting (avoid re-entrant deadlock). Reconcile the time unit to one
  basis.
- **Rationale**: FR-020–023; interop-safe default; Constitution II sanctions the awareness
  goroutine. Concurrency is the risk → `-race` + no-goroutine-leak tests.
- **Alternatives**: explicit `Start()` (rejected via Q1 — easy to forget, reintroduces the bug).

### R-1.7B — XML serialization (B1–B4)
- **Root cause**: `y_xml_text.go`/`y_xml_element.go` render attribute values, boolean/string
  formatting marks, embeds, and nil/array/object values divergently from yjs (`""` for non-string
  attrs; node emitted only when value is an Object; `%v` for nil/array/object).
- **Decision (DRY)**: one yjs-faithful `xmlAttrValueString(any) string` helper (`nil→"null"`,
  primitives via `%v`, arrays comma-joined, objects `[object Object]`); route both
  `YXmlElement.ToString` and `YXmlText.ToString` through it; drop the Object-only node guard.
- **Rationale**: FR-030; closes B1–B4 at one site (Constitution VII).
- **NOT bugs (do not "fix")**: self-closing/unclosed tags; XML entity-escaping (yjs does not
  escape — escaping would break byte-parity). `ToDOM` is out of scope (Phase 3/4).

---

## Open items intentionally deferred to implementation/tasks (not blocking)
- Exact `keepItem`/`AddToScope`/event-payload shapes — match yjs at implementation.
- Whether narrowing `WriteStateVector`/`ReadStateVector` ripples to other callers — confirmed
  during 1.4 (they only use the varint framing both DS encoders expose).
- `GetStates` copy semantics impact on existing callers — confirmed during 1.7A.
- Fuzz nesting depth/probability bounds — tuned in 1.8.
