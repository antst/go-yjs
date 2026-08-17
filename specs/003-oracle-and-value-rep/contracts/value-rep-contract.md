# Contract: Value Representation (`Attrs` / `EqualAttrs` / `GetOrNull`)

The single value-representation layer. Behavior is fixed to Yjs semantics; the Go signatures are
the contract, the internal layout is free.

## C-V1 — The three-state value model (FR-009)

A value read for a key is exactly one of:

| Read | `Get(k)` returns | `GetOrNull(k)` returns | JS analogue |
|---|---|---|---|
| key absent | `(nil, false)` | `Null` | `o.get(k) ?? null` where `o.get(k) === undefined` |
| present, explicit null | `(Null, true)` | `Null` | `o.get(k) === null` |
| present, value `v` | `(v, true)` | `v` | `o.get(k) === v` |

- `GetOrNull` is the **only** sanctioned way to express `o.get(k) ?? null`. No call site may write
  its own `x == nil ? Null : x` / `?? null` (SC-004). It consolidates the existing `attrOrNull`
  (`y_text.go:102`).
- **Distinct from `Object.GetOr`** (`type_define.go:79`), which returns Go `nil` for a missing key,
  NOT the `Null` sentinel. `GetOr` (→nil) and `GetOrNull` (→Null) are different accessors for
  different call-sites and MUST NOT be conflated.
- Storage MUST keep *absent* and *Null* distinct; only the `GetOrNull` read collapses them.

## C-V2 — Ordered iteration (FR-011)

- `Each(fn)` and any serialization MUST visit keys in **first-insertion order**, matching JS
  object `for..in`, so formatting emits byte-identical format markers.
- `Set` on an existing key updates the value and leaves order unchanged; `Set` on a new key
  appends.
- **Test**: building the same logical attrs in two insertion orders yields different `Each` orders,
  and the one matching Yjs's object order reproduces Yjs's bytes.

## C-V3 — One comparator per semantic (FR-010) — NOT one global function

There are **two** comparators, each defined once, and they MUST stay distinct (verified against
source — collapsing them breaks parity):

| Comparator | Semantic | Used by | Nested values | Source |
|---|---|---|---|---|
| `EqualAttrs` | **shallow** lib0 `equalFlat` | Y.Text **format attributes** | **reference identity** (one level) | `utils.go:353`, with the "must stay shallow" rationale comment immediately above it |
| `equalAttrsDeep` | **deep** lib0 `equalityDeep` | **awareness** state | **structural** (recursive) | `utils.go:498`; `awareness.go:155-166` |

`attrStrictEqual` (`utils.go:426`) is the shared `===` primitive both build on. Common cases for
both: both absent / both `Null` → equal; one `Null`, other absent → equal (after `?? null`); `Null`
vs a value → not equal; primitives → value-equal. They differ **only** on nested objects/arrays
(shallow = reference identity; deep = structural).

- **Why not one**: routing format attrs through the deep comparator changes the Y.Text item chain
  (`MinimizeAttributeChanges` marker-drop), breaking wire parity (constitution I); routing awareness
  through the shallow one re-introduces the 002 spurious-change bug. DRY (VII) is satisfied by *one
  definition per semantic*, which is already the case — it does **not** override I.
- US2's only equality-related work is folding the duplicated value type-switch enumerations
  (`abstract_type.go:547`, `:694`) into one helper (SC-004); the comparators themselves are not
  rewritten.

## C-V4 — Adoption (FR-009, FR-012)

All **format-attribute** sites use the layer's `GetOrNull` + shallow `EqualAttrs`:

- `InsertAttributes`, `MinimizeAttributeChanges`, `InsertNegatedAttributes`
- `GetDelta`, `ToDelta`, `cleanupFormattingGap`

**Awareness** keeps the layer's **deep** `equalAttrsDeep` (a member of the layer, not a separate
hand-rolled path) — never the shallow comparator.

**Gate (SC-004)**: a grep finds zero hand-rolled `?? null`/null-coalescing (and no stray
`attrOrNull(`) at call sites; one comparator per semantic (shallow `EqualAttrs` + deep
`equalAttrsDeep`) + one ordered attribute type.

## C-V5 — Invariants preserved

- Wire bytes unchanged for any input that was already correct (the layer is a refactor of
  representation, not of the encoding) — guarded by the oracle (C-O4/C-O5) and the V1/V2 fixtures
  (SC-006).
- No new runtime dependency; reflection use on the attribute hot path is reduced, not increased
  (perf-friendly for SC-007 and the later perf phase).
