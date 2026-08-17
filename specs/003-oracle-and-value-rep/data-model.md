# Data Model: Verification Oracle & Value-Representation Foundation

The entities this feature introduces or formalizes. "Fields" are conceptual (the contract),
not a prescribed Go layout.

## OracleCase

A single deterministic differential test case.

| Field | Meaning |
|---|---|
| `seed` | unsigned integer; the case is fully reproducible from it (FR-002) |
| `surface` | enum: `text` \| `array` \| `map` \| `xml` \| `subdoc` \| `gc` \| `snapshot` \| `applyDelta` |
| `direction` | `A` (yjs→Go) \| `B` (Go→yjs) |
| `ops` | the generated op stream (or `base` + `delta` for `applyDelta`) |
| `attrs` | attribute values captured as **ordered entries** `[[k,v],…]`, never an unordered map (FR-003) |
| `applyOrder` | permutation index for the convergence invariant (FR-007); `0` = canonical order |
| `referenceState` | Yjs `encodeStateAsUpdate` hex (direction A expectation) |
| `referenceUpdate` | Yjs decode+re-encode hex (direction B expectation) |

**Validation rules**: same `seed` → identical case (deterministic); `attrs` order preserved
end-to-end; an empty/degenerate `ops` is rejected by the corpus assertion (FR-004).

## Verdict

The result of comparing one `OracleCase`.

| Field | Meaning |
|---|---|
| `match` | bool — Go artifact byte-equal to the Yjs reference |
| `firstDiffByte` | index of the first differing byte (diagnostics) |
| `error` | any encode/decode error, always surfaced (FR-004) |

## DivergenceMap

The objective scope artifact produced by running the sound oracle at scale (research.md).

| Field | Meaning |
|---|---|
| `surface` | the surface tag |
| `total` | cases run |
| `divergent` | cases where `match == false` |
| `firstSeeds` | first N divergent seeds (for minimization) |

A surface enters Pillar 2/3 scope **iff** `divergent > 0` (FR-014).

## Fault (self-test only)

A deliberate mutation applied to a clean `OracleCase` to prove the oracle has teeth (FR-001).

| Kind | Mutation |
|---|---|
| `corrupt-expectation` | flip a byte in `referenceState`/`referenceUpdate` |
| `permute-attr` | reorder the entries of an `attrs` set |
| `drop-op` | remove one op from `ops` |
| `dup-op` | duplicate one op (injected only where the op is **not idempotent**) |
| `reorder-op` | swap two ops (injected only where they are **not commutative**) |

This table is the **single source of truth** for the mutant kinds; tasks.md (T010), research.md,
and contracts/oracle-contract.md (C-O7) reference it rather than re-listing.

**Invariant (SC-002)**: every *applicable* fault MUST produce `match == false`. `reorder-op`/`dup-op`
are applicable only on order-dependent / non-idempotent cases — a reorder on a commutative multiset
legitimately still matches and is not a missed detection. If any applicable mutant still matches,
the oracle is blind there and the gate is invalid.

## JSValue (the value model)

The canonical model the value layer enforces. A value is exactly one of:

| State | Go representation | JS equivalent |
|---|---|---|
| *absent* | key not in the map / `nil` | `o.get(k) === undefined` |
| *Null* | the `Null` sentinel (`NullType{}`) | `o.get(k) === null` |
| *value* | a concrete value | a concrete value |

**Rule**: `Null` and absent are **distinct** and never conflated; `GetOrNull` collapses both to the
Yjs `?? null` result *only at the read site*, never in storage.

## Attrs (the attribute type)

Insertion-ordered attribute map; the one type all formatting/attribute code uses.

| Operation | Contract |
|---|---|
| `Get(k) (any, bool)` | distinguishes absent (`false`) from present-null (`true, Null`) |
| `GetOrNull(k) any` | `present? value : Null` — the Yjs `o.get(k) ?? null` (defined once) |
| `Set(k, v)` | preserves first-insertion order of `k` |
| `Each(fn)` | iterates in **insertion order** (matches JS `for..in`) — FR-011 |
| `Len()` | count |

**State transitions**: `Set` on a new key appends to the order; `Set` on an existing key updates
value, order unchanged (JS object semantics).

**Equality is per-semantic, not single (FR-010)**: formatting uses **shallow** `EqualAttrs` (lib0
`equalFlat` — reference identity for nested values), awareness uses **deep** `equalAttrsDeep` (lib0
`equalityDeep` — structural). Both already exist in `utils.go`, each defined once; they are **not**
collapsed (shallow is required for Y.Text parity, deep for awareness — see
[value-rep-contract.md](./contracts/value-rep-contract.md) C-V3). `Object.GetOr` (→ Go `nil`) is a
*different* accessor from `GetOrNull` (→ `Null`) and must not be conflated.
