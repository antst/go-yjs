# Library gaps — deferred until performance work lands

Written 2026-08-14, after benchmarking and source-comparing against
[`reearth/ygo`](https://github.com/reearth/ygo) (MIT), the other production Go Yjs implementation.

This is a **deferred work list**, not a plan. Nothing here is a correctness gap: the differential
oracle is green across 21 (surface, direction) cells and all 95 reference API exports have a Go
counterpart. These are the things that make a library pleasant and safe to *depend on*, and they
are being consciously postponed until the mutation-path performance work is done.

## Scope note: what is deliberately NOT on this list

ygo ships `persistence/` (a CGo-free SQLite store), `cluster/` (Redis relay), a WebSocket provider,
a `cmd/ygo-server` binary, and mobile bindings. **None of that belongs here.** Persistence and
transport are orthogonal to a CRDT library — the consuming product chooses them, and not every
consumer wants SQL at all.

That is not a neutral difference. Their layering has a measurable cost for a library consumer:

| | direct runtime dependencies |
|---|---|
| ygo | `modernc.org/sqlite`, `redis/go-redis`, `gorilla/websocket`, `miniredis`, `testify`, `x/sync`, `x/time` — **7** |
| this library | `mitchellh/copystructure` — **1** |

Importing their `crdt` package pulls a module graph containing a transpiled SQLite and a Redis
client. Module-graph pruning keeps them from linking, but they still land in `go.sum` and therefore
in vulnerability scanning, licence review, and dependency policy. Staying dependency-light is a
feature of this library, and the list below must not erode it.

---

## 1. Injectable logging *(highest priority — it is an API defect, not a missing feature)*

`Logf` is package-level global state. A library that logs to a global sink cannot be embedded
cleanly: the consumer cannot route output, attach request context, adjust level, or silence it in
tests without racing every other consumer in the process.

**Wanted:** an optional `*slog.Logger` on `Doc` (nil ⇒ silent), threaded to the places that
currently call `Logf`. `log/slog` is stdlib, so this costs no dependency.

**Watch:** several `Logf` call sites are on decode paths reachable from hostile input; keep them
non-fatal and make sure a nil logger is genuinely free rather than formatting-then-discarding.

## 2. Context-aware transactions

No way to bound or cancel a long apply. `ApplyUpdate` on a large hostile update runs to completion.
There are DoS ceilings (struct-count caps, delete-set bounds) but no caller-side deadline.

**Wanted:** `TransactContext` / `ApplyUpdateContext` equivalents that check cancellation at a safe
boundary — between struct integrations, not mid-integration, so a cancelled apply leaves a
consistent document rather than a half-integrated one. That boundary choice is the whole design
question here.

## 3. Resource caps the consumer can set

Internal DoS guards exist and are tested (`merge_dos_review_test.go`,
`merge_oom_ceiling_review_test.go`), but their limits are compile-time constants. A server embedding
this library cannot tighten them per-tenant.

**Wanted:** options along the lines of ygo's `WithMaxPendingItems` — consumer-tunable ceilings with
the current constants as defaults.

## 4. Observability

No way to see inside a document's health: pending structs awaiting missing dependencies, store size,
GC backlog. A consumer debugging a stuck sync has nothing to query.

**Wanted:** a small read-only stats accessor (`PendingStats()`-shaped). Deliberately minimal — a
metrics *interface* would be scope creep; a struct of counters is enough.

## 5. Consumer documentation

`specs/` is process material — feature specs, task lists, research notes. It documents how the work
was done, not how to use the result. There is no architecture overview, no "how do I wire this to a
transport", no explanation of the type system or the transaction model.

**Wanted:** `docs/ARCHITECTURE.md` (type system, transactions, encoding pipeline) and
`docs/USAGE.md` (wiring to a transport, awareness, undo, snapshots). ygo's `docs/` is a reasonable
shape to study, not to copy.

## 6. Project hygiene

Absent: `CHANGELOG.md`, `SECURITY.md`, `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`, and — most
actionable — **a committed `.golangci.yml`**.

The linter config gap is the one that actually bites: CI and every developer run whatever
`golangci-lint` defaults their installed version has, so "0 issues" is not reproducible across
machines or across a version bump. Pin the config next to the code.

No release process or semver commitment either. Pre-1.0 with no consumers makes that fine *today*;
it stops being fine the moment anything depends on this.

## 7. Code organisation

**63 files / ~16,900 lines** for this library's core against ygo's **29 files / ~13,150 lines** for
strictly more functionality. Some of that is Constitution II mandating a single package, but not
most of it — the file count is the tell.

Deferred deliberately: refactoring while the oracle is green is safe and cheap, but doing it *now*
would churn every file the performance work is touching. The right order is performance first, then
reorganise with the oracle holding the line.

## 8. Drop the last dependency

`mitchellh/copystructure` (+ `mitchellh/reflectwalk`) is the only runtime dependency, and
Constitution III asks for zero. It is reflection-based deep copy, which is also a performance
liability wherever it sits in a hot path.

**Wanted:** replace with explicit copy code for the handful of types that need it. Two wins at once
— genuine zero-dependency, and reflection out of the hot paths.

---

## Not gaps, recorded so they are not re-litigated

- **Attribution API.** ygo has one (`ContentAttribute`); yjs does not. Adding it would be *ahead* of
  the reference, not catching up, and it would be a wire-format extension — exactly the kind of
  divergence this project's constitution forbids without a deliberate decision.
- **Live cross-peer subdocument sync.** ygo does not have it either (their issue #142).
- **Browser client.** Out of scope for both; the browser stays on Yjs.
