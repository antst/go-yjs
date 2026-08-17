# Feature Specification: Verification Oracle & Value-Representation Foundation

**Feature Branch**: `003-oracle-and-value-rep`

**Created**: 2026-06-25

**Status**: Draft

**Input**: User description: "Establish an exhaustive, bidirectional differential oracle as the
correctness gate (replacing sampling code-review, which is slow, costly, and incomplete — it
missed 100+ bugs), then use the oracle's divergence map to unify value representation (JS null vs
Go nil, attribute key-order, attribute equality) into one designed layer and rewrite the Y.Text
formatting layer on it. Out of scope: full rewrite, module rename, performance improvement beyond
no-regression."

## Overview

This feature changes **how correctness is established**, then uses that mechanism to **pay down
the one structural fault line** behind the recurring Yjs-parity bugs.

The retrospective on the post-`e8e6d95` history found 88 `fix` commits over ~6 active days,
overwhelmingly patching faithfulness bugs **inherited** from the original Go port
(`y_text.go`, `abstract_type.go`, `item.go`, `doc.go` — all present at `e8e6d95`), not bugs in
the V2 work we added (V2 is byte-exact and converged fast). The patching never converges because
the **gate is sampling review**, which by construction finds *a* defect per pass rather than
*the* defect set. Meanwhile the native-op differentials built late reached **zero divergence on
Map, Array, XML, and Text state** — proving most of the engine is already byte-exact and the
damage is localized to **Y.Text formatting + value representation**.

This feature therefore:

1. **Establishes an exhaustive, bidirectional differential oracle as THE correctness gate**,
   replacing sampling review. An oracle converges (green ⇒ proven); review cannot.
2. **Unifies value representation** (JS `null`/`undefined`/value, attribute ordering, attribute
   equality) into one designed layer, so the `?? null` bug class is killed at the root instead of
   patched at each call site.
3. **Rewrites the Y.Text formatting layer** on that foundation — the one "cracked room" the
   oracle proves divergent — leaving the proven-good ~90% of the engine untouched.

**Definition of done is mechanical, not editorial**: the oracle is green at scale and `-race` is
clean. Not "green until the next review."

## Clarifications

### Session 2026-06-25

- Q: Does this feature rewrite the whole library? → A: No. It replaces only the proven-divergent
  layer (value representation + Y.Text formatting). V1/V2 codecs, structs, merge, sync, and the
  flat root types stay — the oracle adjudicates the scope boundary objectively (a surface is
  rewritten only if the oracle shows it diverges).
