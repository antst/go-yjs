# Contract: Oracle Harness Core

The interface every surface registers against. Its purpose is Principle VII: thirteen surfaces sharing
one harness, not thirteen harnesses. Existing generators migrate onto it in Phase 0, before any new
surface is added — doing it afterwards would mean rewriting the new code immediately.

## C-H1 — Surface registration

Every surface declares name, realized directions, applicable fault kinds, and tier membership.

- **C-H1.1**: Registration with an empty fault set MUST fail the suite. There is no exempt category
  (FR-008); an unfaultable surface is an underbuilt harness.
- **C-H1.2**: Registration of any realized (surface, direction) CELL without the `fast` tier MUST
  fail the suite (SC-001a). Tier membership is keyed per cell, not per surface: at surface
  granularity a direction could be silently nightly-only while the invariant still passed.
- **C-H1.3**: The registry is the single source of truth for "every surface". Any check phrased as
  "for every surface" iterates it rather than a local list.

## C-H2 — Corpus handling

- **C-H2.1**: A corpus that is absent, empty, or yields zero cases MUST **fail**, naming the
  surface. Both the absent and set-but-empty cases — 003 closed the first and left the second open,
  and an empty corpus passed green.
- **C-H2.2**: Generator stderr MUST be surfaced, never discarded. The health line (`emitted=`,
  `dropped=`) is how a silently-degenerate corpus is caught.
- **C-H2.3**: Every run reports the **(surface, direction) cells** covered and volume **per cell**, with per-surface totals derived (FR-023). SC-001's floor is per cell, so a per-surface number cannot evidence it.

## C-H3 — Fault injection

- **C-H3.1**: Detection is by comparison **against the reference**, not against this library's own
  output (FR-007).
- **C-H3.2**: An injected fault that is NOT detected fails the suite — that is a proven blind spot.
- **C-H3.3**: Applicability is declared per (surface, kind), so an inapplicable kind is skipped
  explicitly rather than silently counted as passing.

## C-H4 — Coverage report

- **C-H4.1**: Derived from the exported method set, never a hand-maintained list (FR-005a).
- **C-H4.2**: Covers operations producing observable output — mutating **and** read/serialization.
- **C-H4.3**: A public operation with no generator invoking it MUST appear as missing and fail.

## C-H5 — Determinism

- **C-H5.1**: Same seed → same corpus, for every generator.
- **C-H5.2**: A generator MUST NOT narrow its own coverage to avoid a known divergence (FR-006).
  Any deliberate restriction is recorded as an open finding, not absorbed as a harness constant.
  This is the single-key-map lesson: the restriction hid a real defect for a whole feature.

## C-H6 — Reference pinning

- **C-H6.1**: Reference and transitive dependencies installed from committed lockfiles (`npm ci`).
- **C-H6.2**: The comparison baseline MUST NOT change between runs (FR-020). `lib0` is the encoder
  every byte comparison rests on; an unpinned range silently redefines ground truth.
