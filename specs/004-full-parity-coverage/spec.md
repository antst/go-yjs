# Feature Specification: Full Parity Coverage & Awareness Reaper Redesign

**Feature Branch**: `004-full-parity-coverage`

**Created**: 2026-08-13

**Status**: Planned (spec clarified ×3, plan + tasks complete, checklist 16/16)

**Input**: User description: "lets do now proposed spec 004 which address what has not been addressed, and also include into it: 'Only by removing the reaper goroutine and reaping lazily on access instead — then the fields could stay exported safely. That's a real option, and notably spec.md already calls the reaper goroutine an undesigned deviation that cost 23 churn commits. But it's a bigger design change than a rename, so I took the narrower fix. Say the word if you'd rather go that direction — it would restore source compatibility and delete a goroutine.'"

## Overview

Feature 003 established a differential oracle as the correctness gate and proved the engine
byte-exact with `yjs@13.6.31` across 1.1M native-op seeds. It merged **not complete by its own
Definition of Done**, and a subsequent review found eleven defects in code the gate had been green
over — including two crashes in default configuration and a data race.

The stated goal is **100% functional parity** with the JavaScript reference. The 003 oracle cannot
establish that, and no increase in seed count changes it: coverage is a property of the
**generators**, not the corpus size. The measured gaps (recorded in
`specs/003-oracle-and-value-rep/research.md`, "Oracle coverage-gap analysis") are:

- All seven generators together emit **five op kinds** — `insert`, `delete`, `set`, `format`,
  `setAttr`. That is the entire vocabulary behind "1.1M seeds, 0 divergence".
- Three of the four wire formats have **zero** differential coverage: relative positions, the sync
  protocol, and awareness. A divergence in any of them is a silent interop break with real Yjs
  clients that nothing would currently detect.
- Undo/redo has **zero** coverage, yet four confirmed defects were found there by hand.
- Direction B is unimplemented, so the library's own constructors are never exercised against the
  reference — which is where an encoding nondeterminism defect hid.

This feature closes those gaps, and separately resolves the awareness reaper: 003 stopped a data
race by hiding the awareness state maps behind accessors. The underlying cause is an always-on
background timer that the 003 specification itself calls an *undesigned deviation* responsible for
23 churn commits. Making that timer **opt-in** removes it from every consumer that does not need
it, and lets the plain type expose plain struct fields again with no library-scheduled writer.
It cannot be removed outright: the reference's timer also performs local renewal — an outbound
heartbeat that keeps reference peers from dropping this client — and nothing triggered by reads can
reproduce something triggered by elapsed time.

## Clarifications

### Session 2026-08-13

- Q: Does "100% functional parity" include behaviour no wire format can observe (event ordering,
  observer status)? → A: Yes, but targeted rather than universal. Where the event or status IS the
  contract — undo stack availability and its events, awareness change/update — it is compared
  directly. Elsewhere, events on ordinary edits are derivable from the state already compared, so
  they are not separately instrumented. This is not a thoroughness compromise: two of the four undo
  defects found in 003 (a phantom undo entry, a lost redo) change NO document bytes, so a
  bytes-only comparison cannot reach them by construction.
- Q: Is a defect found by review rather than by the oracle a failure of this feature? → A: No, but
  every defect found by any means MUST be converted into oracle coverage, so the same class cannot
  recur undetected. This is the feature's core convergence rule (FR-016).
- Q: Does generator coverage include read/serialization operations, or only mutating ones? → A:
  Every public operation producing observable output, read paths included. Two known defects sit in
  read paths — a delta rendering that omitted its attribute-presence flag, and a text rendering that
  silently drops children of unexpected kinds — so a mutating-only rule excludes a proven defect
  class. Read paths are also the cheapest to compare, needing no new op kinds. The coverage report
  MUST derive from the API surface rather than a hand-maintained list, so a newly added method
  fails the report instead of silently escaping it.
