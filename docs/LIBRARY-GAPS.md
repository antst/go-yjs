# Library gaps

Written 2026-08-14, after benchmarking and source-comparing against
[`reearth/ygo`](https://github.com/reearth/ygo) (MIT), the other production Go Yjs implementation.

Reviewed 2026-08-21. This is a **work list**, not a plan. Nothing here is a correctness gap: the
differential oracle is green across 21 (surface, direction) cells and all 95 reference API exports
have a Go counterpart. These are the things that make a library pleasant and safe to *depend on*.

The original framing — "deferred until the mutation-path performance work lands" — no longer
applies: that work has landed. Items 1-6 below are open. Item 8 is done and kept only as the
record of a closed decision.

## Scope note: what is deliberately NOT on this list

ygo ships `persistence/` (a CGo-free SQLite store), `cluster/` (Redis relay), a WebSocket provider,
a `cmd/ygo-server` binary, and mobile bindings. **None of that belongs here.** Persistence and
transport are orthogonal to a CRDT library — the consuming product chooses them, and not every
consumer wants SQL at all.

That is not a neutral difference. Their layering has a measurable cost for a library consumer:

| | direct runtime dependencies |
|---|---|
| ygo | `modernc.org/sqlite`, `redis/go-redis`, `gorilla/websocket`, `miniredis`, `testify`, `x/sync`, `x/time` — **7** |
| this library | none — `go.sum` is empty |

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

**84 non-test Go files** across `crdt`, `protocol`, `backend` and `internal`, against ygo's **29
files / ~13,150 lines** for a different functional split. Some of that is Constitution II mandating
a single CRDT package, but not most of it — the file count is the tell. The count has grown since
this was written, largely from the backend ports and their conformance suites, which did not exist
in the original comparison and which ygo does not have an equivalent of.

The performance work this was waiting on has landed, so the stated ordering constraint is gone.

## 8. Drop the last dependency — DONE

`mitchellh/copystructure` was the only runtime dependency. It has been replaced with explicit copy
code, so the module now has zero runtime dependencies and an empty `go.sum`, and reflection is out
of the copy paths.

---

## Not gaps, recorded so they are not re-litigated

- **Attribution API.** ygo has one (`ContentAttribute`); yjs does not. Adding it would be *ahead* of
  the reference, not catching up, and it would be a wire-format extension — exactly the kind of
  divergence this project's constitution forbids without a deliberate decision.
- **Live cross-peer subdocument sync.** ygo does not have it either (their issue #142).
- **Browser client.** Out of scope for both; the browser stays on Yjs.
