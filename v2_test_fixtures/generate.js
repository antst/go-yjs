'use strict';
// V2 reference-payload generator for the Go y-crdt V2 compatibility suite.
//
// Pins yjs@13.6.31 (the verified-current reference; spec FR-004) and emits a
// single JSON file (fixtures.json) holding one record per named scenario. Each
// record carries:
//   - name        : stable scenario id (the Go test selects by name)
//   - clientID    : the fixed client id used to build the doc (the Go test mocks
//                   GenerateNewClientID to this value so output is byte-identical)
//   - guid        : the doc guid the Go side must construct with
//   - updateV1    : base64 Y.encodeStateAsUpdate(doc)
//   - updateV2    : base64 Y.encodeStateAsUpdateV2(doc)
//   - stateVector : base64 Y.encodeStateVector(doc)        (always V1-encoded)
//   - json        : doc.toJSON() (for state assertions)
//
// Determinism: yjs assigns doc.clientID randomly; we overwrite it with a fixed
// value immediately after construction and BEFORE any operation, so every struct
// id is reproducible. Some scenarios use several explicit client ids by building
// separate docs and merging them.
//
// Usage: node generate.js   (writes ./fixtures.json)

const fs = require('fs');
const path = require('path');
const Y = require('yjs');

const YJS_VERSION = require('yjs/package.json').version;
if (YJS_VERSION !== '13.6.31') {
  // Hard-fail: the spec pins this exact version for byte-equality.
  throw new Error(`expected yjs@13.6.31, got ${YJS_VERSION}`);
}

const b64 = (u8) => Buffer.from(u8).toString('base64');

// newDoc builds a Y.Doc with a fixed clientID and guid so the Go side can
// reproduce identical struct ids.
function newDoc(clientID, guid) {
  const doc = new Y.Doc({ guid });
  doc.clientID = clientID;
  return doc;
}

// record serializes one scenario built on a single fixed-client doc.
function record(name, clientID, guid, build) {
  const doc = newDoc(clientID, guid);
  build(doc);
  return {
    name,
    clientID,
    guid,
    updateV1: b64(Y.encodeStateAsUpdate(doc)),
    updateV2: b64(Y.encodeStateAsUpdateV2(doc)),
    stateVector: b64(Y.encodeStateVector(doc)),
    json: doc.toJSON(),
  };
}

// recordDoc serializes a scenario where the caller already produced the doc
// (e.g. a multi-client merge). clientID is informational only.
function recordDoc(name, doc, clientID, guid) {
  return {
    name,
    clientID,
    guid,
    updateV1: b64(Y.encodeStateAsUpdate(doc)),
    updateV2: b64(Y.encodeStateAsUpdateV2(doc)),
    stateVector: b64(Y.encodeStateVector(doc)),
    json: doc.toJSON(),
  };
}

const fixtures = [];

// ---- text operations ----
fixtures.push(record('text_insert_delete', 440166001, 'guid', (doc) => {
  const t = doc.getText('type');
  doc.transact(() => {
    t.insert(0, 'def');
    t.insert(0, 'abc');
    t.insert(6, 'ghi');
    t.delete(2, 5);
  });
}));

fixtures.push(record('text_unicode', 712003991, 'guid', (doc) => {
  const t = doc.getText('content');
  t.insert(0, 'héllo 世界 🌍 αβγ');
}));

// ---- text with formatting attributes (the parity gap the v1 gate flagged) ----
fixtures.push(record('text_formatting', 305501221, 'guid', (doc) => {
  const t = doc.getText('rich');
  t.insert(0, 'Hello World');
  t.format(0, 5, { bold: true });
  t.format(6, 5, { italic: true, color: '#ff0000' });
  t.insert(5, ' big', { bold: true });
}));

fixtures.push(record('text_formatting_embed', 661120044, 'guid', (doc) => {
  const t = doc.getText('rich');
  t.insert(0, 'ab');
  t.insertEmbed(1, { image: 'data:foo' }, { alt: 'pic' });
  t.format(0, 1, { bold: true });
}));

// ---- map operations ----
fixtures.push(record('map_set', 440166001, 'guid', (doc) => {
  const m = doc.getMap('test');
  doc.transact(() => {
    m.set('k1', 'v1');
    m.set('k2', 'v2');
  });
}));