- Q: FR-008 allowed a surface with no applicable fault injection to be listed "unproven", but
  SC-002 requires zero unproven surfaces — which wins? → A: SC-002. The escape hatch is removed. A
  surface that appears unfaultable is a harness gap to close, not an exception to record: any
  surface producing comparable output can be faulted by corrupting the expected value. Permitting
  "unproven" recreates the 003 failure of a surface counted as covered without being exercised.
- Q: With the presence fields exported again, what prevents a consumer reading them directly while
  the managed type's writer is running — a Go map race, which aborts the process? → A: Two types.
  A plain presence type that owns no thread and exposes exported fields, and a separate managed
  type that owns the timer and exposes accessors only. The unsafe combination becomes
  unrepresentable rather than merely documented; relying on a comment to prevent a fatal abort is
  the reasoning that produced the original race.
- Q: What wall-clock budget should the per-PR gate hold to, given this feature roughly triples the
  surface matrix? → A: Three tiers — a fast per-PR gate (~7 min), a full gate on merge to main, and
  the scale run nightly. Binding constraint: EVERY surface keeps some per-PR enforcement, however
  thin. A surface with no PR-time coverage is exactly how the 003 hollow gate survived.
- Q: Is the awareness background thread removed outright, or removed by default with an explicit
  opt-in? → A: Opt-in (Constitution II already carves this out). The reference's timer does two
  things, and only one can be made lazy: reaping is a read-time judgement, but local renewal is an
  outbound heartbeat triggered by elapsed time. Removing the thread outright would stop the
  heartbeat and get this client dropped by reference peers — trading a data race for an interop
  bug. Default mode is thread-free with expiry-on-access; the managed type reproduces the timer in
  full, and presence parity claims attach to that mode.
- Q: Does verifying undo include fixing the ordering deviation carried over from 003, or should the
  undo comparison accommodate it? → A: Fix it. The deviation was accepted on the explicit grounds
  that "the cross-impl gate does not exercise undo"; this feature removes that premise, and
  accommodating it would be the same coverage-narrowing FR-006 forbids.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Undo/redo is verified against the reference (Priority: P1)

As a maintainer, I need undo and redo compared against the JavaScript reference the same way
document edits already are, so that the undo subsystem stops being a place where defects live
undetected.

**Why this priority**: Highest measured yield. Undo/redo has zero differential coverage today, and
four confirmed defects were found there by hand during 003 review — a phantom undo entry, silent
loss of a valid redo, resurrection of deleted content on redo, and a crash on a legitimate scope
argument. Every one was invisible to a green oracle at 1.1M seeds.

**Independent Test**: Generate random edit sequences interleaved with undo/redo/clear operations,
apply them in both implementations, and compare resulting state. Delivers value alone: it converts
the least-verified subsystem into a gated one.

**Acceptance Scenarios**:

1. **Given** a random op stream interleaved with undo and redo operations, **When** it is applied
   natively in both implementations, **Then** the resulting encoded state is byte-identical.
2. **Given** an undo manager restricted to a subset of the document, **When** edits occur both
   inside and outside that subset, **Then** only the in-scope edits are reverted, identically in
   both implementations.
3. **Given** a capture window that coalesces several edits into one undo step, **When** undo is
   followed by redo, **Then** content deleted before the undo remains deleted in both.
4. **Given** an operation that produces no structural change, **When** it is applied, **Then**
   neither implementation gains an undo entry nor loses an available redo.
5. **Given** content deleted by more than one participant, **When** those deletions are undone,
   **Then** both implementations restore them in the same order and reach identical state — the
   comparison is made with no ordering carve-out.

---

### User Story 2 - The library's own output is verified, not just its input (Priority: P1)

As a maintainer, I need op streams generated by this library — not only by the reference — so that
code which runs when the library *constructs* documents is exercised, rather than only code that
runs when it *applies* someone else's updates.

**Why this priority**: Closes the requirement 003 left unmet (**003's** FR-006, direction B — not this spec's FR-006, which is the no-narrowing rule) that blocks any
completeness claim. Today the reference builds every document and this library only applies the
encoded result, so its constructors never run under the oracle. That is exactly where an encoding
nondeterminism defect hid: an identically-built document produced four different encodings across
forty runs, meaning two peers emitted different identities for the same logical edit.

