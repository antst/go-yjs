# Contributing

## The one rule that matters

**Compatibility with the JavaScript Yjs implementation is the correctness test.**
Not the existing Go tests, not what seems reasonable — what `yjs@13.6.31` does
with the same bytes. When you are unsure how something should encode or behave,
read the Yjs source. Do not guess and do not infer it from this codebase, which
is where a wrong guess would already be living if it existed.

## Getting set up

```
go test ./...                      # the suite
go test ./... -race                # whole module, never a filtered -run
golangci-lint run                  # config is committed; must report 0
```

For the differential oracle you also need Node 24 and the pinned reference:

```
cd fuzz && npm ci                  # npm ci, never npm install
bash fuzz/run-gate.sh --tier fast --dir both
```

`npm ci` matters: the whole claim is byte-compatibility with a specific `yjs`
version. A resolved-but-different version would silently change what
"compatible" means, and the differential would still pass.

A `pre-push` hook runs lint, build, vet in both build configurations, the suite,
the fuzz corpus replay and the oracle gate. `core.hooksPath=.githooks` enables it.

## What a change needs

**Root cause before fix.** A change that makes a symptom go away without an
explanation of why it occurred will be sent back. "Speculative fix" is a
rejection reason on its own.

**A test that fails without it.** Not coverage — a test that defends a named
invariant. The standard here is that a test must be shown to *reject*: plant the
defect it is meant to catch and watch it fail. Several tests in this repository
were written, passed, and were then found to be checking nothing; the ones that
survived were the ones someone tried to break. If your test cannot fail, it is
not evidence.

**Both codecs, if you touch encoding.** V1 and V2 are both complete and
byte-exact. A change to one without the other is incomplete.

**The public API contract updated deliberately.** `internal/apicontract` holds
every exported identifier. If your change moves the surface, update
`testdata/public_api.txt` in the same commit — that file is the record of an API
decision, not a generated artefact to refresh without reading.

## What gets turned down

- **Guessed Yjs behaviour.** See above.
- **Transport or server code in the core.** Transport belongs to your service.
- **New runtime dependencies.** There are currently zero and `go.sum` is empty.
  That is a feature, and the bar for the first one is high.
- **Abstractions for hypothetical consumers.** A new interface, mode, option or
  compatibility path needs a concrete caller whose correct behaviour depends on
  it, and a description of what goes wrong without it. "Future consumers may
  need" is not an argument; neither is "for flexibility".
- **Stubs and placeholders.** Unfinished code that compiles is worse than absent
  code, because it reads as done.
- **Comments explaining obvious code.** Comments here carry the *why* — the
  measurement, the Yjs source line, the failure that made a rule necessary.

## Backends

If you implement `persistence.Store` or `persistence.CheckpointStore`, run the
conformance suites in `backend/conformance` against it. They are adversarial on
purpose: an in-process hub is naturally ordered and duplicate-free, so the suite
reorders, duplicates and redelivers, because otherwise the shipped default would
quietly become the de-facto contract.

If a suite fails your implementation and you believe the suite is wrong, say so
before working around it. Two real contract defects were found exactly that way —
both times the contract was wrong, not the store.

## Style

`gofmt` and `goimports` are enforced. The committed `.golangci.yml` enables
fourteen linters and contains no exclusions and no `nolint` directives; if your
change needs one, that is a discussion, not a diff.

Commit messages explain *why*, in prose, at whatever length the reason needs.
Look at the log for the shape.
