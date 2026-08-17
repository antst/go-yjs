# Research: Verification Oracle & Value-Representation Foundation

Phase 0 — design decisions (Decision / Rationale / Alternatives) and the divergence-map plan.
Every Yjs-behavior claim is re-verified against pinned `yjs@13.6.31` / `y-protocols@1.0.7` source
at implementation time (FR-013).

## D1 — Correctness gate: bidirectional differential oracle, not review

- **Decision**: Make an exhaustive, bidirectional differential oracle the correctness gate; demote
  code-review to API/docs.
- **Rationale**: The retrospective showed sampling review missed 100+ bugs and never converges by
  construction (each pass samples *a* defect, not *the* defect set). A differential oracle against
  the Yjs reference is exhaustive within its surface and converges (green ⇒ proven).
- **Alternatives**: (a) Keep review as the gate — rejected, demonstrably non-converging and costly.
  (b) Full clean reimplementation — rejected; the flat-root differentials already hit 0, so the
  bulk is correct and a rewrite would discard proven work and re-incur the same hazards.

## D2 — Oracle soundness (the harness must be trustworthy before it grades anything)

- **Decision**: deterministic seeded generators; attributes captured as **ordered entries**
  (serialize-time `Object.entries`, Yjs still applies the real object); surface every encode/decode
  error; assert a non-empty/non-degenerate corpus; add a **fault-injection self-test**.
- **Rationale**: This session demonstrated *localized* harness soundness failures that manufactured
  false divergences. The generators are already seeded (`mulberry32`), but the **delta** generator
  alone carries a paren-precedence bug (`native_diff_delta.mjs:2`: `((t^(t>>>14)>>>0))` is signed
  and can go negative) that skews its distribution — the degenerate corpus that hid a real
  ApplyDelta divergence behind a false "0/1362". Separately, the delta generator's apply-delta
  `delta` ops (lines 15/17/19) lose attribute order via a raw JS object. The arr/map/xml generators
  are already correctly seeded *and* ordered, and the `format` op already captures ordered entries.
  So the soundness work is: fix the delta paren bug, order the delta path, and add the determinism +
  non-degenerate-corpus + fault-injection assertions. An oracle that is not self-proving is not a
  gate.
- **Alternatives**: Trust the existing harness — rejected (proven unreliable, twice, in one
  session). Per-surface ad-hoc checks — rejected (no teeth, no convergence guarantee).

## D3 — Direction B across all surfaces

- **Decision**: this library generates ops → encodes → Yjs decodes + re-encodes → byte-exact
  compare, for **all** surfaces (Text/Array/Map/XML/subdoc/GC/snapshot), per the clarify decision.
- **Rationale**: Direction A (yjs→Go) proves we *apply* Yjs updates correctly; direction B proves
  our *encoder output* is canonical to Yjs — a distinct failure mode. Maximum encoder-canonicality
  assurance was the explicit choice.
- **Alternatives**: update + delete-set only — considered (pragmatic), rejected by clarify in
  favour of all surfaces.
- **Feasibility note**: snapshot/GC direction-B is well-defined — generate Go-native states, encode
  to a V1/V2 update, and have Yjs `applyUpdate` + `encodeStateAsUpdate`; any valid update Yjs
  decodes, so the round-trip compare is sound.

## D4 — Convergence / commutativity invariant

- **Decision**: assert the same op multiset applied in permuted orders yields identical final state
  in both implementations.
- **Rationale**: This is the defining CRDT property; a differential that only checks one apply order
  can miss order-dependent divergence (merge, tie-breaking).
- **Alternatives**: single-order compare only — rejected (misses a real class).

## D5 — CI: GitHub Actions, enforced

- **Decision**: add `.github/workflows/oracle.yml` — fast committed-corpus tier + `go test -race`
  on every PR; ≥1e6-seed tier nightly on a schedule. Go 1.24 + Node LTS + pinned `yjs@13.6.31`.
- **Rationale**: "the oracle is the gate" must be *enforced*, not dependent on a human running it;
  no CI exists today, so this feature adds it (clarify decision).