**Independent Test**: Have this library generate and encode documents, have the reference decode
and re-encode them, and compare bytes. Independently valuable: it is the only way to detect
non-canonical output.

**Acceptance Scenarios**:

1. **Given** a document constructed natively by this library, **When** its encoded form is decoded
   and re-encoded by the reference, **Then** the round-trip is byte-identical.
2. **Given** the same logical document constructed repeatedly, **When** each build is encoded,
   **Then** every run produces identical bytes.
3. **Given** a document built by populating a container *before* attaching it to the document,
   **When** it is encoded, **Then** the result matches the reference for the same construction.

---

### User Story 3 - Every wire format is verified (Priority: P1)

As an integrator, I need every format this library puts on the wire compared against the reference,
so that a client written against the JavaScript implementation can interoperate without silent
corruption.

**Why this priority**: Relative positions, the sync protocol, and awareness are all encoded formats
with **zero** differential coverage. Unlike an internal defect, a divergence here breaks
interoperability with real third-party clients, and would surface as data loss in production rather
than a failing test.

**Independent Test**: For each format, encode with one implementation and decode with the other in
both directions, comparing the decoded meaning and the re-encoded bytes.

**Acceptance Scenarios**:

1. **Given** a position anchored in a document, **When** it is encoded by one implementation and
   resolved by the other, **Then** both resolve it to the same location, including after concurrent
   edits that move it.
2. **Given** a sync exchange between the two implementations, **When** each message is produced by
   one and consumed by the other, **Then** both reach identical state.
3. **Given** presence information encoded by one implementation, **When** decoded by the other,
   **Then** the client entries and their ordering match.

---

### User Story 4 - The oracle exercises the whole public surface (Priority: P2)

As a maintainer, I need every public operation to appear in the generated op streams, so that
"the oracle is green" covers the API users actually call.

**Why this priority**: Six public operations are never invoked by any generator, and one of them is
where a silent data-loss defect was found (content added to a container before attachment was
discarded). A related hazard: one generator restricts itself to a single map key, noting that a
single key is order-independent — a precaution that leaves prelim key ordering unexercised. The
restriction is a harness constant narrowing coverage, whether or not a divergence was known.

**Independent Test**: A coverage report derived from the public API surface shows every operation
producing observable output invoked at least once; removing any one from the generators leaves a
detectable gap, and adding a new public operation shows up as unexercised without anyone updating a
list.

**Acceptance Scenarios**:

1. **Given** the generator set, **When** its op vocabulary is enumerated against the public API
   surface, **Then** every operation producing observable output appears — mutating and
   read/serialization alike.
2. **Given** a container populated with several keys before attachment, **When** encoded, **Then**
   the result matches the reference — with no single-key restriction in the generator.
3. **Given** a workaround comment in a generator that narrows coverage, **When** the suite runs,
   **Then** no such narrowing remains undocumented in the coverage report.

---

### User Story 5 - The gate proves itself on every surface (Priority: P2)

As a maintainer, I need the fault-injection self-test to cover every surface, so that "green" is
backed by evidence the oracle would have detected a fault on that surface.

**Why this priority**: 003 claimed this and did not deliver it. Its self-test covers one type and
only compares this library against itself, so it demonstrates encoder sensitivity rather than
cross-implementation detection. An undetectable surface makes its green meaningless.

**Independent Test**: Inject each fault kind on each surface and confirm the oracle reports every
one; a fault that survives is a proven blind spot and fails the suite.

**Acceptance Scenarios**:

1. **Given** a deliberately corrupted result on any surface, **When** the oracle runs, **Then** it
   reports divergence.
2. **Given** a surface for which no fault injection has been built, **When** the suite runs,
   **Then** it FAILS naming that surface — an unfaultable surface is an underbuilt harness, not an
   exception to be recorded and passed over.

---

### User Story 6 - Awareness starts no thread unless asked (Priority: P2)

