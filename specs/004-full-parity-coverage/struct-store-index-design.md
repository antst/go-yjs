# Chunked per-client struct store design

Status: design only. Do not implement until the flat-store encapsulation phase and its
equivalence oracle have landed independently.

## Why this exists

Every client currently owns one sorted `[]IAbstractStruct`. Splitting an Item inserts the
right half into that slice with `SpliceStruct`, which shifts the entire suffix. A document
created by one large paste followed by uniformly random one-character deletes therefore
performs one or two middle insertions per edit.

Measured on Apple M1 Max, with setup included identically at every size:

| deletes | elapsed |
|---:|---:|
| 2,000 | 6.45 ms |
| 8,000 | 23.5 ms |
| 16,000 | 85.8 ms |
| 32,000 | 312.4 ms |
| 64,000 | 1.239 s |

The local doubling ratios are 3.64x from 16k to 32k and 3.97x from 32k to 64k.
Once suffix copying dominates, the operation is effectively quadratic. At 32k,
`FindIndexCleanStart` accounts for 53% cumulative CPU, `SplitItem` for 37.5%, and
`SpliceStruct`/`runtime.memmove` for about 28%.

This layout is inherited, not a Go-specific porting defect. Yjs inserts split structs into a
JavaScript Array with `splice`; yrs 0.27.3 stores each client's blocks in a Rust `Vec` and uses
`Vec::insert`. On the same 32k fixture Go is already about 55x faster than Yjs, so changing the
layout is a scaling investment and an algorithmic lead, not an urgent benchmark repair.

## Why a single gap buffer is not enough

A gap buffer helps when edits remain near the previous edit. For independent uniformly random
split positions, the expected gap movement is proportional to the store length, so cumulative
work remains quadratic. Multiple fixed gaps only divide that constant. They do not remove the
term unless their number grows with the document, at which point the design has become an index
over chunks in another form.

## Required migration order

The representation change must not be combined with the call-site refactor. The store is used by
encoding, decoding, GC, snapshots, undo, delete sets, integration, and transaction cleanup.

### Phase 1: encapsulate the existing flat representation

1. Replace the exported `StructStore.Clients map[Number]*[]IAbstractStruct` with a private client
   table whose value is a concrete `clientStructList`.
2. Keep `clientStructList` backed by the current flat slice. This phase must not change layout or
   algorithms.
3. Route all access through a small operation set:
   - client lookup, creation, count, and deterministic client iteration;
   - first, last, and lookup by clock;
   - append and capacity reservation;
   - cursor-based insert, replace, and remove;
   - forward and reverse range iteration.
4. Replace numeric-index mutation loops with cursors where the next operation is adjacent. A
   cursor is an implementation detail and may not escape a transaction or survive a mutation
   unless that operation explicitly documents cursor stability.
5. Make `GetStructs` return an independent flattened slice. The existing method exposes internal
   storage, which is incompatible with any non-flat representation and is not used internally.

Phase 1 lands alone and runs the complete correctness and performance battery. Its purpose is to
prove that every production access has crossed the new boundary while bytes and timings remain
unchanged.

### Phase 2: segmented leaves, flat directory

Replace the flat slice inside `clientStructList` with fixed-capacity leaves (initial target: 256
struct interfaces) and a sorted directory of leaf summaries.

Each leaf stores:

- a bounded slice/array of structs in clock order;
- its first clock and exclusive end clock;
- links to the preceding and following leaf for allocation-free sequential iteration.

The directory stores leaf pointers in clock order. Lookup binary-searches the directory and then
the selected leaf. A split insert shifts at most one leaf; a full leaf is divided and adds one
directory entry. Removal compacts one leaf. Empty leaves are removed; optional rebalancing is
allowed only after its effect on cleanup and encoding has been measured.

This phase does not claim a strict asymptotic solution: directory insertion is still linear in
the number of leaves. It reduces the practical suffix shift by orders of magnitude while keeping
the implementation small, and it establishes the leaf/cursor API needed by a tree directory.
For N inserts and leaf capacity B its rough work is `O(N*B + N^2/B^2)`, rather than the current
`O(N^2)`. The benchmark must state the observed exponent instead of calling it linear.

