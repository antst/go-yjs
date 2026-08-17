# Feature Specification: Phase 1 — Correctness Completeness

**Feature Branch**: `002-phase1-correctness-completeness`

**Created**: 2026-06-24

**Status**: Approved

**Input**: User description: "we want to develop phase 1, but we exclude renaming from the plan at this moment."

## Overview

This feature closes the remaining behavioral and wire-compatibility gaps between this
library and the reference Yjs implementation, as consolidated on-branch in
[research.md](./research.md) — derived from the `YGO-PHASE1-PLAN.md` / `YGO-DEVELOPMENT-PLAN.md`
planning docs, which live in the main checkout (untracked, not on this branch). Today the core
engine is byte-exact with
Yjs on the *exercised* paths (V1/V2 update, sync, awareness wire), but several real Yjs
features are partially or non-functional: nested subdocuments, deletion/garbage-collection
of nested collaborative types, rich-text formatting edge cases, snapshots in the V2
format, the undo/redo manager, automatic presence expiry, and XML string serialization.

The phase also widens the cross-implementation fuzz gate (the regression oracle) so every
fix is *proven* against Yjs and so later work (e.g. a performance rewrite) cannot silently
re-break any of these areas.

**Explicitly excluded from this phase** (per the user instruction and the plan's phasing):
the module/package **rename** and any change to the project's published identity. The work
proceeds under the current identity so the "upstream contribution vs. independent fork"
decision can be made later without rework.

## Clarifications

### Session 2026-06-24

- Q: When does the awareness presence reaper start? → A: Automatically on `NewAwareness` (Yjs parity); `Destroy()` stops it (async via stopCh; not joined, to avoid a self-deadlock if an observer calls Destroy from the reaper).
- Q: How many UndoManager gaps does Phase 1 close? → A: All 7 (G1–G7), per the Definition of Done — Phase 1 (below).
- Q: Which Yjs version is the byte-parity reference, and how are existing fixtures handled? → A: Standardize on 13.6.31 (verified current via npm registry + GitHub releases on 2026-06-24) everywhere; regenerate the existing 13.6.27 fixtures against it.
- Q: Does Phase 1 hold a no-performance-regression line, or defer perf entirely to Phase 2? → A: No-regression line — must not regress the bench (when available); performance improvement deferred to Phase 2.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Regression oracle covers every Phase-1 surface (Priority: P1)

As a maintainer, I need the cross-implementation fuzz gate to exercise nested types,
subdocuments, garbage collection, snapshots, and deep/XML-hook trees — not just the four
flat root types it covers today — so that each correctness fix is proven to converge with
Yjs and cannot be silently regressed by future changes.

**Why this priority**: This is the prerequisite for every other story. Without it, a fix
"passes" only against itself, not against Yjs, and the planned performance rewrite could
re-break any fixed area undetected. It must land first.

**Independent Test**: Run the widened gate on the current (pre-fix) codebase with all
strict assertions disabled; it generates and decodes cases across all five new areas and
is green — proving the generation/harness wiring independently of any fix.

**Acceptance Scenarios**:

1. **Given** the widened generator, **When** a randomized run is produced, **Then** it
   emits cases that nest collaborative types, insert/delete subdocuments, force garbage
   collection, capture snapshots, and build deep XML/hook trees.
2. **Given** a generated case, **When** the library applies it under both V1 and V2 (single
   and concurrent apply orders), **Then** the resulting document state matches the Yjs
   reference state for that case.
3. **Given** the gate with all `STRICT_*` assertions off, **When** it runs on the current
   base, **Then** it is green (harness proven before any fix is graded).
4. **Given** a later correctness fix, **When** its PR lands, **Then** exactly one new strict
   assertion dimension is switched on, so the fix and its proof land together.

---

### User Story 2 - Deleting nested collaborative types converges with Yjs (Priority: P1)

As a developer, when I delete a nested `Map`/`Array`/`Text` (or remove a key holding one),
its child content must be tombstoned, and — in a garbage-collected document — collected,
so that document state, JSON output, and re-encoded updates match Yjs exactly.

**Why this priority**: Today deleting a nested type is effectively a no-op on its children:
the children stay live, producing divergent state, leaked content, wrong JSON, and encode
divergence. This is a high-severity correctness defect on a common operation.

**Independent Test**: Build an `Array` containing a `Map` with several keys; delete the
`Map`; assert all child content is marked deleted and JSON output is empty; in a GC-enabled
document, assert the children are collected and the re-encoded update decodes identically.

**Acceptance Scenarios**:

1. **Given** a nested type with live children, **When** the parent item is deleted, **Then**
   every child (linked-list entries and map entries) is tombstoned.
2. **Given** a garbage-collected document, **When** a tombstoned nested type is collected,
   **Then** its children are replaced with collected placeholders and the re-encoded update
   is byte-equivalent to Yjs.
3. **Given** the same nested-delete sequence applied in this library and in Yjs, **When**
   both reach quiescence, **Then** post-GC state and re-encoded bytes converge across V1 and
   V2 apply orders.

---

### User Story 3 - Nested subdocuments are fully functional (Priority: P1)

As a developer embedding a `Document` inside another document's `Map`/`Array`, the
subdocument must be registered, loaded (when configured), and — on delete — removed and
destroyed, so that nesting documents (a supported Yjs feature) works and interoperates.

**Why this priority**: The surrounding subdocument infrastructure already exists but is
never fed, so nesting a document silently does nothing. Consumers that rely on subdocuments
(a real Yjs capability) are broken today; the completion is small and low-risk.

**Independent Test**: Insert a `Document` into a `Map`/`Array`; assert it appears in the
parent's subdocument set and (when configured to load) in the "loaded" notification; delete
it and assert it moves to "removed" and is destroyed.

**Acceptance Scenarios**:

1. **Given** a document inserted into a shared type, **When** the change is integrated,
   **Then** it is registered as a subdocument and added to the "added" set.
2. **Given** a subdocument configured to auto-load, **When** it integrates, **Then** it is
   reported as loaded.
3. **Given** an integrated subdocument, **When** it is deleted, **Then** it is reported as
   removed and destroyed (or, if it was only just added, simply withdrawn).
4. **Given** the same nested-document scenario in this library and Yjs, **When** both apply
   the update, **Then** the subdocument GUID set and each subdocument's JSON converge.

---

### User Story 4 - Undo/redo is safe and complete (Priority: P1)

As a developer using undo/redo in a collaborative session, undoing a local change must not
silently clobber a concurrent remote change to the same data, whole-document undo scopes
must be supported, and undo state must not leak memory, so that undo behaves as it does in
Yjs.

**Why this priority**: Two gaps are correctness-critical — undo can silently overwrite a
concurrent remote map write (data loss), and a whole-document undo scope is currently
impossible. The remaining gaps cause memory leaks (un-reclaimed items, an unremoved
lifetime listener) and missing/incorrect events.

**Independent Test**: Two clients concurrently set the same map key; the first client
undoes; assert the remote write is preserved by default and overwritten only when the
"ignore remote map changes" option is enabled, with final state byte-identical to Yjs.

**Acceptance Scenarios**:

1. **Given** a concurrent remote edit to the same map key, **When** a local edit is undone,
   **Then** the remote edit is preserved (default), and is overwritten only when "ignore
   remote map changes" is set.
2. **Given** an undo manager scoped to the whole document, **When** a nested edit is undone,
   **Then** the edit is reverted (no failure).
3. **Given** an explicit stop-capturing between two edits, **When** undo runs, **Then** the
   two edits occupy separate undo entries; without it, coalesced edits form one entry and
   emit an "updated" (not a second "added") event.
4. **Given** a fresh local edit after an undo, **When** the redo stack is discarded, **Then**
   discarded items are unpinned so they can be garbage-collected.
5. **Given** an undo manager that is destroyed, **When** the document is mutated afterward,
   **Then** no new undo entries are created and no listener remains registered.

---

### User Story 5 - Presence expiry and self-renewal work against real peers (Priority: P2)

As a developer using the awareness/presence protocol, stale remote presence must expire
automatically and this node's own presence must renew before peers time it out, so that
ghost cursors disappear and a live node does not vanish to browser peers after idle time.

**Why this priority**: The automatic expiry timer is disabled. Two user-visible symptoms:
stale presence never expires, and — because local renewal is missing — correct remote peers
remove this node after the idle timeout even though it is alive. This specifically breaks
interop with real browser presence clients.

**Independent Test**: With a shortened/injected clock, assert (a) a stale remote presence is
removed and a "removed" notification fires, and (b) the local presence timestamp advances on
the renew tick so the local node is not reaped.

**Acceptance Scenarios**:

1. **Given** a remote presence idle beyond the outdated timeout, **When** the reaper runs,
   **Then** that presence is removed and a "removed" change is emitted.
2. **Given** a live local presence approaching half the outdated timeout, **When** the renew
   tick runs, **Then** the local timestamp is bumped so peers keep this node.
3. **Given** repeated create-and-destroy of awareness instances, **When** measured, **Then**
   no background worker is leaked and concurrent access is race-free.
4. **Given** a newly created awareness instance, **When** no explicit start call is made,
   **Then** the reaper is already active (renew + reap running) and `Destroy()` stops it.

---

### User Story 6 - Rich-text formatting and embedded types are correct (Priority: P2)

As a developer using rich text, an unformatted insert inside a formatted run must not
inherit the surrounding formatting, change notifications must not contain empty/no-op
operations, and a collaborative type embedded in text must appear in the change delta, so
that text deltas match Yjs.

**Why this priority**: Causes visible formatting bleed (e.g. text inserted into a bold run
becomes bold) and silently drops embedded nested types from change notifications. Medium
severity, self-contained.

**Independent Test**: Format a range bold, then insert unformatted text into it via a delta;
assert the inserted text is a separate, unformatted operation matching Yjs's delta output.

**Acceptance Scenarios**:

1. **Given** a formatted run, **When** unattributed text is inserted into it, **Then** the
   inserted text is unformatted (formatting is reset, not inherited).
2. **Given** an observer on a text type, **When** operations that net to nothing occur,
   **Then** the emitted delta contains no empty-insert, zero-delete, or zero-retain ops.
3. **Given** a nested type inserted into a text type, **When** the change is observed,
   **Then** the delta contains one insert operation carrying that nested type.

---

### User Story 7 - Snapshots round-trip in both V1 and V2 (Priority: P2)

As a developer using document snapshots, both the V1 and V2 snapshot formats must
encode/decode and round-trip correctly, and V2 snapshot bytes must match Yjs, so that
snapshots produced or consumed across implementations interoperate.

**Why this priority**: The V2 snapshot path cannot round-trip today (the "V2" decode reads
V1 bytes), so any V2 snapshot is unusable. Medium severity, isolated to the snapshot path.

**Independent Test**: For a multi-client document, assert that decode-of-encode reconstructs
the snapshot for both V1 and V2, and that V2 bytes match a Yjs-produced fixture.

**Acceptance Scenarios**:

1. **Given** a multi-client document snapshot, **When** it is V2-encoded then V2-decoded,
   **Then** the original snapshot (delete set + state vector) is reconstructed.
2. **Given** the same snapshot, **When** V1-encoded then V1-decoded, **Then** it round-trips
   (no regression of the existing default).
3. **Given** a Yjs-produced V2 snapshot, **When** this library decodes it, **Then** it
   restores correctly; and this library's V2 snapshot bytes equal Yjs's for a fixture.

---

### User Story 8 - XML string serialization matches Yjs (Priority: P3)

As a developer serializing XML types to a string, attribute values, formatting marks, and
embeds must render exactly as Yjs renders them, so that string output is byte-identical
across implementations.

**Why this priority**: Several parity bugs (non-string attribute values rendered empty;
boolean/string formatting marks dropping their wrapper tag; divergent rendering of
nil/array/object values). Parity-affecting but lower user-facing impact than the data-loss
defects above.

**Independent Test**: Build matched XML element/text/fragment trees (nested elements,
formatted text with object/boolean/numeric marks, an embed, a null attribute, a hook) in
this library and Yjs; assert string output is byte-identical.

**Acceptance Scenarios**:

1. **Given** an XML text node with a non-string attribute value, **When** serialized,
   **Then** the value renders as Yjs renders it (not as empty string).
2. **Given** a boolean/string formatting mark, **When** serialized, **Then** its wrapping
   tag is emitted (as in Yjs).
3. **Given** null/array/object attribute values, **When** serialized, **Then** each renders
   identically to Yjs.

---

### User Story 9 - Deterministic multi-key ordering is guarded (Priority: P3)

As a maintainer, deterministic emission order for multi-key/multi-client documents must be
guarded by a regression test, so that the (already-correct) ordering cannot regress due to
the language's randomized map iteration.

**Why this priority**: Already resolved by prior hardening; this is a low-cost regression
guard only, not a fix. Included for completeness of the Phase-1 Definition of Done.

**Independent Test**: Build a multi-key, multi-client `Map`; encode update/state-vector/
delete-set; assert byte-identical to a checked-in Yjs fixture, repeated many times to defeat
nondeterministic iteration order.

**Acceptance Scenarios**:

1. **Given** a multi-key, multi-client document, **When** it is encoded 100 times, **Then**
   every run produces byte-identical output.
2. **Given** that output, **When** compared to a Yjs fixture, **Then** it is byte-identical.

---

### Edge Cases

- **Garbage collection on vs. off**: nested-delete behavior must converge with Yjs for both
  GC-enabled and GC-disabled documents; snapshots are only meaningful for GC-disabled docs.
- **Absent client in prior state**: a child whose client has no prior-state entry is treated
  as clock 0 when deciding tombstone vs. merge (matching Yjs).
- **Auto-loading subdocuments**: a subdocument configured to auto-load must be reported as
  loaded on integration, not merely added.
- **Empty/zeroed text operations**: a sequence of operations that nets to nothing must
  produce a delta with no operations, not a delta of empty ops.
- **Nested type inside text**: a collaborative type embedded in a text type must appear in
  deltas, deletion handling, and full-delta export — not only plain embeds.
- **Presence clock units**: the local-renew vs. remote-reap decision must use a single,
  consistent time unit (the latent unit-mismatch in the disabled timer must be reconciled).
- **Concurrent apply orders**: every convergence assertion must hold across single and
  concurrent apply, in both V1 and V2.
- **No-leak on destroy**: destroying an awareness instance or undo manager must remove its
  background worker/listener with no goroutine or registration leak.

## Requirements *(mandatory)*

### Functional Requirements

**Regression oracle (Story 1)**

- **FR-001**: The cross-implementation fuzz gate MUST generate and check cases covering
  nested collaborative types, subdocuments, garbage collection (on and off), snapshots (V1
  and V2), and deep XML / XML-hook trees, in addition to the existing flat root types.
- **FR-002**: The gate MUST verify that generated cases converge with the Yjs reference
  across V1 and V2 apply, single and concurrent, using a single deterministic generator
  (not forked scripts) seeded reproducibly.
- **FR-003**: Each new assertion dimension MUST be independently toggleable (default off), so
  the widened gate is green on the current base and each fix turns on exactly its own
  assertion when it lands — **except nested-types**, which lands strict *with* the gate itself
  (work item 1.8): it proves the harness and has no separate fix to gate.
- **FR-004**: The entire suite — the existing V1/V2 byte-exact fixtures, the fuzz generator,
  and all new fixtures — MUST use a single pinned reference version, **Yjs 13.6.31** (verified
  current on 2026-06-24). The existing 13.6.27 fixtures MUST be regenerated against 13.6.31; a
  resulting byte diff (if any) MUST be investigated as a real finding, not silently accepted.

**Nested-type deletion & GC cascade (Story 2)**

- **FR-005**: Deleting a nested collaborative type MUST tombstone all of its children (both
  sequence entries and key entries), matching Yjs.
- **FR-006**: When a tombstoned nested type is garbage-collected, its children MUST be
  replaced with collected placeholders so that the re-encoded update matches Yjs.
- **FR-007**: Post-deletion JSON output of a deleted nested type MUST be empty, and post-GC
  state plus re-encoded bytes MUST converge with Yjs for GC-on and GC-off documents.

**Subdocuments (Story 3)**

- **FR-008**: Integrating a document nested in a shared type MUST register it as a
  subdocument and add it to the "added" set.
- **FR-009**: A subdocument configured to load (including auto-load) MUST be reported as
  loaded on integration.
- **FR-010**: Deleting an integrated subdocument MUST move it to "removed" and destroy it; a
  subdocument deleted in the same transaction it was added MUST simply be withdrawn.
- **FR-011**: Garbage collection of a subdocument-bearing item MUST remain a no-op on the
  subdocument content, matching Yjs.

**Undo/redo (Story 4)**

- **FR-012**: Undoing a local change MUST NOT revert a concurrent remote change by default;
  an "ignore remote map changes" option MUST exist to opt into the previous behavior.
- **FR-013**: The undo manager MUST support a whole-document scope (not only individual
  shared-type scopes).
- **FR-014**: Capture coalescing MUST match Yjs (including the guard that prevents an initial
  spurious coalesce), so explicit stop-capturing yields separate entries.
- **FR-015**: The undo manager MUST emit the full event set used by Yjs, including
  "stack-item-updated" on coalesce/merge and "stack-cleared" on clear, rather than always
  emitting "added".
- **FR-016**: Discarding the redo stack on a fresh edit MUST unpin discarded items so they
  can be garbage-collected (no slow leak), and selective clearing of undo vs. redo MUST be
  supported with corresponding "can undo / can redo" queries.
- **FR-017**: The undo manager MUST be destroyable, unregistering its document listener and
  tracked origins, with no listener leak; "add to scope" and tracked-origin management MUST
  exist.
- **FR-018**: Undo/redo in-progress flags MUST reset even if an operation fails partway.
- **FR-019**: Undo/redo changes already merged upstream MUST NOT be reverted or
  re-implemented; the overlapping redo/seed-left logic MUST resolve to a single code path.

**Awareness / presence (Story 5)**

- **FR-020**: A stale remote presence (idle beyond the outdated timeout) MUST be removed
  automatically, emitting a "removed" change/update.
- **FR-021**: The local presence MUST renew automatically before the outdated timeout so
  that correct peers do not reap a live node.
- **FR-022**: The automatic-expiry time comparison MUST use a single consistent time unit
  (reconciling the existing unit mismatch).
- **FR-023**: The presence reaper MUST start automatically when an awareness instance is
  created (Yjs parity) — no explicit start call is required — and run as a background worker;
  concurrent access to awareness state MUST be race-free; and `Destroy()` MUST stop the reaper
  worker with no goroutine leak (signalled async; not joined, to avoid a self-deadlock when an
  observer calls Destroy from the reaper).

**Rich text (Story 6)**

- **FR-024**: An unattributed insert inside a formatted run MUST reset (negate) the
  surrounding formatting on the inserted content, matching Yjs.
- **FR-025**: Emitted text deltas MUST omit empty-insert, zero-delete, and zero-retain
  operations.
- **FR-026**: A nested collaborative type embedded in text MUST appear as an insert in change
  deltas, in deletion handling, and in full-delta export — not only plain embeds.

**Snapshots (Story 7)**

- **FR-027**: Both V1 and V2 snapshot encode/decode MUST round-trip correctly for
  multi-client documents.
- **FR-028**: This library's V2 snapshot bytes MUST equal Yjs's for fixture documents, and it
  MUST correctly decode a Yjs-produced V2 snapshot.
- **FR-029**: The existing V1 snapshot default behavior MUST be preserved (no regression).

**XML serialization (Story 8)**

- **FR-030**: XML element and text string serialization MUST render attribute values,
  formatting marks (including boolean/string marks and their wrapper tags), embeds, and
  null/array/object values byte-identically to Yjs.

**Ordering guard (Story 9)**

- **FR-031**: A regression test MUST assert byte-identical encoding output across repeated
  runs and against a Yjs fixture for multi-key/multi-client documents.

**Cross-cutting (constitutional)**

- **FR-032**: Every fix MUST preserve existing V1 and V2 byte-exact compatibility; no change
  may introduce a wire divergence (Constitution Principle I).
- **FR-033**: Every fix MUST ship with tests in the same change, including a regression test
  that fails before and passes after, reaching at least 95% statement coverage on new/changed
  code (Constitution Principles IV, XI, XIV).
- **FR-034**: Each fix MUST be traceable to its root cause and described with a clear
  what/why/how; no speculative edits (Constitution Principles VI, XV).
- **FR-035**: Every reference to Yjs behavior MUST be verified against the actual pinned Yjs
  source (`yjs@13.6.31`, installed/fetched for reading) and cited by `file:line` — never
  inferred from training data or secondary documentation. The "current" reference version
  MUST be re-verified online before it is pinned or bumped (Constitution Principles XII, XIII).

### Key Entities

- **Document**: A collaborative document; may contain shared types and nested subdocuments;
  has a garbage-collection mode.
- **Subdocument**: A `Document` nested inside another document's shared type; has a
  load/auto-load lifecycle and add/load/remove/destroy states.
- **Shared types**: `Map`, `Array`, `Text`, and XML types (`Fragment`, `Element`, `Text`,
  `Hook`) that hold collaborative content and may nest other shared types.
- **Item / content**: The internal building blocks whose deletion and garbage collection
  must cascade into nested content.
- **Update (V1/V2)**: The binary change payload; must remain byte-exact with Yjs.
- **State vector / delete set**: Document metadata encoded with version-specific framing;
  central to the snapshot fix.
- **Snapshot**: A point-in-time document marker, encodable in V1 and V2.
- **Awareness state**: Per-client presence with a last-updated timestamp subject to renewal
  and expiry.
- **Undo/redo stack item**: A unit of reversible change with associated metadata, scope, and
  origin tracking.
- **Fuzz case record**: A generated scenario plus reference outputs (state, subdocuments,
  snapshots, restored state) used by the gate.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: 100% of generated fuzz cases across all five new areas (nested types,
  subdocuments, GC, snapshots, XML/hook) converge with the Yjs reference across V1 and V2
  apply, single and concurrent, with every strict assertion enabled (the subdoc-GC no-op,
  FR-011, is intentionally unit-gated, not part of `FUZZ_STRICT_GC`).
- **SC-002**: Deleting a nested collaborative type leaves zero live child content (empty JSON)
  and, under garbage collection, a re-encoded update that is byte-identical to Yjs in 100% of
  tested cases.
- **SC-003**: A nested subdocument is correctly added, loaded, removed, and destroyed, and its
  GUID set plus per-subdocument JSON converge with Yjs in 100% of tested nesting scenarios.
- **SC-004**: Undoing a local edit preserves a concurrent remote edit by default (0% data
  loss) and reverts it only when explicitly opted in; whole-document undo succeeds; final
  state is byte-identical to Yjs.
- **SC-005**: A live node remains visible to reference presence peers after remaining idle
  beyond the outdated timeout, and stale remote presence is removed within one reaper interval
  of the timeout.
- **SC-006**: Unattributed text inserted into a formatted run is unformatted in 100% of cases,
  and emitted deltas contain zero empty/no-op operations.
- **SC-007**: V1 and V2 snapshots round-trip in 100% of tested cases, and V2 snapshot bytes
  equal the Yjs reference for every fixture.
- **SC-008**: XML string serialization is byte-identical to Yjs across the full parity table.
- **SC-009**: Encoding a multi-key/multi-client document 100 consecutive times yields
  byte-identical output every time, matching the Yjs fixture.
- **SC-010**: The existing V1 and V2 byte-exact compatibility fixtures remain green, the
  awareness fix passes the race detector, and create/destroy cycles leak no background workers
  or listeners.
- **SC-011**: New and changed code reaches at least 95% statement coverage with meaningful
  tests (no coverage padding).
- **SC-012**: Phase 1 introduces no measurable performance regression against the pre-phase
  baseline on the bench suite (when the Phase 0.3 bench gate is available); performance
  improvement is explicitly out of scope. If that bench gate does not yet exist this phase,
  SC-012 is a consciously accepted, documented gap (recorded per the Phase-1 tasks), not a
  silent omission.

## Assumptions

- **Rename excluded**: The module/package rename and any change to the published project
  identity (`YGO-DEVELOPMENT-PLAN.md` Phase 0.1) are **out of scope** for this phase, per the
  user instruction. Work proceeds under the current identity; downstream consumers continue
  via their existing local replacement, and the upstream-vs-fork decision is deferred.
- **Base includes the V2 work**: This phase builds on the merged V2 encoding/sync work and the
  nine already-merged upstream fixes; none of those fixes is reverted or re-implemented.
- **Reference version**: The single byte-parity reference is **Yjs 13.6.31** — verified as the
  current latest on 2026-06-24 via the npm registry and GitHub releases (not from training
  data). All fixtures and the fuzz generator pin this version; the existing 13.6.27 fixtures are
  regenerated against it. All behavior claims are verified against the actual `yjs@13.6.31`
  source, installed locally for reference; the version is re-verified before any future bump.
- **Subdocuments are implemented, not scoped out**: Because the surrounding infrastructure
  already exists and the completion is small and low-risk, subdocuments are implemented rather
  than documented as unsupported.
- **UndoManager scope**: All seven enumerated undo gaps are addressed in this phase (the two
  correctness-critical ones first), per the Definition of Done — Phase 1 (below).
- **Out of scope (later phases / parity-preserving)**: error-return threading on apply,
  removal of the panic-recovery boundary, unexporting internals (all Phase 3); DOM
  serialization and XML entity-escaping (would break byte-parity with Yjs, which does not
  escape).
- **Performance posture**: This phase is correctness-only — performance *improvement* is Phase
  2 and depends on this phase's gated, correct base. However, Phase 1 MUST NOT knowingly regress
  performance: changes run under the Phase 0.3 bench gate when it is available and hold a
  no-regression line against the pre-phase baseline. Phase 1 is not blocked on building the
  bench harness.
- **Concurrency contract**: Adding the awareness background worker requires guarding shared
  awareness state; this is treated as part of the awareness fix, not a separate effort.

## Definition of Done — Phase 1

Phase 1 is complete when **all** hold:

1. **Every gap fixed or explicitly scoped**: subdocs implemented (1.1); `ContentType.Delete`/`GC`
   cascade (1.2); `InsertText` negation pre-pass + `YTextEvent` delta guards + `ContentType` case
   (1.3); snapshot V1 **and** V2 round-trip + byte-match (1.4); ordering regression guard (1.5);
   the seven UndoManager gaps closed and reconciled with merged fixes (1.6); awareness reaper +
   the four XML serialization bugs (1.7); plus the fuzz-gate widening (work item 1.8, in §2
   below) — so the full set is 1.1–1.8. `ToDOM` and XML entity-escaping are explicitly out of
   scope.
2. **Widened fuzz gate GREEN with every `FUZZ_STRICT_*` ON** — subdocs, GC, snapshot, XML, and
   nested types converge with Yjs across V1 and V2, single and concurrent (SC-001). (The
   subdoc-GC no-op, FR-011, is unit-gated, not part of `FUZZ_STRICT_GC` — so "all areas, every
   strict assertion" does not imply fuzz coverage of subdoc-GC.)
3. **Byte-parity maintained** — existing V1+V2 fixtures stay green; snapshot-V2 and XML gain new
   byte-parity fixtures; no change introduces a wire divergence (FR-032).
4. **No new flakiness** — the ordering test runs 100× green; the awareness fix passes `-race` and
   the no-goroutine-leak test.
5. **Coverage** — ≥95% on new/touched code, with meaningful tests (SC-011, Principle XIV).
6. **Performance** — no regression vs the pre-phase baseline on the bench suite when available
   (SC-012); improvement is out of scope.
7. **Upstream reconciliation honored** — no merged upstream fix is reverted/re-implemented; the
   seed-left/`redoItem` overlap is a single code path (FR-019).