As a library consumer, I need presence handling to run only while I am calling into the library, so
that adding it to my program does not introduce a thread I did not ask for, a lifecycle I must
remember to shut down, or concurrent access to my data that I did not schedule.

**Why this priority**: An always-on timer is the root cause of the 003 data race, and the 003
specification already classes it as an undesigned deviation that cost 23 churn commits — 003 then
guarded the symptom by hiding the state maps rather than removing the cause. Making the thread
opt-in removes it from every consumer that does not need it, while keeping it available for those
that do. It cannot be removed outright: the reference's timer also performs local renewal, an
outbound heartbeat that stops this client being dropped by reference peers, and no read-triggered
scheme can reproduce something whose trigger is elapsed time.

**Independent Test**: Create presence state without starting the managed type, let entries age
past the timeout, read the state, and confirm expired entries are gone and no thread was ever
created. Then start the managed type and confirm renewal and reaping match the reference.

**Acceptance Scenarios**:

1. **Given** presence state created without starting the managed type, **When** the program runs
   without calling into it, **Then** no background thread exists and no state changes on its own.
2. **Given** a remote entry that has aged past the timeout, **When** presence state is next read in
   the plain type, **Then** that entry is absent, matching the reference's judgement of presence.
3. **Given** many plain presence values created and discarded, **When** they go out of
   scope, **Then** no threads or timers remain, with no disposal call required.
4. **Given** the plain type, **When** the program is run under race detection, **Then** the library
   itself introduces no concurrent access — any race is caller-scheduled, exactly as for any Go
   struct with exported fields. (Stated this way deliberately: with exported maps and caller-owned
   concurrency, "unsynchronized concurrent access is race-free" would be false by construction and
   could only be satisfied by library-side locking, which FR-012 explicitly rejects for this type.)
5. **Given** the managed type is started, **When** the local client stays otherwise idle past half
   the timeout, **Then** local state is renewed and republished exactly as the reference does, so
   reference peers do not drop this client.
6. **Given** the managed type is started and then stopped, **When** the object is discarded,
   **Then** no threads, timers, or registered callbacks remain.

---

### User Story 7 - Residual defects and documentation drift are closed (Priority: P3)

As a maintainer, I need the remaining known-but-unfixed findings resolved, so that the merged state
contains no defect we have already identified.

**Why this priority**: Lowest not because the items are unimportant but because each is
individually small and independently verifiable. Leaving identified defects unfixed is corrosive:
it teaches readers that a recorded finding does not imply action.

**Independent Test**: Each item has a test that fails before the fix and passes after.

**Acceptance Scenarios**:

1. **Given** a container rendered as text, **When** it holds children of any supported kind,
   **Then** all children appear, matching the reference.
2. **Given** a stored snapshot in the older encoding, **When** it is read back, **Then** it either
   decodes correctly or reports an error — never silently produces wrong content.
3. **Given** documentation naming a reference version, **When** compared to the pinned version,
   **Then** they agree.

---

### Edge Cases

- A relative position anchored to content that is later deleted, then garbage-collected.
- Undo of an operation whose target was concurrently modified by a remote peer.
- Presence entries expiring while an update for the same client is being applied.
- A container populated before attachment and then never attached.
- Sync exchange where one side has state the other cannot yet interpret.
- Undo across a document whose subdocuments were removed during the same session.

## Requirements *(mandatory)*

### Functional Requirements

**Coverage — the gaps 003 left open**

- **FR-001**: The oracle MUST compare undo, redo, scope restriction, capture coalescing, and stack
  clearing against the reference, on op streams that interleave them with ordinary edits, including
  streams produced by more than one participant.
- **FR-001a**: Undo MUST restore content in the reference's order rather than a locally-chosen
  canonical one. The existing ordering deviation was accepted on the explicit grounds that the gate
  did not exercise undo; FR-001 removes that premise, so the deviation MUST be closed rather than
  accommodated by the comparison. This requires an ordering that does not depend on Go map
  iteration, which is randomised per run.
