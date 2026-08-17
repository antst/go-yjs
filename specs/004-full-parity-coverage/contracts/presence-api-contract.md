# Contract: Presence API

Two types, so that exported state fields and a library-owned writer cannot coexist on one value.
Enforced by the type system rather than a doc comment, because an unguarded concurrent map read
aborts the Go process rather than raising a recoverable error.

## C-P1 — Plain type

- **C-P1.1**: MUST NOT start a goroutine or timer, ever.
- **C-P1.2**: State is exposed as **exported fields**. With no library-owned writer, concurrency is
  caller-scheduled and caller-owned — ordinary for a Go data structure.
- **C-P1.3**: Stale remote entries are judged expired **on access**, matching the reference's
  judgement of which clients are present.
- **C-P1.4**: Performs **no** local renewal. Documented parity limitation (FR-011): a client whose
  program stays quiet past the timeout is dropped by reference peers.
- **C-P1.5**: Requires **no** disposal call. Discarding leaves nothing behind.

## C-P2 — Managed type

- **C-P2.1**: Starts its ticker only on an explicit call by the consumer (`StartPresence()` on the in-repo `WSSharedDoc` consumer; the equivalent explicit starter on the managed type itself) — Constitution II's named
  exception ("unless explicitly requested by the consumer (e.g., awareness timeout cleanup)").
- **C-P2.2**: Reproduces the reference interval **in full**: periodic local renewal when half the
  timeout has elapsed, and reaping past the full timeout, emitting the same events with the same
  payloads.
- **C-P2.3**: State is exposed **only** through accessors that copy under its lock.
- **C-P2.4**: Stopping leaves no threads, timers, or registered callbacks.
- **C-P2.5**: **Presence parity claims attach to this type.** The plain type is safe and
  thread-free; it is not the parity claim.

## C-P3 — Invariants

- **C-P3.1**: No single value exposes both directly-readable state fields and a library-owned
  writer (FR-012a).
- **C-P3.2**: Both types are race-free under race detection — unconditionally for the plain type,
  through the accessors for the managed type.

## C-P4 — In-repo consumer

- **C-P4.1**: `WSSharedDoc` uses the **managed** type — its peers are remote by definition, so the
  plain type's lack of renewal would have reference peers drop it (FR-013a).
- **C-P4.2**: Its timer is started by an explicit `StartPresence()`, **never by
  `NewWSSharedDoc`** — a root-package constructor that starts a goroutine hands a consumer a
  library-owned thread without the explicit request Constitution II requires.
- **C-P4.3**: Its `update` observer then runs on the managed type's goroutine, not the caller's.
  That MUST be documented at the call site, since it changes which goroutine consumer code runs on.