- **Alternatives**: local/scripted only (rejected — unenforced); CI-ready-but-deferred (rejected —
  leaves the gate aspirational).

## D6 — Value representation: one `Attrs` layer

- **Decision**: a single insertion-ordered `Attrs` type with one `?? null` accessor (`GetOrNull`, consolidating `attrOrNull`), the existing `Null` sentinel used **uniformly**, and the existing
  **two** comparators kept one-per-semantic (shallow `EqualAttrs` for format attrs, deep
  `equalAttrsDeep` for awareness — each already in `utils.go`, **never collapsed**: shallow is
  required for Y.Text parity, deep for awareness); collapse only the duplicated value type-switches
  (`abstract_type.go:547`, `:694`). `Object.GetOr` (→nil) stays distinct from `GetOrNull` (→Null).
- **Rationale**: One fault line (JS null vs Go nil, attribute order, equality) produced divergences
  across `InsertAttributes`/`MinimizeAttributeChanges`/`GetDelta`/`cleanupFormattingGap` and the
  harness. Fixing the *class* once beats patching instances. Insertion order is required so
  formatting emits byte-identical markers. The layer is also the perf-debt paydown (replaces
  `reflect`/deep-copy on the hot attribute path).
- **Alternatives**: continue site-by-site `?? null` — rejected (whack-a-mole, the status quo);
  reflection-based generic equality — rejected (perf + the source of current cost).

## D7 — Text rewrite scope is data-driven

- **Decision**: rewrite only the surface the oracle marks divergent (expected: Y.Text formatting);
  leave V1/V2 codecs, structs, merge, sync, and flat roots untouched (FR-014).
- **Rationale**: The differentials already hit 0 on Map/Array/XML/Text-state; recent V1/V2 churn was
  untrusted-input hardening, not happy-path bugs. Scope by evidence, not by feel.
- **Alternatives**: rewrite the whole library — rejected (discards proven-good code).

## D8 — Performance baseline

- **Decision**: add a representative document-ops benchmark (text format/insert/delete +
  apply-delta), capture the pre-change run as the baseline, gate SC-007 against it.
- **Rationale**: The existing 2 benchmarks aren't document-ops representative; SC-007 needs a real
  baseline to have teeth. The `Attrs` layer is designed allocation-light so it should hold or
  improve the line.
- **Alternatives**: existing benches + informal (rejected — weak); defer perf entirely (rejected —
  the user wants the no-regression line enforced now).

## Open-item resolutions (implementation details, non-blocking)

- **Committed-corpus size**: a few thousand seeds per surface keeps `go test ./...` fast (seconds);
  the nightly tier scales to ≥1e6. Exact count tuned in T009/T015 (corpus commit + gate entrypoint)
  to keep the PR fast tier under a wall-clock budget (**target: < 60s**). The ≥1e6 figure
  (SC-001/FR-008) is an **aggregate** across the surface×direction matrix, with a
  per-(surface, direction) floor of ≥10k.
- **CI matrix**: Go 1.24.x + Node LTS, `yjs@13.6.31` / `y-protocols@1.0.7` pinned via lockfile.
- **Self-test mutants**: the 5 kinds defined in the `data-model.md` Fault table (the single source
  of truth) — each *applicable* mutant must be detected (SC-002).
- **Seed allocation (SC-001)**: the ≥1e6 aggregate is allocated across the surface×direction matrix
  with a ≥10k floor per **realized** (surface,direction) cell. apply-delta×B is realized within
  Text×B (not a separate encoder path), so the effective cell count is ≤16 (≤160k floor); the
  remaining budget is weighted toward the higher-divergence-risk surfaces (Text + apply-delta) and
  direction B; exact weights set in T015/T016 and recorded with the scale run.

## The divergence-map plan

After Pillar 1 is sound, run the oracle at scale across all surfaces (both directions) and record a
`surface → {total, divergent, firstSeeds}` map here. That map is the **objective scope** for
Pillars 2–3 (FR-014): a surface is rewritten only if it appears divergent. Expectation from the
retrospective: divergence localized to Y.Text formatting; everything else already 0.