- **FR-001b**: The undo comparison MUST include observable stack status — whether an undo or redo
  step is available, and the events emitted when the stacks change — not only resulting document
  state. A structurally-empty transaction that pushes a phantom undo entry, or an operation that
  discards an available redo, alters no encoded bytes; state comparison alone is blind to both, and
  both occurred in practice.
- **FR-002**: The oracle MUST compare op streams generated by **this library** (not only by the
  reference), with the reference decoding and re-encoding the result, across every shared type.
- **FR-003**: The oracle MUST verify that constructing the same logical document repeatedly yields
  identical encoded output.
- **FR-004**: The oracle MUST compare relative positions, the sync protocol, and awareness against
  the reference, in both directions. For awareness this MUST include the emitted change/update
  events and their payloads, which are the observable contract a consumer reacts to, not merely the
  resulting presence map.
- **FR-005**: The generators MUST invoke every public operation that produces observable output on
  every shared type — both mutating operations and read/serialization ones (delta, string and JSON
  rendering, attribute and value getters) — and the suite MUST report any operation not exercised.
  Two known defects sit in read paths, so a mutating-only rule would exclude a proven defect class.
- **FR-005a**: The coverage report MUST be derived from the public API surface, not from a
  hand-maintained list of operations. **Inclusion predicate**: exported methods on the shared types
  that read or mutate document content — mutations, and renderings/getters returning content.
  Excluded, because they are not user-facing content operations: lifecycle (`Integrate`, `Destroy`,
  `Copy`, `Clone`), observation registration (`Observe`, `CallObserver`), and the internal
  `IAbstractContent` family (`Splice`, `Write`, `GetRef`). The predicate MUST be stated in code so
  that a method's inclusion is decidable rather than argued. A newly added public operation MUST cause the report to show
  it unexercised until it is covered; a list that must be remembered goes stale silently, which is
  the failure mode this requirement exists to prevent.
- **FR-006**: No generator may narrow its own coverage to avoid a known divergence; any deliberate
  restriction MUST be recorded as an open finding rather than absorbed as a harness constant.
- **FR-007**: Fault detection MUST be by comparison **against the reference**, not against this
  library's own output. (Surface coverage of fault injection is FR-008's rule; this requirement is
  only about what the comparison is made against — the 003 self-test compared Go to Go, proving
  encoder sensitivity rather than cross-implementation detection.)
- **FR-008**: Every surface MUST be proven by fault injection — there is no "unproven but accepted"
  state. A surface that resists fault injection indicates an underbuilt harness, not an unfaultable
  surface: anything producing output comparable against the reference can be faulted by corrupting
  the expected value. Each surface MUST report its proven status, and a surface that cannot report
  one blocks the feature rather than being recorded as an exception.

**Awareness redesign**

Presence handling has TWO time-driven behaviours in the reference, and they are not
interchangeable. **Reaping** decides which remote clients still count as present — a read-time
judgement. **Renewal** re-publishes local state on a timer so remote peers do not time this client
out — an *outbound* action that must happen whether or not anyone calls in. Reaping can be made
lazy; renewal cannot, because its trigger is elapsed time, not a read.