- Q: Is "out-engineering Yjs" allowed? → A: Yes, but only as a *designed* deviation, not a patch.
  The value-representation layer is exactly such a deviation (a purpose-built attribute type vs.
  Yjs's plain JS object); the awareness reaper goroutine is the cautionary example of an
  *undesigned* deviation that cost 23 churn commits.
- Q: What happens to code-review as a gate? → A: Demoted to supplementary. The oracle is the
  correctness gate; the Go race detector + stress gate our own concurrency additions; review is
  reserved for what no oracle sees (API ergonomics, documentation).
- Q: Parity reference? → A: `yjs@13.6.31` / `y-protocols@1.0.7`, consistent with feature 002.
  Every Yjs-behavior claim is verified against the actual pinned source (FR-013).
- Q: How is the oracle gate enforced, given no CI exists today? → A: This feature ADDS GitHub
  Actions CI — a fast committed-corpus tier on every PR and a scaled (≥1e6-seed) tier nightly —
  so the gate is enforced, not dependent on a human remembering to run it (FR-008, FR-017).
- Q: How broad is direction B (Go→Yjs re-encode)? → A: All surfaces (Text/Array/Map/XML/
  subdoc/GC/snapshot), not just update + delete-set — maximum encoder-canonicality assurance
  (FR-006).
- Q: What is the performance no-regression line measured against? → A: A representative
  document-ops benchmark (text format/insert/delete + apply-delta) added by this feature; its
  pre-change run is the baseline (FR-018, SC-007).

## User Scenarios & Testing *(mandatory)*

### User Story 1 - The oracle is trustworthy and exhaustive (Priority: P1)

> **Status (2026-08-13): PARTIALLY MET.** Direction A is implemented, teethed and CI-enforced.
> Direction B (AS-2 / FR-006) is **unimplemented**, and the fault-injection self-test (AS-4 /
> SC-002) covers Y.Text only, not every surface. "Exhaustive" therefore does not yet hold as
> written. See tasks.md T011/T013 — this needs either implementation or a signed-off descope.

As a maintainer, I need a differential oracle I can *believe* — one that compares this library
against Yjs across every native operation in direction A (Yjs generates -> Go replays -> byte-compare;
direction B is unimplemented, see T013), detects any divergence it is shown,
and runs as a gate — so that "the oracle is green" is a sound proof of wire/behavioral parity and
correctness no longer depends on sampling review.

**Why this priority**: This is the prerequisite for everything else. Until the oracle is sound
and exhaustive, neither "the engine is correct" nor "the rewrite is correct" can be *proven* — and
the harness itself has demonstrated bugs (PRNG bias, attribute-order loss) that manufactured false
divergences and burned cycles. It must land first and be self-proving.

**Independent Test**: Run the oracle's fault-injection self-test — deliberately corrupt an
expected state / permute an attribute order / drop an op — and confirm the oracle **detects every
injected fault**. Then run it clean on the current engine and observe the *real*, finite
divergence map (expected: localized to Y.Text formatting).

**Acceptance Scenarios**:

1. **Given** a generated op stream, **When** it is applied natively in this library and in Yjs,
   **Then** the oracle compares encoded state byte-exact (direction A: yjs→Go).
2. **Given** an op stream applied natively in this library, **When** its encoded update is decoded
   and re-encoded by Yjs, **Then** the round-trip is byte-exact (direction B: Go→yjs).
3. **Given** the same op multiset in permuted apply orders, **When** both implementations reach
   quiescence, **Then** final state converges (the CRDT commutativity invariant).
4. **Given** a deliberately injected fault (corrupted expectation, permuted attribute order,
   dropped/duplicated op), **When** the oracle runs, **Then** it reports the divergence — 100% of
   injected faults are detected.
5. **Given** the committed seed corpus, **When** `go test ./...` runs, **Then** the oracle runs as
   a real gate (not skipped) and is green; a scaled nightly tier runs at least 1e6 seeds.

---

### User Story 2 - Value representation is unified, not patched (Priority: P1)

As a developer, I need JS `null` vs `undefined` vs value, attribute key-order, and attribute
equality modeled in **one** designed layer with the Yjs semantics built in, so that no call site
hand-rolls `?? null`, no two type-switches duplicate the value enumeration, and the whole bug
*class* is closed rather than its instances.

**Why this priority**: This single fault line produced divergences in `InsertAttributes`,
`MinimizeAttributeChanges`, `GetDelta`, `cleanupFormattingGap` — and in the test harness itself.
It is also the upcoming performance bottleneck (`reflect` + deep-copy + the `Null` sentinel).
Fixing it structurally serves correctness and performance at once.

**Independent Test**: Grep the codebase for hand-rolled null coalescing and duplicated value
type-switches after the change — there are none; all attribute access routes through the layer.
The oracle's attribute-heavy cases (multi-key, null-valued, ordered) are green.

**Acceptance Scenarios**:

1. **Given** an attribute read where the key is absent, present-with-null, and present-with-value,
   **When** code uses the layer's accessor, **Then** it yields the Yjs `o.get(k) ?? null` result in
   all three cases without any call-site `?? null`.
2. **Given** two format-attribute maps differing only in key insertion order, **When** compared by
   the shallow `EqualAttrs` comparator (lib0 `equalFlat`), **Then** the result matches Yjs
   `equalAttrs`; awareness state instead compares via the deep `equalAttrsDeep` comparator (lib0
   `equalityDeep`) — the two comparators are kept distinct (FR-010).
3. **Given** a multi-key attribute set, **When** it is iterated for formatting, **Then** iteration
   order matches JS object key-order (insertion order), so emitted format markers match Yjs
   byte-for-byte.

---

### User Story 3 - Y.Text formatting matches Yjs exhaustively (Priority: P1)

As a developer using rich-text, I need every formatting operation — insert, format, delete,
apply-delta, get-delta, to-delta, negation, gap cleanup — to match Yjs exactly, proven by the
oracle at scale, so the formatting "cracked room" is closed once and stays closed.

**Why this priority**: Y.Text formatting was the suspected-divergent surface and the direct cause
of the long review tail. Verifying it exhaustively on the unified value layer (US2), behind the
trustworthy oracle (US1), converts an open-ended patch stream into a bounded, proven surface. Per
FR-014 it is rewritten only if the oracle's divergence map shows divergence; the map (research.md)
found none, so the work is verification/coverage of the existing (non-divergent) behavior, not a
rewrite.

**Independent Test**: The oracle's Text + apply-delta surfaces run at scale (direction A; B unimplemented) with
zero divergence; the oracle's fault-injection self-test (US1) shows these surfaces have teeth (an
injected formatting fault is detected).