### Result (2026-06-25, sound generators + teethed oracle)

| Surface / gate | Scale | Divergent |
|---|---|---|
| Text (native ops) | 1000 | 0 |
| Array | 1000 | 0 |
| Map | 1000 | 0 |
| XML (state + ToString) | 1000 | 0 |
| ApplyDelta (narrow) | 2000 | 0 |
| **ApplyDelta (wide: multi-key, false-vs-null, varied text)** | **20000** | **0** |
| Existing fuzz gate — single (V1+V2, STRICT all) | 2000 / 160k ops | 0 |
| Existing fuzz gate — **concurrent** merge (V1+V2, STRICT all) | 2000 / 352k ops | 0 |

**Seed 42 was a HARNESS ARTIFACT, not a Go bug.** The delta generator serialized each op's
`attributes` *after* `applyDelta` ran — and yjs mutates the delta op objects during application
(adding negation keys), with shared `ATTRS` references compounding it — so Go was being fed yjs's
post-mutation multi-key delta, an input yjs never received. Fix: clone attrs + serialize the delta
*before* `applyDelta`. With the corrected (sound) generator, ApplyDelta is byte-exact even when
broadened to 20k multi-key / `false`-vs-`null` / varied-text cases (the `?? null` fault line,
stressed hard).

**Net divergence map: 0 across every surface tested, including concurrent merge.** Combined with
the oracle's fault-injection self-test (`oracle_selftest_test.go`, all five fault kinds detected),
"the oracle is green" is now a *teethed, comprehensive* proof. The engine is far more converged
than the duct-taping history implied — the prior 88 commits + 002's gate already did the
convergence. **There is no formatting divergence for US3 to fix**; US2 is therefore a debt-paydown
consolidation (one `GetOrNull` accessor, one `isAnyEncodable` predicate), verified byte-exact, NOT
a behavioral rewrite.

**Scale (SC-001):** a local sweep of **520k native-op seeds** (ApplyDelta-wide 200k +
Text/Array/Map/XML 80k each) plus the V1/V2 gate (512k ops, single + concurrent) — **~1M+ ops,
0 divergence**. The ≥1e6 nightly tier is wired in CI (`.github/workflows/oracle.yml`, T016). The
fast tier runs inside `go test ./...` (T009).

**Re-verified 2026-08-13** on the current tree, against `yjs@13.6.31`:

| Gate | Volume | Result |
|---|---|---|
| Native-op differentials (text/array/map/xml/applyDelta) | 1,100,000 seeds | 0 divergence |
| V1/V2 update gate — `single` | 20,000 cases / 2,000,000 ops | 0 fail |
| V1/V2 update gate — `concurrent` (strict subdoc/gc/snapshot/xml) | 20,000 cases / 4,400,000 ops | 0 fail |
| Wire fixtures (V1+V2 byte-exact) | 41 tests | pass, 0 skips |
| Full suite `-race`, `ORACLE_REQUIRED=1` | 431 tests | pass |

**FR-007 (convergence) — MET.** `TestFuzzGate`'s `concurrent` mode replays **6 permuted apply
orders** per case (V1 `base,u1,u2` / `base,u2,u1`, the V2 equivalents, `full1,full2`, and a mixed
V1/V2 order), requiring Go to reach Yjs's canonical state. That *is* the permuted-order invariant;
no separate `TestOracleConvergence` is needed and quickstart §4 no longer names one. As of
2026-08-13 it is enforced **on every PR**, not nightly-only.

**FR-006 (direction B) — NOT met; accepted, documented gap (T013).** No `fuzz/diff_b_*.mjs` /
`dir_b_diff_test.go` exists. Partial argument: Go's encoded bytes are byte-identical to Yjs's own
output across 1.1M seeds, so Yjs *decodability* follows. The residual gap is **generator
diversity** — every corpus is Yjs-generated, so op sequences Go can reach but the JS generator
never emits are unexplored. `run-gate.sh --dir B|both` exits non-zero rather than imply coverage
that does not exist.