- **FR-009**: Presence handling MUST NOT create background threads or timers **by default**.
  Creating one MUST require an explicit call by the consumer, consistent with Constitution II
  ("MUST NOT start goroutines that outlive function calls unless explicitly requested by the
  consumer").
- **FR-010**: In the plain type, stale remote entries MUST be expired when presence state is
  accessed, matching the reference's judgement of which clients are present. "Matching the
  reference" is verified by the `awareness` surface differential, not by a Go-side-only assertion
  that expiry merely happened.
- **FR-011**: The plain type MUST NOT perform local renewal, and this MUST be documented as its
  parity limitation: a client whose program stays quiet past the timeout will be dropped by
  reference peers. Consumers needing interop timeout behaviour use the managed type (FR-011a).
- **FR-011a**: The managed type MUST reproduce the reference's timer behaviour in full —
  periodic local renewal AND stale-client reaping, emitting the same events with the same payloads.
  **Parity claims for presence attach to this mode.**
- **FR-011b**: Stopping the managed type, or discarding the presence object without ever starting
  it, MUST leave no threads, timers, or registered callbacks. Plain-type values MUST require no
  disposal call at all.
- **FR-012**: The two modes MUST be **separate types** — `Awareness` (plain) and
  `ManagedAwareness` (owns the timer) — so that a library-owned writer and directly readable state
  fields cannot coexist on the same value. The plain type keeps the existing name, since it is the
  default and the one most consumers want:
  - a **plain presence type** that never starts a thread and exposes its state as ordinary exported
    fields — any concurrency is caller-scheduled and caller-owned, as for any Go data structure;
  - a **managed presence type** that owns the timer and exposes state only through accessors that
    copy under its lock.
- **FR-012a**: The unsafe combination MUST be unrepresentable rather than merely documented. It is
  not sufficient to export fields and warn against reading them while a writer runs: an unguarded
  read of a map being written concurrently aborts the Go process outright rather than raising a
  recoverable error, so a doc comment is not a safety mechanism.
- **FR-013**: Presence state access MUST be free of data races under race detection in both types —
  unconditionally for the plain type, since the library schedules nothing, and through the
  accessors for the managed type.

**Residual findings and hygiene**

- **FR-013a**: The in-repo consumer of presence (`WSSharedDoc`, production code in the root package)
  MUST be migrated deliberately, not left to inherit whichever type it happens to compile against.
  It MUST use the managed type, since its peers are remote by definition and the plain type performs
  no renewal — but the timer MUST be started by an **explicit call** on it, never by its
  constructor, because a root-package constructor that starts a goroutine gives a consumer a
  library-owned thread without the explicit request FR-009 and Constitution II require. Its update
  observer then runs off the caller's goroutine, which MUST be documented at the call site.
  *(Scope note: this touches transport-shaped code that Constitution II would place in a subpackage.
  That relocation stays out of scope — see Out of Scope — and is not adjudicated here; only the
  presence-mode decision is.)*
- **FR-014**: Rendering a container as text MUST include children of every supported kind, matching
  the reference. (A known instance of the class FR-005 now covers generally: the current
  implementation silently drops children that are not XML types, where the reference coerces every
  child. It is listed separately because it is a defect to fix, not only a gap to cover.)
- **FR-014a**: `AddToScope` MUST accept the same scope forms as the constructor, including slice
  arguments. It currently skips anything that is not a shared type, so a slice is silently dropped —
  the normalization was added to the constructor instead of the shared primitive, the inverse of the
  reference. (Distinct from the constructor typed-slice panic, which is already fixed.)
- **FR-014b**: Rendering a binary attribute MUST match the reference's coercion (`1,2,3`), not Go's
  default slice formatting (`[1 2 3]`).
- **FR-014c**: Timeout-removal payloads MUST order clients **as the reference does**, not merely
  deterministically. The current implementation ranges a Go map, so a multi-peer timeout emits a
  nondeterministically ordered payload — but "sorted for determinism" is NOT the fix here: US3 AS3
  and C-S4.1 require the emitted payload to match the reference, and the Assumptions permit
  determinism-only ordering solely where the reference's order is unobservable. It is observable
  here. Accepting a canonical-but-different order would repeat exactly the mistake FR-001a exists
  to correct.
- **FR-014d**: Content-kind handling in the text layer MUST follow the reference's default-case
  behaviour rather than a whitelist, so a content kind absent from the whitelist stops being
  uncounted and shifting every later index. **This is a core semantic change to countability and
  index arithmetic, not a cosmetic fix**: it MUST be verified byte-exact against the reference
  (SC-010) and MUST NOT be treated as low-risk because it is grouped with residual defects.
- **FR-015** *(revised during implementation — see below)*: A decoder MUST NOT silently reinterpret
  data written in a different encoding **where the encoding permits that to be detected**. For
  snapshots it does not: the format carries no version marker.

  **Finding (T063).** The original wording ("a mismatch MUST produce an error") was written on the
  assumption that a V1/V2 snapshot mismatch is detectable. It is not, and the reference has the
  identical hazard. Verified directly against `yjs@13.6.31`: `decodeSnapshotV2(v1Bytes)` and
  `decodeSnapshot(v2Bytes)` both decode WITHOUT throwing and both return a snapshot that fails
  `equalSnapshots` against the original. Go behaved the same way before any change.

  Adding an error would therefore have been a **deviation from the reference**, rejecting input yjs
  accepts — which Constitution I forbids absent a deliberate, recorded decision. The requirement is
  satisfied instead by (i) the explicit `DecodeSnapshotV1` / `DecodeSnapshotV2` entry points, so the
  caller names the format rather than inheriting a default, (ii) a documented hazard warning on
  `DecodeSnapshot` in `snapshot.go`, and (iii) `TestSnapshotCrossFormatDecodeMatchesReference`,
  which pins both the exact round-trip contract and the indistinguishability, so the behaviour
  cannot drift unnoticed and any future decision to deviate must be taken explicitly.
- **FR-016**: Every defect found by any means during this feature MUST be converted into coverage
  that would have detected it, before the defect is considered closed. **Two bars, deliberately
  different**: (a) every defect ships a test that fails when the fix is reverted (SC-008 — the
  minimum, always required); (b) a defect on a *registered surface* MUST additionally be reachable
  by that surface's differential, so the oracle and not only a unit test would catch a recurrence.
  Where (b) is not achievable because the behaviour lies outside every surface, that MUST be
  recorded explicitly as a limit of the oracle's reach, not left implied.
- **FR-017**: Documentation MUST name the same reference version as the pinned dependency.
- **FR-018**: Rewritten code MUST meet the project's new-code test coverage floor, measured over the
  functions actually changed rather than only newly added ones.

**Gate integrity**

- **FR-019**: Every gate MUST fail when it has nothing to compare, and MUST report what it compared
  so that an empty or absent corpus cannot read as success.
- **FR-020**: The reference implementation and its transitive dependencies MUST remain pinned, so
  the comparison baseline cannot change between runs.
- **FR-021**: Verification MUST run in three tiers — a fast tier on every change proposal, a full
  tier before changes are accepted into the mainline, and a scale tier on a schedule.
- **FR-022**: **Every realized (surface, direction) cell MUST be exercised in the fast tier**,
  however small its share — cell granularity, matching SC-001a, because at surface granularity a
  direction can be silently nightly-only while the check still passes. Seed counts per cell are
  negotiable; a cell present only in a slower tier is not enforced, which is precisely how the
  previous feature's gate stayed hollow for its whole duration.
- **FR-023**: Each tier MUST report which **(surface, direction) cells** it covered and at what
  volume **per cell**, with per-surface totals as a derived rollup, so that a cell silently dropping
  out of a tier is visible rather than inferred from a green result. Cell granularity is required
  because SC-001's floor is per cell and there is no per-surface number that can evidence it.

### Key Entities

- **Op stream**: an ordered sequence of operations, including undo/redo actions, applied identically
  to both implementations; the unit a seed produces.
- **Surface**: a distinct behavioural area under comparison. **data-model.md holds the canonical
  registry list (13 surfaces)** — each shared type, `applyDelta`, `update`, undo, relative
  positions, sync, awareness, snapshots, garbage collection, subdocuments — each with realized
  directions and a proven status. Every surface must reach proven; there is no accepted unproven state.
- **Direction**: which implementation generates and which verifies — reference-generates versus
  library-generates.
- **Fault**: a deliberate corruption injected to prove a surface would report a divergence.
- **Presence state**: per-client published state plus the metadata determining whether a client is
  still considered present. Exists in two forms: a **plain** type (no library-owned thread,
  exported fields, expiry judged on access) and a **managed** type (owns the reference's timer,
  accessor-only, full renewal and reaping). Parity claims attach to the managed form.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: Zero divergence from the reference across every surface in both directions at scale —
  at least one million seeds in aggregate, with a floor of **≥10,000 seeds per realized
  (surface, direction) cell**. A cell below the floor fails SC-001 rather than being averaged away
  by a high-volume surface.
- **SC-001a**: The fast tier exercises **every realized (surface, direction) cell** — the same
  granularity SC-001 uses, so a direction cannot be silently fast-tier-exempt — and completes within
  a **hard ceiling of 10 minutes** measured in CI, with ~7 minutes the tuning target. Exceeding the
  ceiling fails; per-surface seed weights are the tuning knob, never cell membership. The full tier gates
  acceptance into the mainline at **≥1,000 seeds per realized cell within 30 minutes** — a merge
  gate with no defined volume degenerates silently; the scale tier meets SC-001 on a schedule. Each tier names the
  cells it ran and at what volume.
- **SC-002**: The fault-injection self-test detects 100% of injected faults on every surface, and
  the count of surfaces without applicable fault injection is zero — there is no exempt category.
- **SC-003**: Every public operation producing observable output on every shared type — mutating
  and read/serialization alike — is exercised by the generators, verified by a report the suite
  derives from the API surface.
- **SC-004**: Undo and redo reach zero divergence on op streams that interleave them with edits,
  including scope restriction and capture coalescing, compared on both resulting state AND
  observable stack status.
- **SC-005**: Constructing the same logical document one hundred times produces one distinct
  encoding.
- **SC-006**: Creating and discarding ten thousand presence objects without starting the explicit
  mode leaves zero residual threads and requires no disposal call; starting and stopping the managed type likewise leaves zero.
- **SC-006a**: In the managed type, an otherwise-idle client is not dropped by a reference peer
  across a full timeout period — the renewal heartbeat matches the reference.
- **SC-007**: The full suite is free of data races under race detection.
- **SC-008**: Every defect found during this feature has an accompanying test that fails when the
  fix is reverted.
- **SC-009**: No performance regression against the recorded baseline, judged by the same rule
  adopted in 003 (a regression must be both statistically significant and material).
- **SC-010**: All existing byte-exact reference fixtures continue to pass — no interoperability
  regression.

## Assumptions

- `yjs@13.6.31` / `y-protocols@1.0.7` remain the parity reference, pinned by committed lockfiles as
  established at the end of 003.
- "100% functional parity" is scoped to behaviour observable through the public API, including
  encoded output and emitted events. Internal representation may differ where the reference does not
  define it and nothing observable depends on it. Sorted ordering adopted purely to make Go's
  randomised map iteration deterministic is retained ONLY where the reference's own order is
  unobservable through the public API; where the reference's order IS observable — undo restoration
  being the case in point — the reference's order wins (FR-001a).
- Deviations from the reference remain permitted where a language difference forces one, but each
  must be deliberate, documented at the site, and accompanied by a statement of what it costs — and
  the cost statement MUST NOT rest on "no gate exercises this". The undo ordering deviation is the
  cautionary case: it was justified partly because the gate did not exercise undo, so it silently
  became wrong the moment the gate did. Deviations justified by absent coverage are re-opened
  whenever that coverage arrives (FR-001a).
- The oracle remains the correctness gate; review stays supplementary. This feature exists because
  the gate's *coverage* was narrower than its reputation, not because review should replace it.
- Making the presence thread opt-in means plain-type expiry is observed on access rather than
  announced spontaneously, and the plain type performs no local renewal. The library is pre-1.0 with
  no released version and no external consumers, so changing the default shape is free. Presence parity
  claims attach to the managed type, which reproduces the reference timer in full.
- The perf-focused work (removing reflection and deep copying) remains deferred to a later feature,
  as in 003; this feature holds the no-regression line only. The 003 baseline is stale — the tree
  changed substantially after it was captured — so a fresh baseline is recorded before any
  production change in this feature, and SC-009 is judged against that.
- Work is sequenced so each user story lands independently: US1 alone already converts the
  least-verified subsystem into a gated one.

## Out of Scope

- Performance improvement beyond the no-regression line.
- Module and package rename, and any published-identity change.
- Transport or server code.
- A full clean reimplementation — the byte-exactness established in 003 stands; this feature widens
  what is checked rather than rewriting what was checked.