**Acceptance Scenarios**:

1. **Given** a base text and an arbitrary rich-text delta (including null-valued and multi-key
   attributes), **When** apply-delta runs, **Then** encoded state is byte-exact with Yjs.
2. **Given** overlapping/adjacent formats and deletions, **When** gap cleanup and negation run,
   **Then** the item chain matches Yjs (same structs, same merge outcome).
3. **Given** a deliberately injected formatting fault (e.g. a permuted attribute order or a dropped
   format op), **When** the oracle runs, **Then** it reports divergence (the Text + apply-delta
   surfaces have teeth — the green is load-bearing, not cosmetic).

---

### Edge Cases

- Attribute key absent vs. present-with-`null` vs. present-with-value — all three distinct.
- Multi-key attribute maps where iteration order changes emitted format markers.
- Concurrent / permuted apply orders (CRDT commutativity).
- Formatting interacting with GC, snapshots, and nested/embedded types.
- Go→Yjs direction: an encoded update that decodes but is non-canonical in Yjs.
- The oracle's own soundness: non-deterministic generators, lost ordering, swallowed encode
  errors, or an empty corpus must be impossible (asserted) — these are the failures that produced
  false "0 divergence" before.

## Requirements *(mandatory)*

### Functional Requirements

**Oracle — soundness**

- **FR-001**: The oracle MUST be self-testing: a fault-injection suite (corrupted expectation,
  permuted attribute order, dropped/duplicated/reordered op) MUST be detected at 100%.
- **FR-002**: The oracle's generators MUST be deterministic and reproducible from a seed (no
  unvetted PRNG; no wall-clock/random nondeterminism in the recorded corpus).
- **FR-003**: The oracle MUST preserve attribute insertion order end-to-end (record ordered
  entries, replay in order) so attribute-order never manufactures a false divergence.
- **FR-004**: The oracle MUST surface every encode/decode error and MUST assert a non-empty,
  non-degenerate corpus (guard against a generator that silently emits nothing useful).

**Oracle — coverage & gate**

- **FR-005**: The oracle MUST compare direction A (Yjs generates ops → applied natively in this
  library → byte-exact encoded-state compare) across Text, Array, Map, XML, subdocuments, GC,
  snapshots, and apply-delta.
- **FR-006**: The oracle MUST compare direction B (this library generates ops → encodes → Yjs
  decodes and re-encodes → byte-exact compare) across ALL surfaces (Text, apply-delta, Array, Map,
  XML, subdocuments, GC, snapshots) — not just update + delete-set.

  > **UNMET (2026-08-13).** No direction-B harness exists (T013). Partial mitigation: Go's encoded
  > bytes are byte-identical to Yjs's own output across 1.1M seeds in direction A, so Yjs
  > *decodability* follows by construction. The residual, uncovered risk is **generator
  > diversity** — every corpus is Yjs-generated, so op sequences reachable by this library but
  > never emitted by the JS generator are unexplored. `run-gate.sh --dir B|both` exits non-zero
  > rather than imply coverage that does not exist. Closing or formally descoping this is a
  > maintainer decision; until then FR-006 blocks the Definition of Done.
