// Package yjs is the module landing page for a Go implementation of the Yjs
// CRDT algorithms, wire-compatible with the JavaScript reference.
//
// It exports nothing. The code lives in the packages below, and this file
// exists so that the module has a canonical documentation page.
//
// # Layout
//
//   - [github.com/antst/go-yjs/crdt] — the CRDT: documents, shared types,
//     update encoding, transactions, snapshots, undo.
//   - [github.com/antst/go-yjs/protocol] — sync and awareness message framing.
//   - [github.com/antst/go-yjs/backend] — the ports a service implements, plus
//     supported in-process defaults and the conformance suites to check an
//     implementation against.
//
// # Using it
//
// A service that wants to serve Yjs to browsers pulls this module, implements
// the ports it needs, and wires them together. Persistence and the transport
// adapter are yours to write; the document registry and the fan-out hub ship
// with working in-process defaults you can replace. Clustering is optional and
// a single process is a first-class configuration, not a degraded one.
package yjs
