# Performance and retained read caches

Repeated read/serialization calls use bounded, mutation-invalidated projections for fragmented
text (`ToString`, `ToDelta`), maps (`Keys`, `Entries`, `ToJson`), and XML child slices. The caches
are deliberately deferred until repeated reads and are cleared by local and remote mutations. Map
projections have per-type width caps and the two large map projections are mutually exclusive.

This is a latency/memory tradeoff, not free speed. In an August 2026 retained-heap measurement of
200 documents, each containing 10,000 text characters and a 500-key map with every read cache
primed, cached documents retained 46.65 MB versus 31.05 MB uncached: **about 50% more heap, or
78 KB per document**. The corresponding 1,000-character case was about 45% / 65 KB per document.
Actual overhead depends on text fragmentation, delta segments, map width, and which read APIs are
used.

The measured bound relies on every projection being either capped absolutely or bounded by the
document itself. Revisit the memory envelope before adding an uncapped cache proportional to
document size or substantially raising the map projection caps.

Read caching is enabled by default because it turns the common repeated formatted-text and
serialization paths into bounded clones instead of complete CRDT walks. Servers retaining large
fleets of mostly-idle documents can opt out per document:

```go
doc := y_crdt.NewDoc(guid, true, y_crdt.DefaultGCFilter, nil, false,
	y_crdt.WithReadCache(false),
)
```

The option is fixed at construction. Disabling it changes only retained derived state and read
latency; document semantics and wire encoding are unchanged.

## Large plain-sequence mutation index

Random-position edits in a linked CRDT sequence eventually outgrow the reference search-marker
cache. With `C` markers and `N` physical Items, each edit pays both `O(C)` marker maintenance and
an average `O(N/C)` linked walk. The former adaptive `C ≈ sqrt(N)` schedule was therefore the
best possible tuning of that representation, but cumulative random insertion still approached
quadratic growth at large sizes.

Plain, unformatted sequences now activate a writer-only block index at 16,000 physical Items. It
maintains subtree visible lengths over blocks of linked Items, reducing mutation-position lookup
to logarithmic tree descent plus a bounded block walk. The index is derived state: it never enters
the wire encoding, is discarded on any failed invariant check, and is released when its containing
type or subdocument is deleted. The immutable concurrent-read index remains a separate mechanism.

Using the same 32-bit LCG workload at each size, cumulative random text insertion changed as
follows on an Apple M1 Max (August 2026):

| Final operations | Marker cache | Block index |
| ---: | ---: | ---: |
| 16,000 | 12.6 ms | 13.3 ms |
| 32,000 | 40.5 ms | 23.1 ms |
| 64,000 | 164.6 ms | 51.0 ms |
| 128,000 | 624.7 ms | 114.5 ms |
| 256,000 | 2.75 s | 289.7 ms |

The fitted exponent over this range fell from approximately **1.94 to 1.11**; the 256k workload is
about **9.5x faster**. The table also shows the cost of the threshold: at 16,000 Items the index is
about 5.6% *slower* than the marker cache it replaces, because activation is paid there and the
tree has not yet earned it back. It breaks even just above that point and is 43% faster by 32,000.
Lowering the threshold would move that penalty onto smaller documents for no benefit. At 256k, allocated benchmark bytes rose from 61.52 MB to 64.45 MB (+4.8%).
Existing small and non-batched benchmark rows retain their prior allocation counts.

Activation performs one linear pass and is paid by the mutation that crosses the threshold. Its
measured one-time cost was about 0.58 ms / 295 KB at 20k Items and 8.33 ms / 1.40 MB at 100k Items.

Formatted text uses the same structural index from 512 physical Items, but position lookup must
also recover the attributes active at the cursor. Each block counts live `ContentFormat` Items;
the lookup skips format-free subtrees and replays preceding live formats through the canonical
attribute reducer in document order. The resulting cost is `O(log N + F)`, where `F` is the number
of preceding format boundaries. A long stable formatting run therefore scales linearly, while a
format-dense document with `F ≈ N` does not gain the same bound.

Random inherited single-character insertion inside one bold run measured:

| Final operations | Linked attribute walk | Block + format index |
| ---: | ---: | ---: |
| 1,000 | 0.934 ms | 0.600 ms |
| 2,000 | 4.80 ms | 1.14 ms |
| 4,000 | 24.5 ms | 2.54 ms |
| 16,000 | 460 ms | 11.1 ms |
| 32,000 | 2.24 s | 24.5 ms |

Any formatting mutation discards the accelerator instead of maintaining it through churn. The
next inherited edit rebuilds once from the new stable formatting state. This preserves the
existing `TextFormatChurn` cost while preventing stale ordered attributes from being cached.

### Formatted sequences

Formatted text was originally excluded: a `ContentFormat` Item disables the mutable search markers,
because a position's active attributes depend on every live format to its left and a cached jump
would skip them. That left inherited-run editing quadratic — one bold run with random single-character
inserts measured 5.18 ms at 2,000 operations and 2.44 s at 32,000, with 77% of CPU flat in
`FindNextPosition`.

Stable formatted text now uses the same block index, carrying live local and subtree `ContentFormat`
counts alongside the visible lengths. A formatted lookup descends by visible length, replays only the
format-bearing subtrees before the selected block through the canonical `UpdateCurrentAttributes`, and
then performs the existing bounded block walk. Attributes are never cached: the query reads each Item
dynamically, so mutation of the exported `Key`/`Value` stays correct and no ordered-Object snapshot
can go stale.