- **FR-007**: The oracle MUST verify the convergence/commutativity invariant: the same op multiset
  applied in permuted orders yields identical final state in both implementations.
- **FR-008**: The oracle MUST run as a real, CI-enforced gate: a fast committed-seed-corpus tier
  in `go test ./...` on every PR (GitHub Actions), plus a scaled tier of at least 1e6 seeds in
  aggregate across the surface×direction matrix (per-(surface,direction) floor ≥10k; aggregate
  allocated per the weighting in research.md "Seed allocation"; see SC-001) run nightly on a
  schedule.

**Value representation**

- **FR-009**: A single value-representation layer MUST model the JS value space (absent / explicit
  `null` / value) and expose one accessor with `o.get(k) ?? null` semantics; no call site may
  hand-roll null coalescing.
- **FR-010**: There MUST be exactly one comparator **per distinct Yjs semantic**, each defined
  once — **shallow** `equalFlat` (`EqualAttrs`, for Y.Text format attributes) and **deep**
  `equalityDeep` (`equalAttrsDeep`, for awareness state), sharing the `attrStrictEqual` `===`
  primitive — plus one ordered attribute type; duplicated value type-switch enumerations MUST be
  removed. Collapsing the two comparators into one is **forbidden**: format attrs MUST stay shallow
  (deep changes the Y.Text item chain — see the shallow-rationale comment above `EqualAttrs`,
  `utils.go:353`) and awareness MUST stay deep (shallow re-introduces the 002 spurious-change bug —
  `awareness.go:155-166`). "Unified" here means *one definition per semantic*, not one global
  function.
- **FR-011**: The attribute type MUST preserve insertion order to match JS object key-order, so
  formatting emits byte-identical markers.

**Text rewrite & scope**

- **FR-012**: Y.Text formatting (`FindPosition`, `FormatText`, `InsertText`,
  `cleanupFormattingGap`, negation, apply-delta, get-delta, to-delta) MUST pass the oracle's Text +
  apply-delta surfaces at scale (direction A; direction B remains unimplemented per T013); it MUST be rewritten on the value layer ONLY if
  the divergence map (FR-014) shows divergence. The map found zero formatting divergence, so this
  reduces to verification/coverage of the existing behavior (which already routes through the
  unified `Attrs` accessor), not a rewrite.
