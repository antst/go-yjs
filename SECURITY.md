# Security

## Reporting a vulnerability

Report privately through GitHub's [Security Advisories](https://github.com/antst/go-yjs/security/advisories/new)
for this repository. Please do not open a public issue for anything you believe
is exploitable.

You will get an acknowledgement within a week. If you have not heard back in
that time, assume the report was lost and open a public issue saying only that
you sent a private report and got no reply — no details.

This is a small project with no paid security team and no bug bounty. What you
get is a straight answer about whether it is a real issue, and a fix if it is.

## What this library's threat model actually is

**It decodes untrusted bytes.** That is its job. A collaboration server hands
`ApplyUpdate`, `ApplyUpdateV2`, `MergeUpdates`, the snapshot and relative-position
decoders, the awareness decoders, and `protocol`'s message readers whatever
arrives on a socket. Anything reachable from those that panics, hangs, or
allocates without bound is a vulnerability in this library, not caller error.

In scope:

- A crafted update that panics, deadlocks, or allocates unboundedly.
- Bytes that decode to a document that does not match what the reference
  implementation produces from the same bytes — divergence is a correctness bug
  and can be a security one, because two peers then disagree about content.
- A decoder that reports success while silently dropping or corrupting content.
- Any way to read memory the caller did not hand in.

Out of scope, and deliberately:

- **Transport, authentication and authorisation.** There is none here. The
  library never opens a socket and has no concept of a user. Who may edit a
  document is your service's decision, made before it calls this code.
- **Persistence.** `backend/persistence` is a port you implement. Its contract
  is documented and its conformance suites are shipped, but the storage itself,
  its credentials and its access control are yours.
- **Denial of service from a legitimate client.** A peer that is entitled to
  edit can make a document arbitrarily large. Rate limits and quotas belong to
  the service.
- **Resource limits you can already reach.** Decoding has absolute ceilings on
  cumulative struct count so a small hostile update cannot amplify into an
  unbounded allocation, but they are set to accommodate the largest legitimate
  full-state update. They bound memory, not CPU.

## What is already done about it

- **Differential oracle against the JavaScript reference.** Thirteen surfaces,
  both directions, 20,000 random seeds on every push and tiers up to ten million.
  Direction B — bytes produced here and decoded by `yjs` — is the half that
  catches a non-canonical encoding, because in direction A the bytes never
  originate here.
- **Fuzz targets on every byte-ingress surface**, replayed from a committed
  corpus on every push, with per-call allocation budgets and no-hang deadlines
  rather than only checking for panics.
- **Absolute decode ceilings**, because structs-per-byte is unbounded for a
  legitimate fully-compressed update, so no purely proportional cap can separate
  an amplification attack from real input.

## Panics

The library contains internal invariant assertions that panic. They were
audited: none is reachable from untrusted encoded input, and the reasoning is a
trace through the guards rather than "the fuzzer has not found one". If you find
a byte sequence that reaches any panic through a public decoding entry point,
that is a vulnerability — report it.

Three panics are reachable through *caller* misuse of the public API
(`MakeObject` with an odd argument count or a non-string key, `NewUndoManager`
with an unsupported scope). Those are programming errors and are documented as
such, not security issues.

## Supported versions

Pre-1.0. Only the latest tagged release is supported; fixes land on `main` and
in the next tag rather than being backported.
