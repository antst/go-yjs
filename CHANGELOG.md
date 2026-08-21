# Changelog

Notable changes per release. Pre-1.0: breaking changes are made when they are
the right answer rather than deferred, so **read the entry before moving a pin**.

Full reasoning for each release is in its annotated tag (`git show v0.0.6`) and
in the pull request it came from; this file is the index.

## Unreleased

### Changed — breaking

- **Naming brought in line with Go conventions.** `Doc.Guid` → `Doc.GUID`,
  `Doc.GetSubdocGuids` → `GetSubdocGUIDs`, `Doc.GetXmlFragment` →
  `GetXMLFragment`, and `ToJson` → `ToJSON` on `Doc`, `YArray`, `YMap`, `YText`
  and `YXmlFragment`. A library pitched as the standard Go Yjs implementation
  should not read as a transliteration of JavaScript.
- **`YXmlText` no longer carries two near-identical methods.** It had both an
  inherited `ToJson() interface{}` and its own `ToJSON() string`, returning
  *different* strings — `YText.ToString()` is plain text, `YXmlText.ToString()`
  builds the XML form. The rename collided them and exposed it. The public method
  keeps each type's Yjs-correct signature; polymorphism moved to an unexported
  method.
- Several internal signatures lost parameters and returns that no caller used
  (`transactMutation`, `writeVarIntSigned`, `writeStateVector`, `findMarker`,
  `spliceArray`, `findNextPosition`, `deleteText`).

### Added

- **A committed `.golangci.yml`**, so "0 issues" means the same thing on every
  machine and across linter upgrades. Fourteen linters, **no exclusions and no
  `nolint` directives** — adopting it took roughly 350 code fixes rather than
  configuration. Four linters from the source config are not enabled; each is
  recorded with its finding count and reason in `docs/LIBRARY-GAPS.md`.
- `SECURITY.md`, `CONTRIBUTING.md` and this file.

### Fixed

- **Misfiled and incorrect doc comments.** A 25-line block documenting
  `decodeAwarenessEntries` sat above `ParseAwarenessStateJSON`. `YArray.Splice`
  and `YXmlFragment.Slice` both carried `ToArray`'s description. ~70 exported
  comments did not name the identifier they documented, which is what godoc
  renders.
- `protocol.InspectMessage` wrapped an inner error with `%v` instead of `%w`,
  so it could not be unwrapped.
- Dead code removed: `replaceChar`, whose only callers were the tests proving it
  worked.

### Documentation

- README corrected: it claimed **Go 1.24+** while `go.mod` requires **1.26**, and
  it described implementing `persistence.Store` as "the whole job" although the
  checkpoint profile has existed since v0.0.2. `docs/PERFORMANCE.md`'s example
  did not compile — it used a package name that no longer exists and the
  pre-options `NewDoc` signature.
- The checkpoint codec requirement is now documented where an implementer will
  find it.

## v0.0.6 — 2026-08-19

Concurrency is part of the persistence contract, and single-codec stores are
first-class.

### Changed — breaking

- **Four conformance functions removed** — `PersistenceConcurrency`,
  `PersistenceCompactionConcurrency`, `PersistenceFencingConcurrency`,
  `CheckpointPersistenceConcurrency`. They were never in a tagged release.
  Concurrency is not a capability a store may decline, and a separate suite let
  a store pass the base one and skip exactly the rules hardest to satisfy. The
  behaviour now runs inside the canonical entrypoints, with no opt-out.
- `Store` and `CheckpointStore` now **state** that their methods are safe for
  concurrent use with atomic per-document decisions. Before this the only
  occurrence of the word "concurrent" in the contract was `Compactor`'s.

### Fixed

- **The checkpoint deletion suites hardcoded `EncodingV1`**, so a store
  supporting one codec failed them by construction — while `CheckpointPersistence`
  in the same package carried a skip specifically to accommodate such a store.
  Two suites disagreed about whether a single-codec store was legitimate.
- `Deleter`'s load-after-delete clause contradicted `ErrNotFound`'s own rule for
  a store whose delete leaves a pointer owned by another system dangling.

## v0.0.5 — 2026-08-19

The checkpoint codec is declared, and initialization belongs to the generation.

### Changed — breaking

- **`SaveCheckpointRequest.Encoding` is required.** A wrong-codec decoder
  returns an *empty* document with *no error*, in both directions, so a store
  that infers the codec cannot tell a wrong guess from an empty document. New
  sentinels: `ErrEncodingRequired`, `ErrUnsupportedEncoding`.
- **`memory.OpenFunc` no longer inherits the acquirer's deadline.** It runs under
  a context the registry owns, which keeps the winning caller's *values* and
  drops only its lifetime. Anything relying on the inherited deadline must impose
  its own. Note that new arrivals renew the last-waiter condition, so a busy
  document is not bounded by this context at all.

### Fixed

- A cancelled generation stayed joinable, so a later caller attached to an
  initialization that could no longer succeed.
- A failed open closed its completion channel twice and panicked.
- A waiter could be handed a document `Evict` or `Close` had already destroyed.

## v0.0.4 — 2026-08-18

Fenced checkpoint deletion, and a sentinel that pointed at data loss.
`CheckpointPersistenceDeletionFencing` added; `ErrNotFound`/`ErrCorrupt`
documentation rewritten because it pointed callers at the data-loss answer.

## v0.0.3 — 2026-08-18

Deletion, and the erasure ordering trap. Optional `persistence.Deleter`, plus
the documented ordering an erasure must follow — stop admitting, invalidate,
then delete — and why doing it in any other order silently restores content.

## v0.0.2 — 2026-08-18

The checkpoint persistence profile: `CheckpointStore` for backends that keep one
rewritable blob per document rather than an append log.

## v0.0.1 — 2026-08-17

First release. A fresh version line rather than a continuation of the upstream
fork's, because the result is a different product. V1 and V2 codecs complete and
byte-exact against `yjs@13.6.31`, the sync and awareness protocol package, the
backend ports and their conformance suites, and the differential oracle.