- **FR-013**: Every Yjs-behavior claim MUST be verified against the actual pinned `yjs@13.6.31` /
  `y-protocols@1.0.7` source at implementation time (carries forward 002's FR-035).
- **FR-014**: Surfaces the oracle proves byte-exact (V1/V2 codecs, structs, merge, sync, flat root
  types) MUST NOT be rewritten; the oracle's divergence map adjudicates scope.

**Invariants**

- **FR-015**: Byte-exact V1+V2 wire parity MUST be preserved (existing reference fixtures still
  pass).
- **FR-016**: No new runtime dependency; `go test ./... -race` MUST be clean with no
  goroutine/listener leaks.

**CI & tooling**

- **FR-017**: This feature MUST add GitHub Actions CI that runs the fast oracle tier +
  `go test ./... -race` on every PR and the ≥1e6-seed tier nightly on a schedule, with Node.js
  available for the Yjs reference side.
- **FR-018**: This feature MUST add a representative document-ops benchmark (text
  format/insert/delete + apply-delta) and capture the pre-change run as the no-regression baseline
  for SC-007.

### Key Entities *(include if feature involves data)*

- **Oracle case**: a seed + a generated op stream (or base+delta) + the Yjs reference artifact
  (encoded state and/or re-encoded update) + the surface tag. Deterministic from the seed.
- **Value (JS-value)**: one of *absent* (Go `nil` / JS `undefined`), *Null* (explicit JS `null`),
  or a concrete value — the canonical model the layer enforces.
- **Attrs**: an insertion-ordered attribute map with a null-coalescing accessor, ordered
  iteration, and two distinct comparators — shallow `EqualAttrs` (lib0 `equalFlat`, for Y.Text
  format attributes) and deep `equalAttrsDeep` (lib0 `equalityDeep`, for awareness state), kept
  separate per FR-010; the one type all formatting/attribute code uses.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: The oracle, run at scale (≥1e6 seeds in **aggregate** across the surface×direction
  matrix, with a per-surface floor of ≥10k seeds; direction A only, as direction B is unimplemented per T013), reports **zero
  divergence** from `yjs@13.6.31`. Zero divergence is the done-state; the scale run also produces
  the divergence map that scopes US2/US3 (FR-014, T017) — which found zero Y.Text formatting
  divergence, so US3 is verification of the existing (non-divergent) behavior, not a rewrite.
- **SC-002**: The oracle's fault-injection self-test detects **100%** of injected faults.
- **SC-003**: The convergence invariant holds across the permuted-order suite (zero mismatches).
- **SC-004**: **Zero** hand-rolled `?? null` / null-coalescing at call sites (all via the one
  `?? null` accessor; stray `attrOrNull(`/raw `.GetOr(` for that semantic outside the layer also
  counts as a violation); **one comparator per semantic** (shallow `EqualAttrs` + deep
  `equalAttrsDeep`, each defined once, sharing `attrStrictEqual`) — not one global function; and
  **one** ordered attribute type (verified by the canonical grep in quickstart.md §5 + review).
- **SC-005**: `go test ./... -race` is clean; no goroutine or listener leaks.
- **SC-006**: All existing byte-exact V1+V2 reference fixtures still pass (no wire regression).
- **SC-007**: No performance regression vs. the pre-feature baseline on the new document-ops
  benchmark (FR-018). **Pass rule**: for each `BenchmarkDocOps` metric (ns/op, B/op, allocs/op), a
  regression fails the gate only if it is **both** benchstat-significant (p<0.05) **and** worse by
  **>3%**. Both conditions are required; either alone is insufficient.

  *Rationale (amended after the rule proved unpassable as originally written).* The original rule
  failed a metric on significance **or** >3%. But `B/op` and `allocs/op` are deterministic counters
  with ~0% run-to-run variance, so benchstat reports p<0.05 for a **single extra allocation** — the
  gate could never be passed by any change that allocates at all, regardless of how negligible.
  Conversely, materiality alone is also insufficient: a >3% swing inside timing noise is not
  evidence of a regression. Requiring both keeps the gate meaningful (a real, measurable slowdown
  fails) without making it vacuous (a 0.6% allocation delta does not).
- **SC-008**: Correctness sign-off no longer requires human/LLM review — the oracle gate (US1) is
  the proof; review is supplementary (API/docs). This is a **policy outcome, satisfied-by-construction
  once SC-001/SC-002 hold** — not a separately mechanically-measured criterion.

## Assumptions

- `yjs@13.6.31` / `y-protocols@1.0.7` remain the parity reference; Node.js is available at
  test/CI time for the oracle's reference side.
- The proven-correct results of feature 002 (the byte-exact codecs, the flat-root differentials at
  0) are retained; this feature replaces only the value-rep + Y.Text-formatting layer.
- The module/package rename remains out of scope (deferred, as in 002).
- Performance *improvement* (removing reflection/deep-copy) is the dedicated later phase; this
  feature holds a no-regression line, and the value-layer design is chosen to be perf-friendly so
  it does not have to be re-done then.

## Out of Scope

- A full clean reimplementation of the library (the oracle proves the bulk is already correct).
- The module/package rename and published-identity change.
- Performance improvement beyond the no-regression line.
- Transport/server code (constitution II/III).
