# Quickstart: Verification Oracle & Value-Representation Foundation

How to run and validate each part. Commands are run from the repo root. The Yjs reference side
needs Node.js with `yjs@13.6.31` / `y-protocols@1.0.7` installed under `fuzz/`.

## Prerequisites

```bash
cd fuzz && npm ci   # installs pinned yjs@13.6.31 / y-protocols@1.0.7
```

## 1. Oracle self-test — prove the gate has teeth (SC-002, C-O7)

```bash
go test -run 'TestOracleSelfTest' -count=1 ./...
```

**Expected**: green — each injected fault (permute-attr, drop-op, dup-op, reorder-op) changes the
encoded bytes, so the oracle would flag it.

**Scope caveat (honest)**: this self-test is **Y.Text-only and Go-side-only** — it proves the
*encoder's sensitivity* to op/attribute mutations, not Go-vs-Yjs detection on every surface. The
real cross-impl detection is the differentials in §2/§3, which `ORACLE_REQUIRED=1` forces to run.
T011's "100% of mutants on every surface" is **not** met as written; see tasks.md.

## 2. Fast gate — the PR tier (FR-008, C-O8)

```bash
# The FUZZ_STRICT_* flags must be exported for GENERATION as well as verification (see gotcha).
export FUZZ_STRICT_SUBDOCS=1 FUZZ_STRICT_GC=1 FUZZ_STRICT_SNAPSHOT=1 FUZZ_STRICT_XML=1
node fuzz/generate.js single     1 300 50 > /tmp/single.ndjson
node fuzz/generate.js concurrent 1 300 50 > /tmp/concurrent.ndjson

ORACLE_REQUIRED=1 FUZZ_MODE=single     FUZZ_FILE=/tmp/single.ndjson     go test ./... -race
ORACLE_REQUIRED=1 FUZZ_MODE=concurrent FUZZ_FILE=/tmp/concurrent.ndjson go test -run TestFuzzGate -race .
```

**Gotcha**: `generate.js` keys the emission of `postGcState` / snapshot / subdoc fields off the
`FUZZ_STRICT_*` flags. Generate without them and verify with them and **every** case fails with
`STRICT_GC set but corpus lacks postGcState` — a stale-corpus error, not an engine bug.

**Expected**: green, `-race` clean. This is what the GitHub Actions PR job runs.

`ORACLE_REQUIRED=1` is what gives the gate teeth: without it a missing Node, a failed generator, or
an unset `FUZZ_FILE` **silently skips** and the run goes green having compared nothing. With it,
each of those is a hard failure. A bare `go test ./...` still passes on a plain Go toolchain, but
that run is *not* the gate.

## 3. Scale tier — the nightly run (SC-001)

```bash
fuzz/run-gate.sh --seeds 1000000 --surface all --dir A
```

Pinned CLI contract (T015): `run-gate.sh [--seeds N] [--surface all|<name>] [--dir A|B|both]`,
defaults `--seeds 2000 --surface all --dir A`. `--seeds` is the AGGREGATE across selected surfaces
(each gets `N/|surfaces|`, floored at 200). Surfaces: `text array map xml applyDelta update`.
The legacy positional form `[mode] [cases] [opsPerCase] [seedStart]` is still accepted.

**Direction B is not implemented** (Go generates → Yjs decodes + re-encodes; tasks.md T013).
`--dir B` and `--dir both` exit non-zero rather than pretend. Only direction A is available.

**Expected**: zero divergence across `text, array, map, xml, applyDelta` plus the `update` surface
(V1/V2 apply paths; `subdoc/gc/snapshot/xml` assertions via the `FUZZ_STRICT_*` env flags).

## 4. Convergence invariant (SC-003, C-O6)

```bash
cd fuzz && node generate.js concurrent 1 2000 100 > /tmp/conc.ndjson && cd ..
FUZZ_FILE=/tmp/conc.ndjson FUZZ_MODE=concurrent go test -run TestFuzzGate -count=1 .
```

**Expected**: same op multiset, permuted apply orders → identical final state in Go and Yjs.
`TestFuzzGate`'s `concurrent` mode replays 6 orders per case (V1 `base,u1,u2` / `base,u2,u1`, the
V2 equivalents, `full1,full2`, and a mixed V1/V2 order). There is no separate
`TestOracleConvergence`; this is the convergence gate.

## 5. Value-representation gate (SC-004, C-V1..C-V4)

```bash
go test -run 'TestAttrs' -count=1 ./...
# DRY check — must print nothing. Matches real (non-comment) coalescing / stray attrOrNull outside
# the layer; excludes comment-only lines (which legitimately quote Yjs's `o.get(k) ?? null`) and
# Object.GetOr (→nil, a separate accessor):
grep -rnE '\?\? *null|== *nil *\? *Null|attrOrNull\(' --include='*.go' . | grep -vE 'type_define.go|_test.go' | grep -vE ':[0-9]+:[[:space:]]*//'
```

**Expected**: `TestAttrs` green; the grep finds no hand-rolled null-coalescing or leftover
`attrOrNull` outside the one layer. (Equality stays **two** comparators — shallow `EqualAttrs` for
attrs, deep `equalAttrsDeep` for awareness — each defined once; this is not a violation.)

## 6. Wire-parity regression guard (SC-006)

```bash
go test -run 'TestTextInsertDelete|TestMapSet|TestArrayInsert|TestXmlFragmentInsert|TestStateVector|TestV2' -count=1 .
```

**Expected**: all 41 byte-exact V1/V2 reference fixtures pass (`compatibility_test.go` +
`compatibility_v2_test.go`). There is no `TestCompatibility` prefix — the V1 fixtures are named
per shared type.

## 7. Performance no-regression (SC-007, FR-018)

```bash
# baseline (captured before any production change, saved to specs/003-.../baseline.txt)
go test -run '^$' -bench 'BenchmarkDocOps' -benchmem -count=6 . > /tmp/after.txt
benchstat specs/003-oracle-and-value-rep/baseline.txt /tmp/after.txt
```

**Expected**: no metric both benchstat-significant (p<0.05) **and** worse by >3% — see SC-007 in
spec.md for why *both* conditions are required (significance alone is unpassable: `B/op` and
`allocs/op` have ~0% variance, so p<0.05 fires on a single extra allocation).

**Gotcha**: both files must carry the `goos:/goarch:/pkg:/cpu:` header, or benchstat treats them as
two unrelated configs and prints two standalone tables with **no `vs base` column** — which reads
like a clean run while comparing nothing. If you see no delta column, the header is missing.

Current measured delta vs. baseline: `sec/op -21.5%`, `B/op +1.03%`, `allocs/op +0.64%` (all
p=0.002, n=6). The two memory metrics are significant but far under 3%, so SC-007 passes. The
+16 allocs/op come from the `attributes.ShallowClone()` added in `InsertText` (caller-mutation
fix). Note the baseline was captured at ±10% timing variance vs ±2% now, so the sec/op improvement
is not a claimed optimisation — 003 is a no-regression line, not a perf phase.

## Definition of done (mechanical)

Green on steps 1–7 at scale = feature done. No code-review sign-off is required for correctness
(SC-008); review is reserved for API ergonomics and documentation.