**Oracle blind spot (recorded).** Undo/redo is not exercised by any cross-impl gate. The redo
candidate ordering in `undo_manager.go:285` sorts by `(client, clock)` — a deliberate, documented
canonical order chosen because Go map iteration is non-deterministic, but *not* necessarily Yjs's
delete-set insertion order. A multi-client redo may therefore pick a different LWW left-neighbour
than Yjs, and the oracle cannot detect it. Accepted (undo is local and its result syncs as an
ordinary update), but it is a genuine limit on "the oracle proves correctness".

---

## Oracle coverage-gap analysis (2026-08-13)

Motivated by a post-review finding that a green oracle coexisted with real defects. **Measured, not
estimated** — every number below is reproducible from the repo.

The goal is 100% functional parity with `yjs@13.6.31`. The oracle as built cannot establish that,
and no seed count changes it: coverage is a property of the **generators**, not the corpus size.

### The generators' entire vocabulary is five op kinds

`insert`, `delete`, `set`, `format`, `setAttr` — across all seven generators. That is the whole
op alphabet behind "1.1M seeds, 0 divergence".

### Wire-encoded surfaces with ZERO differential coverage

Each encodes to bytes, so each is parity-critical by definition; a divergence is a silent interop
break with real Yjs clients.

| Surface | encode/decode fns | generator files referencing it |
|---|---|---|
| `RelativePosition` | 4 | **0** |
| Sync protocol (`protocol/`) | 8 | **0** |
| `Awareness` | 1 | **0** |
| Snapshot | 6 | 1 |

### Public API never exercised by any generator

`InsertEmbed`, `YMap.Clear`, `YArray.Unshift`, `YArray.Splice`, `YXmlFragment.InsertAfter`,
`RemoveAttribute` — 0 occurrences. `InsertAfter` is precisely where a silent data-loss defect was
found (content spliced into a local copy and discarded).

### Subsystems with zero coverage

- **UndoManager**: `grep -ric "undo\|redo"` over all seven generators returns **0**. Four confirmed
  defects were found here by review (phantom stack item; redo-stack loss on empty transactions;
  `ContentType.Copy` resurrecting tombstones; typed-slice scope panic). None was reachable by the
  oracle at any seed count.
- **Go-side construction**: direction A has JS build the document and Go only `ApplyUpdate`, so
  Go's constructors and the prelim-content flush never execute. That is where the encoding
  nondeterminism hid (4 distinct byte streams across 40 identical builds). This is the concrete
  cost of FR-006/T013 being unimplemented.

### Assertions that were weaker than they read

- `FUZZ_STRICT_GC` was a **tautology**: yjs `Doc.gc` is a boolean field, not a method, so
  `if (typeof doc.gc === 'function') doc.gc()` never ran; `postGcState` was a second copy of
  `state` and the Go check re-ran the base v2 comparison verbatim. Fixed — `postGcState` is now an
  independent `gc:false` replay, making it a genuine invariance check. **Note**: GC *structural*
  parity was never unverified; the byte-identity block covers it. Only the toJSON check was dead.
- `fuzz/native_diff_arr.mjs:10` restricts embedded maps to a SINGLE key, with a comment stating it
  is "order-independent, so it stays byte-exact regardless of prelim key ordering" — the harness
  was shaped around the nondeterminism rather than exposing it. A second key turns it red.

### Expansion backlog, in yield order

1. **UndoManager differential** — highest yield: four defects, zero coverage.
2. **Direction B (T013)** — unlocks Go-side construction; also closes the FR-006 gap blocking DoD.
3. **`RelativePosition` + sync-protocol differentials** — entire wire formats, currently unverified.
4. **Op-vocabulary expansion** — the six missing API calls; multi-key prelim maps (drop the
   single-key workaround).
5. **Awareness differential** vs `y-protocols@1.0.7`.

Items 1–3 are prerequisites for any 100%-parity claim.