fixtures.push(record('map_mixed_values', 902113447, 'guid', (doc) => {
  const m = doc.getMap('test');
  doc.transact(() => {
    m.set('s', 'string');
    m.set('n', 42);
    m.set('neg', -17);
    m.set('f', 3.5);
    m.set('b', true);
    m.set('nul', null);
    m.set('obj', { a: 1, b: [2, 3], c: 'x' });
    m.set('arr', [1, 'two', false, { k: 'v' }]);
  });
}));

// Encode-safe map values: types whose lib0 any-encoding is order- and
// width-deterministic in the Go port. Multi-key objects ARE now deterministic
// (the Go Object type preserves insertion order to match JS Object.keys), so a
// multi-key object is included here; only non-integer floats are still excluded
// (Go writes float64 where lib0 may pick float32 — covered by map_float_safe).
// Used for Go->JS byte-equality.
fixtures.push(record('map_encode_safe', 902113447, 'guid', (doc) => {
  const m = doc.getMap('test');
  doc.transact(() => {
    m.set('s', 'string');
    m.set('n', 42);
    m.set('neg', -17);
    m.set('b', true);
    m.set('bf', false);
    m.set('nul', null);
    m.set('single', { only: 'one' });
    m.set('arr', [1, 'two', true, null]);
  });
}));

// Multi-key objects with keys in deliberately NON-sorted insertion order. Before
// the insertion-ordered Object change these were NOT byte-deterministic in the Go
// port (map iteration randomized; json.Marshal sorted) and were excluded from
// byte-equality. They now ARE byte-identical: lib0 writeAny emits keys in JS
// insertion order, and the Go Object type reproduces that order on encode.
fixtures.push(record('map_multi_key_object', 717171717, 'guid', (doc) => {
  const m = doc.getMap('test');
  doc.transact(() => {
    m.set('zam', { z: 1, a: 2, m: 3 });            // non-sorted scalar object
    m.set('wbq', { w: true, b: 'x', q: null });    // mixed value types
    m.set('nested', { outer: { y: 9, x: 8 }, k: [1, { d: 4, c: 3 }] });
  });
}));

fixtures.push(record('array_encode_safe', 144440022, 'guid', (doc) => {
  const a = doc.getArray('test');
  doc.transact(() => {
    a.insert(0, [1, 'two', true, null, [9, 8]]);
    a.delete(1, 1);
    a.insert(2, [99]);
  });
}));

// Float values: previously divergent (Go always wrote float64 while lib0 picks
// the tightest of int/float32/float64). After the WriteAny tag-cascade fix these
// are byte-exact. 0.5 is float32-exact (tag 124); 0.1 is not (tag 123);
// integer-valued floats (2.0) collapse to a varint integer (tag 125); a large
// non-float32 integer (2^40+1) is float64.
fixtures.push(record('map_float_safe', 902113448, 'guid', (doc) => {
  const m = doc.getMap('test');
  doc.transact(() => {
    m.set('half', 0.5);
    m.set('tenth', 0.1);
    m.set('intf', 2.0);
    m.set('big', Math.pow(2, 40) + 1);
  });
}));

fixtures.push(record('map_nested_type', 778899001, 'guid', (doc) => {
  const m = doc.getMap('root');
  const inner = new Y.Map();
  m.set('child', inner);
  inner.set('leaf', 'value');
  const arr = new Y.Array();
  m.set('list', arr);
  arr.push(['a', 'b']);
}));

// ---- array operations ----
fixtures.push(record('array_insert', 2525665872, 'guid', (doc) => {
  const a = doc.getArray('test');
  a.push(['a']);
  a.push(['b']);
}));

fixtures.push(record('array_mixed', 144440022, 'guid', (doc) => {
  const a = doc.getArray('test');
  doc.transact(() => {
    a.insert(0, [1, 'two', true, null, { k: 'v' }, [9, 8]]);
    a.delete(1, 1);
    a.insert(2, [99]);
  });
}));

// ---- xml operations ----
fixtures.push(record('xml_fragment_insert', 2459881872, 'guid', (doc) => {
  const frag = doc.getXmlFragment('fragment-name');
  const xt = new Y.XmlText();
  frag.insert(0, [xt]);
  frag.insertAfter(xt, [new Y.XmlElement('node-name')]);
}));

fixtures.push(record('xml_attributes', 333221144, 'guid', (doc) => {
  const frag = doc.getXmlFragment('frag');
  const el = new Y.XmlElement('div');
  el.setAttribute('class', 'container');
  el.setAttribute('id', 'main');
  const txt = new Y.XmlText();
  txt.insert(0, 'hello');
  txt.format(0, 5, { bold: true });
  el.insert(0, [txt]);
  frag.insert(0, [el]);
}));

