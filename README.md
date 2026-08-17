# go-yjs

A Go implementation of the [Yjs](https://github.com/yjs/yjs) CRDT, wire-compatible with the JavaScript reference.

It exists so that a Go service can speak Yjs to browsers. You pull the module, implement the ports your deployment needs, and wire them together. The CRDT is the necessary part; the **contracts are the deliverable**.

```
go get github.com/antst/go-yjs
```

Requires Go 1.24+. No runtime dependencies.

## Layout

| Package | What it is |
| --- | --- |
| `crdt` | Documents, shared types, update encoding, transactions, snapshots, undo |
| `protocol` | Sync and awareness message framing |
| `backend` | Neutral identifiers shared by the ports — no CRDT values, no wire frames |
| `backend/persistence` | **Port you implement**: SQL, files, object store, your choice |
| `backend/memory` | Document registry — working in-process default, replaceable |
| `backend/hub` | Fan-out — working in-process default, replaceable |
| `backend/cluster` | **Optional.** Multi-node document ownership, typically Redis |
| `backend/conformance` | Public suites you run against your own implementations |

The repository root is a documentation page and exports nothing.

## The CRDT

Two replicas edit concurrently and converge. This is `Example_convergence` in `crdt/example_test.go` — every snippet below is a compiled, executed example, not prose.

```go
first := crdt.NewDoc("notes", crdt.WithClientID(1))
second := crdt.NewDoc("notes", crdt.WithClientID(2))

first.GetText("body").Insert(0, "hello", crdt.Object{})
second.GetText("body").Insert(0, "world", crdt.Object{})

// Each side ships what the other has not seen.
firstUpdate, _ := crdt.EncodeStateAsUpdate(first, crdt.EncodeStateVector(second))
secondUpdate, _ := crdt.EncodeStateAsUpdate(second, crdt.EncodeStateVector(first))

crdt.ApplyUpdate(first, secondUpdate, nil)
crdt.ApplyUpdate(second, firstUpdate, nil)

first.GetText("body").ToString() // "helloworld", on both replicas
```

Both inserted at position 0 with no knowledge of each other. The tie breaks deterministically by client ID — the same way the JavaScript implementation breaks it, which is checked on every push rather than assumed.

The example pins client IDs so its output is reproducible. Ordinarily a document is just `crdt.NewDoc("room-1")` — everything optional is a `DocOption`, so each departure from the defaults is named where you make it: `WithGC(false)` to keep deleted content addressable for snapshots, plus `WithClientID`, `WithMeta`, `WithAutoLoad` and `WithReadCache`.

Available types: `Y.Doc`, `Y.Text`, `Y.Array`, `Y.Map`, `Y.XmlFragment`, `Y.XmlElement`, `Y.XmlText`, with subdocuments, snapshots, relative positions, an undo manager, and garbage collection.

## Building a backend

### What you write, what ships

A single process serving Yjs needs a transport adapter and somewhere to put bytes. Everything else has a working default.

- **Transport adapter — yours.** WebSocket, SSE, gRPC, whatever your service already speaks. It is deliberately not in this module: transport belongs to your service, and a CRDT library that owns your connection lifecycle is one you have to fight.
- **Persistence — yours.** Implement `persistence.Store`. Appending updates and loading them back is the whole job; compaction is optional and additive.
- **Registry and hub — shipped.** Without defaults, every single-process service would first write a document registry and an in-process fan-out map: busywork with one correct answer, where each implementer gets the eviction and teardown races wrong differently.
- **Cluster — optional.** A single process is a first-class configuration, not a degraded one. Persistence takes the cluster fence as *optional*, and `Fence(0)` means "not clustered" rather than "unprotected".

### The server seam

`OnUpdate` gives you the exact bytes to persist and broadcast, plus the origin of the transaction that produced them.

```go
doc.OnUpdate(func(update []byte, origin any) {
    // update: the bytes to append to your store and forward to other clients
    // origin: whoever caused this, so you do not echo it back to them
})
```

The origin is what stops an echo loop. Apply a remote client's update with that client as the origin, and your handler can tell "someone else's edit, forward it" from "the edit I just applied on their behalf".

There is a typed subscription rather than a generic observer because this is the seam every server hangs off, and a `...interface{}` callback makes every consumer open with a type assertion that fails silently when it is wrong.

### Durability order and recovery

Applying a remote update and appending it to your store cannot be one atomic operation. A transport adapter must choose which failure it accepts:

- **Apply, then append.** Invalid update bytes never enter durable history. If append fails after a successful apply, the live document is ahead of storage.
- **Append, then apply.** A storage failure cannot leave the live document ahead. If semantic application then fails, the stored update can poison every later replay.

Inspecting the frame with `protocol.InspectMessage` is allocation-free and validates its framing, but only applying the update validates all CRDT semantics. Whichever order you choose, publish to the hub only after the append crosses its durability boundary.

For the apply-first failure, call `Registry.Invalidate`. It poisons the current generation before waiting, closes every outstanding `Handle.Done()` channel, and sends concurrent acquisitions to a freshly loaded document. Session loops stop serving when `Done` closes and release their handles; invalidation destroys the stale document after the last release. Context cancellation bounds the wait but never makes the poisoned generation current again.

### The sync handshake

```go
// Client announces what it has.
step1 := protocol.EncodeSyncStep1(client)

// Server answers with only the difference.
var reply bytes.Buffer
protocol.NewSyncHandler(server).HandleMessage(step1, &reply)

// Client applies it.
protocol.NewSyncHandler(client).HandleMessage(reply.Bytes(), &unused)
```

`SyncHandler` owns the framing. Your transport adapter only moves byte slices between the two sides.

### Conformance

`backend/conformance` ships importable suites for each port. Run them against your implementation:

```go
func TestMyPostgresStore(t *testing.T) {
    newStore := func() persistence.Store { return NewPostgresStore(db) }

    conformance.Persistence(t, newStore)           // every store must pass this
    conformance.PersistenceFencing(t, newStore)    // only if you report a fenced mode
    conformance.PersistenceCompaction(t, ...)      // only if you implement Compact
}
```

`conformance.Memory`, `conformance.Hub` and `conformance.Cluster` do the same for the other ports. Nothing here is optional-by-omission: a store that declares a fence mode is held to it.

The suites are adversarial on purpose. An in-process hub is naturally stronger than the `Hub` contract — ordered, no duplicates, no redelivery — so the suite reorders, duplicates and redelivers. Otherwise the shipped default would quietly become the de-facto contract and the first Redis implementation would fail in production against a suite that passed.

## Correctness

Compatibility with JavaScript Yjs is the correctness test, and it is enforced by a **differential oracle** rather than by hand-written expectations: random operation sequences run through both implementations and the results are compared byte for byte.

- 13 surfaces — text, array, map, XML, delta application, updates, undo, relative positions, sync, awareness, snapshots, GC, subdocuments
- **Both directions.** Direction A has the reference produce bytes we consume; direction B has us produce bytes the reference consumes. Only direction B can catch a non-canonical encoding, because in direction A the bytes never originate here.
- Tiers from 20,000 seeds on every push up to 10,000,000

Pinned against `yjs@13.6.31`, `y-protocols@1.0.7`, with cross-checks against `yrs` and `ygo`. Both the V1 and V2 update codecs are complete and byte-exact, including delta-coded delete sets.

Run it with `bash fuzz/run-gate.sh --tier fast --dir both`, which needs `node` and `cd fuzz && npm ci`.

## Performance

Benchmarks live in `bench/`, with matched workloads implemented four times — this library, `yjs`, `yrs` and `ygo` — driven by the same generator so the comparison is like for like. `bench/run-all.sh` runs them and `bench/status.py` reports, refusing to quote numbers measured against a different commit than the one checked out.

## Status

Pre-1.0. The CRDT and both wire formats are complete and gated; the backend ports are new and their shape may still move. There are no external consumers, so breaking changes are made when they are the right answer rather than deferred.

## Origins

This began as a fork of [skyterra/y-crdt](https://github.com/skyterra/y-crdt) by Qinghui Yao, which provided the initial Go port of the Yjs core and the V1 codec. It has diverged substantially since: 60 Go files became 260, and everything below was added or rewritten — the V2 codec, the sync and awareness protocol package, the backend ports and their conformance suites, the differential oracle, the fuzz targets, and the Yjs-parity work across formatting, snapshots, subdocuments, relative positions and undo.

It is a separate project rather than a maintained fork, but the lineage is real and the original copyright stands in [LICENSE](LICENSE) alongside the current one, as MIT requires.

## License

MIT — see [LICENSE](LICENSE), which carries both the original and the current copyright.