Format insertion and deletion **destroy** the index rather than maintaining counts through churn; the
next inherited edit rebuilds it. This keeps format-dense churn on the original linked path at its
previous cost, and the realistic middle — formatting interleaved with typing — was measured rather
than assumed: on a 32,000-Item bold run with 2,000 random inherited inserts, toggling italic once
every 10, 50 and 100 inserts ran 2.75x, 9.8x and 18.9x faster than the linked path respectively.

The formatted activation threshold is 512 physical Items, far below the 16,000 used for plain
sequences, because formatted positioning is expensive per operation much earlier. Unlike the plain
index there is no penalty band above the threshold: measured with alternating base/indexed runs, the
index is already about 10% faster at 600 Items, 25% at 1,000 and 82% at 2,000.

The cost is allocated bytes. Because each format mutation discards the tree, a rebuild is paid per
format operation and allocates in proportion to the document: the 1-format-per-10-inserts cadence
above allocated 52.6 MB against the linked path's 0.93 MB, so a long-lived process editing very large
formatted documents should expect bytes proportional to document size times format frequency.

The allocation *count* is no longer part of that cost. Tree nodes come from fixed-capacity blocks
rather than one allocation each, which took the same cadence from 417,540 allocations per two
thousand inserts to 2,624 — a 99.4% reduction — and 20.5% off its median time, for 0.84% more bytes.
Blocks are separately allocated arrays that never move once published, so node addresses stay valid
as the arena grows, and destroying an index releases them: five create/destroy cycles on 19,000-Item
trees peak at 17.2 MiB and return to their starting heap.

### Relative positions on fragmented sequences

Relative positions back collaborative cursors, selections, comments, and anchors. Creating one from
a visible index previously walked from the sequence head, and resolving one walked left from its
anchor Item to reconstruct the visible prefix. Both operations were therefore linear in physical
Items even after mutation positioning became indexed.

When a block index is already active, relative-position reads now reuse it without activating,
rebuilding, or mutating derived state. Index-to-anchor lookup descends by subtree visible length;
anchor-to-index lookup accumulates preceding subtrees through parent links and scans only the
anchor's bounded block. Missing or inconsistent indexes retain the original linked fallback. This
keeps concurrent reads safe on a quiescent document: a deliberately index-free document remains
index-free after concurrent relative-position reads.

On a 128,000-Item randomly fragmented text, creation fell from 2.51-2.60 ms to about 0.51 us and
resolution from 2.46-2.52 ms to about 0.39 us, with identical allocations. A 100,000-character
single ContentString — where the old path was already constant-time — remains about 75 ns for
creation and 48 ns for resolution.

## Allocation reduction

A run of changes after the sequence indexes targeted allocation rather than algorithmic cost. Most
are invisible in wall-clock time on small documents and show up as allocation counts; a few moved
time substantially because the code they replaced was quadratic. All figures below were measured on
an Apple M1 Max in August 2026 and are per benchmark operation; the cross-implementation comparison
is generated separately from `bench/status.py` and is not reproduced here.

| Change | Before | After |
| --- | --- | --- |
| XML formatted-text render (2k chars, 500 spans) | 302 us, 284.7 KB, 8,014 allocs | 29.2 us, 31.0 KB, 7 allocs |
| Format past end of document (10k overrun) | 8.25 ms, 53.4 MB, 20,059 allocs | 11.1 us, 11.3 KB, 10 allocs |
| Observed text event delta (10k pieces) | 16.8 ms, 56.8 MB, 50,505 allocs | 4.14 ms, 3.08 MB, 10,500 allocs |
| Empty-observer checks (`TextAppendSmall`) | 436 us | 194-200 us |
| Delete-set pointers (`XmlSetAttribute`) | 112 allocs | 13 allocs |
| Scalar JSON encode (formatted V1) | 6.92 us, 73 allocs | 5.32 us, 15 allocs |
| Streamed delete set (formatted V2 full state) | 10 allocs | 1 alloc |

Two of these were quadratic rather than merely wasteful: the format-overrun path accumulated
newlines with one `fmt.Sprintf` per character, and the observed event delta concatenated every added
string the same way. Their improvements are correspondingly large and are not representative of the
others.

Existing unformatted paths are unchanged by design. `EncodeV1` and `EncodeV2` on unformatted
documents still allocate 2 and 1 times respectively with identical bytes, and the non-batched
mutation benchmarks retain their prior allocation counts exactly. Where a change could not preserve
that, it was rejected: an earlier delete-set representation removed most allocations but grew
`Transaction` from 288 to 304 bytes, crossing a malloc size class and adding 32 bytes to every
non-delete mutation, and was reworked to pack the existing booleans instead.

### Two constraints this work introduced

Several of these paths hand out **borrowed or arena-backed state**, which is correct only under
conditions that are invisible at the point someone would break them.

Internal renderers borrow the canonical delta cache rather than cloning it, so a consumer that
mutated a borrowed operator would corrupt the cache and change what a later *public* `ToDelta`
returns — for a different caller, persisting until invalidation. The render arenas therefore hold
copies, and cache entries are immutable: invalidation replaces an atomic pointer rather than
rebuilding an entry in place. Similarly, the delta text accumulator and the position-index node
blocks are append-only, so previously returned values are never written over, and the node blocks
are separately allocated fixed arrays whose addresses survive the outer slice growing.

The second constraint is **struct size**. Three separate changes were caught adding a field that
pushed a hot struct across a malloc size class, costing several percent before the intended work
delivered anything. `Transaction`, `Doc`, `AbstractType`, `YText` and `YArray` sizes are asserted;
a change that grows one of them should expect to reclaim the space from existing padding rather
than accept the larger class.
