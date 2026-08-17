# Quickstart: Full Parity Coverage & Awareness Reaper Redesign

How to run and validate each part. Commands run from the repo root.

Written against the design in [plan.md](./plan.md); commands for surfaces that do not exist yet are
marked **(after Phase N)** so this file is usable during implementation rather than only at the end.

## Prerequisites

```bash
cd fuzz && npm ci && cd ..    # npm ci, NOT npm install — the lockfile is what pins lib0,
                              # the encoder every byte comparison is made against
```

Verify the pin took:

```bash
node -e "console.log('yjs',require('./fuzz/node_modules/yjs/package.json').version,
                     'lib0',require('./fuzz/node_modules/lib0/package.json').version)"
```

## 1. Fast tier — what every change proposal runs

```bash
fuzz/run-gate.sh --tier fast
```

**Expected**: every registered surface reported with a non-zero case count, zero divergence,
completes in roughly seven minutes.

**The two failures that mean "the gate ran but proved nothing"** — both are hard failures by
design, so seeing them is the system working:

- `empty corpus (<path>) — the gate compared NOTHING` — an absent or zero-case corpus (C-H2.1).
- `surface <name> registered with no applicable faults` — an unfaultable surface is an underbuilt
  harness, not an exemption (C-H1.1).

**Gotcha carried over from 003**: the `FUZZ_STRICT_*` flags must be exported for **generation** as
well as verification. `generate.js` keys field emission off them, so generating without and
verifying with fails every case with `corpus lacks postGcState` — a stale-corpus error, not an
engine bug.

## 2. Full tier — what gates acceptance into the mainline

```bash
fuzz/run-gate.sh --tier full
```

**Expected**: same surfaces, larger per-surface share, zero divergence.

## 3. Scale tier — the scheduled run

```bash
fuzz/run-gate.sh --tier scale --seeds 1000000
```

**Expected**: ≥1e6 seeds aggregate across the surface × direction matrix with a per-cell floor,
zero divergence (SC-001). The run names every surface and its volume (FR-023).

## 4. Undo surface **(after Phase 1)**

```bash
go test -run TestUndoDiff -count=1 .
```

**Expected**: zero divergence including multi-participant streams, compared with **no ordering
carve-out**, and covering observable stack status — not only resulting document state.

Why stack status matters here: a phantom undo entry and a lost redo each change **no** encoded
bytes. Both occurred in 003. A state-only comparison cannot reach either, so a green run that
compared bytes alone would be meaningless for this surface.

## 5. Direction B **(after Phase 2)**

```bash
fuzz/run-gate.sh --tier fast --dir B
```

**Expected**: this library constructs and encodes; the reference decodes and re-encodes;
byte-identical. Plus repeated-build determinism:

```bash
go test -run TestBuildDeterminism -count=1 .
```

**Expected**: one hundred identical builds produce **one** distinct encoding (SC-005). Before this
feature the equivalent produced four distinct encodings across forty builds.

## 6. Wire formats **(after Phase 3)**

```bash
go test -run 'TestRelPosDiff|TestSyncDiff|TestAwarenessDiff' -count=1 .
```

**Expected**: zero divergence in both directions for relative positions, the sync protocol, and
awareness — three formats with no differential coverage at all before this feature. Awareness
compares emitted change/update payloads, not merely the resulting presence map.

## 7. Coverage report **(after Phase 4)**

```bash
go test -run TestOperationCoverage -count=1 .
```

**Expected**: no operation listed as missing. The report is **derived from the exported method set**
(FR-005a) — adding a public method makes it appear as unexercised without anyone editing a list.
Covers read/serialization operations too, since two known defects sit in read paths.

## 8. Fault injection **(after Phase 4)**

```bash
go test -run TestFaultInjection -count=1 -v .
```

**Expected**: 100% of applicable faults detected on **every** surface, zero surfaces without
applicable faults. Detection is against the reference — the 003 self-test compared this library
against itself, which proves encoder sensitivity rather than cross-implementation detection.

## 9. Presence **(after Phase 5)**

```bash
go test -run 'TestPresence' -race -count=1 .
```

**Expected**: the plain type starts no goroutine and needs no disposal; ten thousand created and
discarded leave zero residual threads. The managed type reproduces the reference interval and, per
SC-006a, keeps an otherwise-idle client from being dropped by a reference peer across a full timeout.

## 10. Regression guards

```bash
# wire fixtures — no interoperability regression (SC-010)
go test -run 'TestTextInsertDelete|TestMapSet|TestArrayInsert|TestXmlFragmentInsert|TestStateVector|TestV2' -count=1 .

# race detection across the suite (SC-007)
ORACLE_REQUIRED=1 go test ./... -race -count=1

# lint (Constitution VIII)
golangci-lint run
```

## 11. Coverage over TOUCHED code, not just added code

Feature 003 measured only newly ADDED functions and so reported a floor it had not met. The measure
that matters is every function this feature touched, across the repo root and `protocol/`:

```bash
ORACLE_REQUIRED=1 FUZZ_MODE=single FUZZ_FILE=<corpus>   go test ./... -count=1 -coverprofile=/tmp/cover.out -coverpkg=./...
```

Then attribute blocks to changed lines (`git diff -U0 main..HEAD`) and take the max count per block
across package runs, since `-coverpkg` emits the same block once per test binary.

**Expected**: ≥95% (FR-018, Constitution XIV). Measured for this feature: **95.08%**, up from 86.54%
before the Phase-10 pass — and chasing it is what surfaced the dead `ToDelta`-with-snapshot path.

## 12. What a green run does NOT prove

Read `research.md` § T079 before treating a green gate as parity. In short: V2 is verified by
fixtures plus the text surface, not by all thirteen; `decodeUpdate*` and `obfuscateUpdate*` have no
Go equivalent; and `AddToScope`'s slice handling is outside every surface's reach. Those are stated
limits, not omissions.

## 11. Performance

```bash
# Baseline is captured FRESH in Phase 0 — the 003 baseline is stale.
go test -run '^$' -bench 'BenchmarkDocOps' -benchmem -count=6 . > /tmp/after.txt
benchstat specs/004-full-parity-coverage/baseline.txt /tmp/after.txt
```

**Expected**: no metric both benchstat-significant (p<0.05) **and** worse by >3%. Both conditions
are required — allocation counters have ~0% variance, so significance alone fires on a single extra
allocation and would make the gate unpassable.

**Gotcha**: both files need the `goos:/goarch:/pkg:/cpu:` header, or benchstat treats them as
unrelated configs and prints two tables with **no `vs base` column** — which reads like a clean run
while comparing nothing.

## Definition of done

Green on steps 1–11 at scale, with SC-001…SC-010 holding. Correctness sign-off is the oracle, not
review — but note the scope of that claim: it holds for surfaces in the registry, which is why
FR-008 and FR-022 forbid a surface being absent from fault injection or from the fast tier.