### Phase 3: tree directory, only if measurements justify it

Replace the flat leaf directory with a B+tree whose internal nodes summarize:

- subtree struct count;
- first clock and exclusive end clock;
- child count and parent linkage.

Leaves remain linked, so encoding and range scans are sequential. Clock lookup, ordinal lookup,
leaf insertion, and leaf removal become logarithmic. This is the phase that removes the remaining
directory-shift term; it should not alter callers or leaf behavior.

## Representation invariants

These invariants are checked after every operation in the randomized oracle, not only at
transaction boundaries:

1. Every client list contains only non-nil structs with that client ID.
2. Struct clocks are contiguous: `left.clock + left.length == right.clock`.
3. Flattening leaves yields exactly the logical sequence, including deleted and GC structs.
4. Leaf first/end clocks and every internal subtree summary equal a pointer walk.
5. Every leaf appears exactly once in directory order and exactly once in the linked-leaf chain.
6. Parent pointers, child counts, and subtree struct counts agree in both directions.
7. A cursor returned before a mutation is either explicitly repaired by that mutation or declared
   invalid; no stale cursor is silently accepted.
8. First-insertion client order remains unchanged. Numeric sorting remains confined to wire paths
   that already require it.
9. Flattened V1/V2 encoding is byte-identical to the flat representation.

On any production invariant failure, the safe behavior is to report an error. Unlike a derived
position cache, the struct store is authoritative state and cannot be destroyed and rebuilt from
the linked list without proving that list is complete.

## Equivalence oracle

The flat implementation remains available to tests as an oracle until the chunked implementation
has shipped through a full release cycle.

For every generated primitive operation, apply the operation to both stores and compare after
each step:

- client key set and first-insertion order;
- flattened pointer sequence and struct concrete type;
- IDs, lengths, deletion state, and clock contiguity;
- lookup result for every clock in small exhaustive cases and sampled boundary clocks in large
  randomized cases;
- forward/reverse range iteration;
- cursor insertion, replacement, removal, and merge results;
- state vector, delete-set bytes, V1 bytes, and V2 bytes.

The generator must cover append, split at every offset, replace Item with GC, adjacent merge,
multi-entry removal, missing clients, late client creation, snapshot ranges, undo/redo, and remote
integration in non-numeric client order. Small stores are exhaustively enumerated; large stores use
random sequences with a recorded seed.

The test must also prove it is not vacuous: counters assert that every mutation primitive,
leaf split, leaf removal, and directory insertion was exercised. Fault injections that swap two
entries, skip one summary update, and retain a removed leaf must each make the oracle fail.

The full differential gate also runs with shadow comparison enabled. That makes every store
primitive generated by the existing cross-implementation workloads pass through the flat oracle;
the standalone generator supplements those workloads but is not allowed to define the entire
coverage surface itself.

## Performance and release gates

Correctness gates:

- all package tests and lint;
- the raised race battery;
- batched-vs-per-operation oracle at one million seeds;
- full Node differential, both directions;
- flat-versus-chunked store oracle described above.

Hard performance baselines include all existing non-batched rows, the three batched rows,
EncodeV1/V2, ApplyV1/V2, snapshot creation, GC cleanup, undo/redo, and delete-set creation. A faster
random delete cannot be purchased with slower sequential encoding or apply.

Scaling is measured at 8k, 16k, 32k, 64k, 128k, and 256k for:

- one large text paste followed by random one-character deletes;
- dense random inserts;
- sequential append and batched append;
- tombstone-heavy array cleanup;
- multi-client remote apply.

The segmented phase must materially lower the post-16k exponent and allocation growth. The tree
directory phase is justified only if the directory becomes visible in profiles or the segmented
phase fails the deployment-size scaling target.

## Decision

Do not implement this immediately. The current library already beats Yjs by about 55x at 32k on
the target fixture, while the migration crosses the most authoritative structure in the system.
Keep the design ready, continue profiling other 100k+ workload shapes, and start Phase 1 only when
the scaling benefit outranks the review and correctness surface.
