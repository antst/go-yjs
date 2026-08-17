# Contract: The Verification Oracle

The behavioral contract the oracle MUST satisfy to be a valid correctness gate. Stated as
obligations, each tied to an FR/SC and enforced by a test.

## C-O1 — Determinism (FR-002)

- `generate(seed, surface, direction) → OracleCase` MUST be a pure function of its inputs.
- Re-generating the same `(seed, surface, direction)` MUST yield a byte-identical case.
- No wall-clock, no unseeded randomness, anywhere in generation or in the recorded corpus.
- **Test**: generate a seed twice; assert identical serialization.

## C-O2 — Order fidelity (FR-003)

- Attribute sets MUST travel as ordered entries `[[k,v],…]` from the Yjs generator through to the
  Go replay; no stage may pass them through an unordered map.
- **Test**: a multi-key attribute case replays in the same order Yjs applied; the
  `permute-attr` self-test fault is detected.

## C-O3 — Error transparency & corpus health (FR-004)

- Every encode/decode error on either side MUST be surfaced as a failed `Verdict.error`, never
  swallowed.
- The corpus MUST be asserted non-empty and non-degenerate (e.g., a minimum count and a minimum
  op-variety) before any "0 divergence" is reported.
- **Test**: an injected encode error fails the case; an empty corpus fails the gate.

## C-O4 — Direction A (FR-005)

- For every covered surface, `apply(ops)` natively in this library then `EncodeStateAsUpdate` MUST
  be byte-equal to Yjs `encodeStateAsUpdate` for the same `ops`.
- Surfaces: `text, array, map, xml, subdoc, gc, snapshot, applyDelta`.

## C-O5 — Direction B, all surfaces (FR-006)

- For every covered surface, this library generates ops, encodes an update; Yjs `applyUpdate` then
  `encodeStateAsUpdate`; the result MUST be byte-equal to this library's own re-encode.
- Proves our encoder output is canonical to Yjs (a distinct failure mode from C-O4).

## C-O6 — Convergence invariant (FR-007)

- For a case with `applyOrder` permutations, the final encoded state MUST be identical across all
  orders, in both implementations.
- **Test**: permuted-order suite reports zero mismatches (SC-003).

## C-O7 — Self-test / teeth (FR-001, SC-002)

- Given any clean case, each `Fault` mutant (canonical list: `data-model.md` Fault table) MUST
  yield `match == false`. `reorder-op`/`dup-op` are injected ONLY on order-dependent /
  non-idempotent cases — a reorder on a commutative multiset legitimately still matches, so it is
  not counted as a missed detection.
- **Gate**: 100% of *applicable* mutants detected across every surface. A blind spot invalidates the
  gate.

## C-O8 — Gate wiring (FR-008, FR-017)

- A single entrypoint runs the oracle; exit code 0 ⇔ all cases match.
- Fast tier: the differential runs inside `go test ./...` against a **fresh corpus generated per
  run** from the pinned generator (no committed-ndjson bloat, never stale vs the installed yjs).
  Where Node/yjs is unavailable it **skips locally** so a bare Go toolchain stays green; under
  **`ORACLE_REQUIRED=1`** (set by the PR CI job in `.github/workflows/oracle.yml`) the same
  conditions become a **hard failure**, not a skip — CI cannot pass without the differential
  actually running. Runs alongside `go test -race`.
- Scale tier: ≥1e6 seeds across surfaces, run nightly on a schedule.
- **Test**: CI is red if any case diverges or the self-test finds a blind spot.

## Non-obligations

- The oracle does not gate API ergonomics, documentation, or our own concurrency code (the reaper);
  those are covered by review and `-race`/stress respectively (SC-008).
