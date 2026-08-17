# Implementation Plan: Verification Oracle & Value-Representation Foundation

**Branch**: `003-oracle-and-value-rep` | **Date**: 2026-06-25 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `specs/003-oracle-and-value-rep/spec.md`

## Summary

Replace **sampling review** with an **exhaustive bidirectional differential oracle** as the
correctness gate, then use the oracle's objective divergence map to drive a **targeted rewrite of
the one proven-divergent layer**: value representation (JS `null`/`undefined`/value, attribute
order, attribute equality) and Y.Text formatting. The proven-byte-exact ~90% of the engine (V1/V2
codecs, structs, merge, sync, flat root types) is **not** touched — the oracle adjudicates the
boundary, so scope is data-driven, not by feel.

Three pillars, landed in order:

1. **Sound, bidirectional, CI-enforced oracle (the gate).** Make the existing native-op
   differentials *trustworthy* (deterministic generators, order-preserving attribute capture,
   surfaced errors, non-empty-corpus assertions, and a fault-injection **self-test** that proves
   the oracle has teeth). Add **direction B across all surfaces** (Go-generates → Yjs-validates)
   and the **convergence invariant** (permuted apply orders). Wire it into **GitHub Actions** (fast
   committed-corpus tier on every PR; ≥1e6-seed tier nightly). *This pillar produces the finite
   divergence map that scopes pillars 2–3.*
2. **One value-representation layer.** A purpose-built insertion-ordered `Attrs` type with the Yjs
   `?? null` accessor built in; the two comparators kept **one-per-semantic** (shallow `EqualAttrs`
   for format attrs, deep `equalAttrsDeep` for awareness — never collapsed); the duplicated value
   type-switches collapsed. Kills the `?? null` and attribute-order bug classes at the root, and is
   the perf-friendly shape the later performance phase needs.
3. **Y.Text formatting rewrite** on that layer, faithful to `yjs@13.6.31` source (FR-013), verified
   green on the oracle's Text + apply-delta surfaces at scale.

Definition of done is mechanical: **oracle green at scale, both directions (SC-001) +
fault-injection 100% (SC-002) + `-race` clean (SC-005) + no perf regression on the new bench
(SC-007)**. Review is demoted to API/docs.

## Technical Context

**Language/Version**: Go 1.24 (`go.mod` → `go 1.24.3`)

**Primary Dependencies**: Runtime — stdlib only (the existing `mitchellh/copystructure`; its
removal is the performance phase, but the new `Attrs` layer is designed so attribute copying no
longer needs reflection). Tests/oracle reference — Node.js + `yjs@13.6.31` + `y-protocols@1.0.7`
(test/CI-time only). **No new runtime dependency.**

**Storage**: N/A (library)

**Testing**: `go test ./... -race`; the differential **oracle** (`fuzz/`, Node + `yjs@13.6.31`),
now a real **CI-enforced** gate (GitHub Actions) over a committed seed corpus + a nightly
≥1e6-seed scale tier; byte-exact reference fixtures (`compatibility_v2_test.go`,
`v2_test_fixtures/`) retained as the wire-parity guard; a new document-ops **benchmark** (FR-018)
as the no-regression baseline.

**Target Platform**: Any Go 1.24+ platform. Interop target: JavaScript Yjs / y-protocols. CI:
GitHub Actions (Go + Node).

**Project Type**: Single-package Go library (`package y_crdt` at repo root) + `protocol/`
subpackage. No structural change.

**Performance Goals**: No regression vs. the pre-feature baseline captured on the new document-ops
benchmark (SC-007, FR-018). The `Attrs` layer is chosen to be allocation- and reflection-light so
the dedicated performance phase builds on it rather than redoing it.

**Constraints**: Byte-exact V1+V2 wire parity (non-negotiable, SC-006); the oracle is the gate and
must be sound before it grades anything (FR-001–FR-004); every Yjs claim verified against pinned
source (FR-013); `-race` clean, no leaks (FR-016).

**Scale/Scope**: 3 pillars. Pillar 1 touches `fuzz/` + `*_diff_test.go` + a new
`.github/workflows/` (test infra). Pillars 2–3 touch the value-rep surface (`type_define.go`,
`abstract_type.go`, attribute helpers) and the Y.Text formatting surface (`y_text.go` and callers).
Surfaces outside the oracle's divergence map are explicitly untouched (FR-014).