// ---- mixed operations (text + map + array + xml in one doc) ----
fixtures.push(record('mixed_all_types', 556677889, 'guid', (doc) => {
  const t = doc.getText('t');
  const m = doc.getMap('m');
  const a = doc.getArray('a');
  const x = doc.getXmlFragment('x');
  doc.transact(() => {
    t.insert(0, 'rich text');
    t.format(0, 4, { bold: true });
    m.set('key', { nested: [1, 2, 3] });
    a.push([1, 2, 3]);
    const el = new Y.XmlElement('span');
    el.setAttribute('lang', 'en');
    x.insert(0, [el]);
  });
}));

// ---- edge cases ----
fixtures.push(record('empty_doc', 100000001, 'guid', (doc) => {
  // touch a type so the doc exists, but apply no ops
  doc.getText('empty');
}));

fixtures.push(record('single_client_many_ops', 200000002, 'guid', (doc) => {
  const t = doc.getText('big');
  doc.transact(() => {
    for (let i = 0; i < 200; i++) t.insert(t.length, 'x');
  });
}));

fixtures.push(record('structs_only_no_deletes', 300000003, 'guid', (doc) => {
  const a = doc.getArray('a');
  doc.transact(() => {
    for (let i = 0; i < 10; i++) a.push([i]);
  });
}));

// delete-only update: a second doc deletes content the first created; the
// update from base->after-delete (diffed against the base SV) is structurally a
// delete-heavy payload. We expose the full doc here; the Go delete-set tests
// decode the delete set portion.
fixtures.push((() => {
  const doc = newDoc(400000004, 'guid');
  const a = doc.getArray('a');
  doc.transact(() => {
    a.insert(0, [1, 2, 3, 4, 5, 6, 7, 8]);
  });
  const svBefore = Y.encodeStateVector(doc);
  doc.transact(() => {
    a.delete(1, 2);
    a.delete(3, 2);
  });
  return {
    name: 'delete_only',
    clientID: 400000004,
    guid: 'guid',
    updateV1: b64(Y.encodeStateAsUpdate(doc)),
    updateV2: b64(Y.encodeStateAsUpdateV2(doc)),
    stateVector: b64(Y.encodeStateVector(doc)),
    // diff after the delete-creating transaction, encoded V2 — a delete-only payload
    deleteDiffV2: b64(Y.encodeStateAsUpdateV2(doc, svBefore)),
    deleteDiffV1: b64(Y.encodeStateAsUpdate(doc, svBefore)),
    json: doc.toJSON(),
  };
})());

// interleaved multi-client deletes: build two docs on different clients, sync,
// then delete from both — exercises the V2 delta-coded delete set across clients.
fixtures.push((() => {
  const d1 = newDoc(11110001, 'guid');
  const d2 = newDoc(22220002, 'guid');
  const a1 = d1.getArray('a');
  const a2 = d2.getArray('a');
  d1.transact(() => a1.insert(0, ['a', 'b', 'c', 'd']));
  Y.applyUpdate(d2, Y.encodeStateAsUpdate(d1));
  d2.transact(() => a2.insert(2, ['x', 'y']));
  Y.applyUpdate(d1, Y.encodeStateAsUpdate(d2));
  // now both delete different regions
  d1.transact(() => a1.delete(0, 2));
  d2.transact(() => a2.delete(3, 2));
  Y.applyUpdate(d1, Y.encodeStateAsUpdate(d2));
  Y.applyUpdate(d2, Y.encodeStateAsUpdate(d1));
  return recordDoc('multi_client_deletes', d1, 11110001, 'guid');
})());

// 1000+ distinct clients: stress the client RLE/diff coding. We merge 1100
// single-op docs, each on its own client id, into one doc.
fixtures.push((() => {
  const target = newDoc(900000009, 'guid');
  const updates = [];
  for (let i = 0; i < 1100; i++) {
    const d = newDoc(1000 + i, 'guid');
    d.getArray('a').push([i]);
    updates.push(Y.encodeStateAsUpdate(d));
  }
  for (const u of updates) Y.applyUpdate(target, u);
  return recordDoc('many_clients', target, 900000009, 'guid');
})());

const out = path.join(__dirname, 'fixtures.json');
fs.writeFileSync(out, JSON.stringify({ yjsVersion: YJS_VERSION, fixtures }, null, 0));
process.stderr.write(`wrote ${fixtures.length} fixtures to ${out} (yjs ${YJS_VERSION})\n`);
