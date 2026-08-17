import * as Y from 'yjs'
import { Awareness, encodeAwarenessUpdate } from 'y-protocols/awareness.js'
import { hex } from './harness/index.mjs'

// Reference-produced artefacts for the oracle self-test (FR-007).
//
// The self-test must compare against the REFERENCE, not against this library's own output — the
// feature-003 version compared Go to Go and therefore proved only encoder sensitivity. These are
// small, exactly-reproducible documents so the Go side can rebuild them and the baseline is a true
// match rather than an approximation; the point is to perturb the Go value and require the
// comparison to notice, which only means something if the expected side came from yjs.

const out = {}

// A plain text document: insert + delete only, no attributes, so a Go replay is exact.
{
  const doc = new Y.Doc({ gc: false }); doc.clientID = 1
  const t = doc.getText('t')
  t.insert(0, 'hello')
  t.delete(1, 1)
  t.insert(2, 'XY')
  out.text = hex(Y.encodeStateAsUpdate(doc))
}

// Array of scalars.
{
  const doc = new Y.Doc({ gc: false }); doc.clientID = 1
  const a = doc.getArray('a')
  a.insert(0, [1, 2, 3])
  a.delete(1, 1)
  out.array = hex(Y.encodeStateAsUpdate(doc))
}

// Map with several keys.
{
  const doc = new Y.Doc({ gc: false }); doc.clientID = 1
  const m = doc.getMap('m')
  m.set('k0', 1); m.set('k1', 'two'); m.set('k2', true)
  m.delete('k1')
  out.map = hex(Y.encodeStateAsUpdate(doc))
}

// XML fragment with nested elements and an attribute.
{
  const doc = new Y.Doc({ gc: false }); doc.clientID = 1
  const f = doc.getXmlFragment('x')
  const el = new Y.XmlElement('div')
  f.insert(0, [el])
  el.setAttribute('id', 'a1')
  out.xml = hex(Y.encodeStateAsUpdate(doc))
}

// Formatted text -> delta, the applyDelta surface's artefact.
{
  const doc = new Y.Doc({ gc: false }); doc.clientID = 1
  const t = doc.getText('t')
  t.insert(0, 'hello')
  t.format(0, 3, { bold: true })
  out.applyDelta = JSON.stringify(t.toDelta())
}

// Awareness wire format.
{
  const doc = new Y.Doc(); doc.clientID = 1
  const aw = new Awareness(doc)
  clearInterval(aw._checkInterval)
  aw.setLocalState({ name: 7 })
  out.awareness = hex(encodeAwarenessUpdate(aw, [1]))
}

// A relative position's encoded bytes.
{
  const doc = new Y.Doc({ gc: false }); doc.clientID = 1
  const t = doc.getText('t')
  t.insert(0, 'hello')
  out.relpos = hex(Y.encodeRelativePosition(Y.createRelativePositionFromTypeIndex(t, 2, 0)))
}

// A sync step1 message (the state-vector message).
{
  const doc = new Y.Doc({ gc: false }); doc.clientID = 1
  doc.getText('t').insert(0, 'hello')
  out.syncSV = hex(Y.encodeStateVector(doc))
}

// A snapshot's encoded bytes.
{
  const doc = new Y.Doc({ gc: false }); doc.clientID = 1
  const t = doc.getText('t')
  t.insert(0, 'hello'); t.delete(1, 1)
  out.snapshot = hex(Y.encodeSnapshotV2(Y.snapshot(doc)))
}

process.stdout.write(JSON.stringify(out) + '\n')
process.stderr.write(`emitted=1 dropped=0 surface=selftest-probe\n`)