**Open items for Phase 0 research** (none block the gate; all are implementation-detail decisions):
direction-B generation strategy for snapshots/GC; committed-corpus size that keeps `go test` fast;
Node/Go versions for the CI matrix.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| Principle | How this plan satisfies it | Enforcing gate |
|---|---|---|
| I. Yjs Wire Compatibility (NON-NEGOTIABLE) | The oracle *is* the wire-compat proof, now bidirectional (all surfaces) + at scale + CI-enforced; existing fixtures retained | Oracle green at scale (SC-001) + fixtures (SC-006) |
| V. Cross-Language Compatibility Tests | Pillar 1 makes the cross-impl oracle sound, bidirectional, and a real CI gate | Oracle gate, fault-injection 100% (SC-002) |
| IV. Test-First / XI. Meaningful Tests / XIV. ≥95% New-Code Coverage | The oracle self-test gives the harness teeth; the text rewrite is gated by the oracle, reverting it goes red (FR-012 AS-3) | Self-test (SC-002); coverage on new code |
| VI. Root Cause Analysis (NON-NEGOTIABLE) / XV. Motivated Edits | The whole feature *is* the root-cause fix: one value-rep layer instead of N `?? null` patches; scope is data-driven via the divergence map | Divergence map drives scope (FR-014) |
| VII. DRY — Single Source of Truth | One `Attrs` type, one `?? null` accessor, one comparator **per semantic** (shallow + deep, each defined once — VII does not override I, so they are not collapsed); duplicated value type-switches removed (SC-004) | Grep + review (SC-004) |
| VIII. Lint on Completion | `golangci-lint run` clean before each commit | Lint gate |
| XII. Latest Deps / XIII. No Assumptions / FR-013 | Yjs behavior verified against pinned 13.6.31 source at implementation time | Per-change source-verification |
| III. Zero External Runtime Deps | No new runtime dependency; `Attrs` reduces reflection use; Node/CI are test-time only | Dependency review |
| II. Single-Package Library | No restructure (CI workflow + bench are infra, not package structure) | Design |
| IX. No Legacy Code | Replaces scattered `?? null` / duplicated type-switches / the divergent formatting with one designed layer | Review + SC-004 |

**Result: PASS — no violations.** This plan *strengthens* principle V (the oracle becomes the
CI-enforced gate) and principle VII (value-rep DRY), and is the root-cause discharge of principle
VI. The one deliberate deviation from Yjs (the `Attrs` value layer vs. a plain object) is the
spec's sanctioned "designed deviation" and serves DRY + the performance phase.

## Project Structure

### Documentation (this feature)

```text
specs/003-oracle-and-value-rep/
├── plan.md                       # This file
├── spec.md                       # What/why + measurable done-criteria
├── research.md                   # Phase 0 — per-pillar design + the divergence-map plan
├── data-model.md                 # JS-value model + Attrs + oracle-case entities
├── quickstart.md                 # How to run the oracle gate + self-test + scale tier + bench
├── contracts/
│   ├── oracle-contract.md        # Soundness, bidirectional, convergence, CI-gate obligations
│   └── value-rep-contract.md     # Attrs / equality / accessor API + invariants
└── checklists/
    └── requirements.md           # Spec quality checklist
```

### Source (touched by this feature)

```text
# Pillar 1 — oracle (test infra only)
fuzz/*.mjs, fuzz/corpus/          # generators (sound, deterministic, ordered) + direction-B (all) + self-test; committed corpus
./native_diff_test.go,            # the gate (Go drivers at REPO ROOT, not under fuzz/): committed-corpus run, no t.Skip
  native_{arr,map,xml,delta}_diff_test.go
fuzz/run-gate.sh                  # single entrypoint (fast tier + scale tier)
.github/workflows/oracle.yml      # NEW — PR fast tier + go test -race; nightly ≥1e6 tier (Go + Node)
./bench_test.go                   # NEW — document-ops benchmark (FR-018), no-regression baseline (repo root)

# Pillar 2 — value representation
type_define.go                    # JS-value model + Attrs + GetOrNull (home of the layer; comparators stay per-semantic in utils.go — NOT collapsed)
abstract_type.go                  # collapse the duplicated value type-switches; route through Attrs

# Pillar 3 — Y.Text formatting (only after the oracle scopes it)
y_text.go                         # FindPosition/FormatText/InsertText/cleanup/negation/ApplyDelta/GetDelta/ToDelta
# (callers updated to the Attrs accessor; NO change to codecs/structs/merge/sync — FR-014)
```

## Approach detail

### Pillar 1 — the oracle (CI-enforced gate)

**Today**: five native-op differentials (Text/Array/Map/XML/ApplyDelta), all *yjs→Go only*, all
`t.Skip` unless an env var is set, and at least the delta generator carries soundness bugs (biased
PRNG; attribute order lost via a Go map). That is why a degenerate corpus once read "0/1362" and a
real ApplyDelta divergence hid behind it.

**Target**:
- **Soundness (FR-001–FR-004)**: deterministic seeded generators; attributes recorded as **ordered
  entries** (serialize-time `Object.entries`, with Yjs applying the real object) and replayed in
  order; every encode/decode error surfaced; non-empty/non-degenerate corpus asserted; a
  **fault-injection self-test** that corrupts an expectation / permutes an attribute order / drops
  an op and confirms the oracle flags it (100%, SC-002).
- **Direction B — all surfaces (FR-006)**: this library generates ops → encodes → Yjs decodes +
  re-encodes → byte-exact compare, for Text/Array/Map/XML/subdoc/GC/snapshot, not just
  update/delete-set.
- **Convergence (FR-007)**: same op multiset, permuted apply orders, both impls → identical state.
- **CI gate (FR-008, FR-017)**: a committed seed corpus runs in `go test ./...` (no `t.Skip`) on
  every PR via GitHub Actions (Go + Node + `yjs@13.6.31`), with `go test -race`; a nightly schedule
  runs ≥1e6 seeds across surfaces.

**Output of this pillar**: the *finite, objective divergence map* — exactly which surfaces/ops
diverge — which **scopes pillars 2–3** (FR-014). Expectation from the retrospective: divergence is
localized to Y.Text formatting; flat roots and codecs are already 0.

### Pillar 2 — the value-representation layer

The canonical model (one place, `type_define.go`):

```go
// An attribute value is exactly one of:
//   - absent  : key not present            ~ JS undefined / o.get(k) === undefined
//   - Null     : present, explicit JS null  ~ o.get(k) === null  (the existing NullType sentinel,
//                                             now used UNIFORMLY — never conflated with Go nil)
//   - a concrete value
//
// Attrs is insertion-ordered to match JS object key-order (for..in), so formatting emits
// byte-identical markers. All attribute/formatting code uses Attrs — no call-site ?? null.

type Attrs struct { /* insertion-ordered key→value */ }

func (a *Attrs) GetOrNull(key string) any          // == Yjs  o.get(key) ?? null   (defined ONCE)
func (a *Attrs) Get(key string) (any, bool)         // distinguishes absent from present-null
func (a *Attrs) Set(key string, v any)
func (a *Attrs) Each(fn func(k string, v any))      // insertion order (matches JS for..in)
func (a *Attrs) Len() int

// TWO comparators, one per semantic, each defined once — NOT collapsed (verified vs source):
func EqualAttrs(a, b any) bool        // SHALLOW lib0 equalFlat — Y.Text format attrs   (utils.go:353)
func equalAttrsDeep(a, b any) bool    // DEEP lib0 equalityDeep — awareness state        (utils.go:498)
// NB: the existing Object.GetOr (→ Go nil) is NOT GetOrNull (→ Null sentinel); do not conflate.
```

Work: introduce `Attrs` + the one `?? null` accessor (consolidating `attrOrNull`), fold the two
duplicated value type-switch enumerations (`abstract_type.go:547`, `:694`) into one, and route the
**format-attribute** sites (`InsertAttributes`, `MinimizeAttributeChanges`, `GetDelta`,
`cleanupFormattingGap`) through the layer + the shallow `EqualAttrs`. **Awareness keeps the deep
`equalAttrsDeep`** — it is NOT routed through the shallow comparator (that is the 002
spurious-change bug). The equality functions already exist correctly in `utils.go`; US2 does not
rewrite them. **SC-004**: zero `?? null` at call sites; one comparator per semantic (shallow + deep,
each once); one ordered attr type. This is the "designed deviation" — it intentionally differs from
Yjs's plain-object representation, but as architecture, not as a patch.

### Pillar 3 — Y.Text formatting rewrite

Only the surface the oracle marks divergent. Re-derive `FindPosition` (with the search-marker
discipline), `FormatText`, `InsertText`, `cleanupFormattingGap`, negation, `ApplyDelta`,
`GetDelta`, `ToDelta` faithfully from `yjs@13.6.31` source, expressed on `Attrs`. Verified by the
oracle's Text + apply-delta surfaces at scale (SC-001); reverting it must turn the oracle red
(FR-012 AS-3 — the rewrite is load-bearing).

### Out of scope (FR-014, data-driven)

V1/V2 codecs, structs, merge, sync, flat root types — the differentials already hit 0; the recent
V1/V2 churn was untrusted-input *hardening*, not happy-path correctness. The oracle confirms these
green; they are not rewritten. The awareness reaper (our own concurrency deviation) is gated by
`-race` + stress, not the differential oracle.

## Sequencing & gates

1. **Pillar 1** → oracle sound + bidirectional (all surfaces) + self-tested + CI gate. Gate:
   SC-002 (100% fault detection), self-test green, CI green on the committed corpus.
2. **Run at scale** → produce the divergence map (research.md). Gate: map is finite and stable.
3. **Capture the perf baseline** (FR-018 bench) before any production change. Gate: baseline
   recorded.
4. **Pillar 2** → `Attrs` layer; route all sites. Gate: SC-004 + oracle no worse + bench no
   regression.
5. **Pillar 3** → text rewrite. Gate: SC-001 on Text + apply-delta.
6. **Whole feature done** → SC-001 (all surfaces, both directions, ≥1e6 seeds) + SC-002 + SC-005 +
   SC-006 + SC-007. No review gate.

## Complexity Tracking

No constitution violations to justify. The one deliberate deviation from Yjs (the `Attrs` value
layer vs. a plain object) is sanctioned by the spec's "designed deviation" clarification and serves
DRY (VII) + the performance phase; it is not added complexity but consolidation of existing
scattered complexity.
