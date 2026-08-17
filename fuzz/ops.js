'use strict';
const Y = require('yjs');
const { mulberry32 } = require('./harness/core.cjs');

// Deterministic PRNG (mulberry32) so a seed fully reproduces an op sequence.

// LETTERS feeds randStr, which is used for Y.Text/Y.XmlText CONTENT, Y.Map/Y.Array
// string values, and (crucially for the JSON-escaping byte-parity gate) the
// special-char format-attribute values below. It deliberately includes the
// characters whose JSON.stringify escaping differs from Go encoding/json — < > &
// (Go HTML-escapes them; JSON.stringify emits them literally), the mandatory
// escapes " \ , the literal-in-JSON /, and the line/paragraph separators U+2028 /
// U+2029 (Go always escapes them; JSON.stringify emits them raw). A V1
// format-attribute value carrying any of these is written via WriteJson ->
// MarshalJSONOrdered, so the V1 byte-identity check diverges unless that encoder
// matches JSON.stringify exactly.
const LETTERS = 'abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 <>&"\'\\/  ';
const MAP_KEYS = ['k0', 'k1', 'k2', 'k3', 'k4', 'k5'];
// Formatting attributes the harness applies to Y.Text / Y.XmlText ranges. Mixes
// single-key markers with MULTI-KEY attribute objects whose keys are in a fixed,
// deliberately NON-sorted insertion order (z,a,m / w,b). lib0/JSON.stringify
// emits these keys in that insertion order; a sorted (json.Marshal) or
// map-randomized Go encoder produces different bytes, so these exercise the
// object-key-ordering byte-parity path the round-trip gate now asserts.
//
// SPECIAL_FMT_ATTRS (appended below) carry string VALUES drawn from randStr,
// which now includes < > & " \ / U+2028 U+2029. Under V1 a format-attribute value
// is serialized through WriteJson -> MarshalJSONOrdered (JSON.stringify), so these
// exercise the string-escaping byte-parity path: the V1 byte-identity check
// diverges if MarshalJSONOrdered escapes those characters differently than JS.
const FMT_ATTRS_BASE = [
  { bold: true }, { italic: true }, { bold: null }, { italic: null },
  { color: 'red' }, { color: 'blue' }, { color: null },
  { z: 1, a: 2, m: 3 }, { w: true, b: 'x' }, { color: 'red', bold: true },
];
// Multi-key plain objects for Y.Map / Y.Array values, keys in fixed non-sorted
// insertion order. JSON.stringify / lib0 writeAny emit z,a,m (not a,m,z).
const MULTI_KEY_OBJECTS = [
  { z: 1, a: 2, m: 3 },
  { w: true, b: 'x', q: null },
  { name: 'n', id: 7, ok: false },
  { c: 3, b: 2, a: 1 },
];

function randStr(rng, n) {
  let s = '';
  for (let i = 0; i < n; i++) s += LETTERS[(rng() * LETTERS.length) | 0];
  return s;
}

// pickFmtAttr returns a formatting attribute object. Most draws come from the
// fixed FMT_ATTRS_BASE pool (single- and multi-key markers, exercising key
// ordering); a fraction are SPECIAL: a single key whose string VALUE is a
// randStr (so it may contain < > & " \ / U+2028 U+2029). Under V1 that value is
// serialized via WriteJson -> MarshalJSONOrdered, so a special value exercises
// the JSON string-escaping byte-parity path. Fixed key names keep reproducibility.
const SPECIAL_FMT_KEYS = ['note', 'data', 'title', 'href'];
function pickFmtAttr(rng) {
  if (rng() < 0.35) {
    const key = SPECIAL_FMT_KEYS[(rng() * SPECIAL_FMT_KEYS.length) | 0];
    return { [key]: randStr(rng, 1 + ((rng() * 6) | 0)) };
  }
  return FMT_ATTRS_BASE[(rng() * FMT_ATTRS_BASE.length) | 0];
}

function randInt(rng) {
  return ((rng() * 2000000) | 0) - 1000000;
}

// A scalar value safe for Y.Map / Y.Array. Avoids non-integer floats (lib0 may
// pick float32 where Go writes float64). Multi-key objects ARE emitted now: the
// byte-identity round-trip gate (decode JS update in Go -> re-encode -> compare to
// the JS bytes) requires object key ORDER to survive end to end, so they must
// appear in the generated docs. To keep key order reproducible and deliberately
// NON-sorted, a multi-key object is drawn from the fixed MULTI_KEY_OBJECTS pool
// (insertion order z,a,m / w,b,q / ...) rather than built from random keys.
function randScalar(rng, depth) {
  const r = rng();
  if (depth > 0 && r < 0.1) {
    // Fixed-order multi-key object (e.g. {z,a,m}) — drives the key-ordering path.
    return MULTI_KEY_OBJECTS[(rng() * MULTI_KEY_OBJECTS.length) | 0];
  }
  if (depth > 0 && r < 0.2) {
    const a = [];
    const n = 1 + ((rng() * 3) | 0);
    for (let i = 0; i < n; i++) a.push(randScalar(rng, depth - 1));
    return a;
  }
  if (r < 0.5) return randStr(rng, 1 + ((rng() * 6) | 0));
  if (r < 0.75) return randInt(rng);
  if (r < 0.9) return rng() < 0.5;
  return null;
}

function getTypes(doc) {
  return {
    text: doc.getText('t'),
    map: doc.getMap('m'),
    arr: doc.getArray('a'),
    xml: doc.getXmlFragment('x'),
  };
}

// Nested feature: embed fresh Y-type children inside Y.Map values / Y.Array items.
// Default ON (set FUZZ_FEATURES=0 to disable). This exercises the ContentType
// delete/GC cascade (work item 1.2): a parent overwrite/delete must tombstone the
// whole nested subtree identically in both impls, and toJSON must serialize it the
// same. Prelim Y types accept ops before integration, so we populate then embed.
const NESTED = process.env.FUZZ_FEATURES !== '0';

function makeChild(rng, depth) {
  const kind = (rng() * 3) | 0;
  if (kind === 0) {
    const m = new Y.Map();
    const n = 1 + ((rng() * 2) | 0);
    for (let i = 0; i < n; i++) {
      m.set(MAP_KEYS[(rng() * MAP_KEYS.length) | 0], randScalar(rng, depth));
    }
    return m;
  }
  if (kind === 1) {
    const a = new Y.Array();
    const items = [];
    const n = 1 + ((rng() * 2) | 0);
    for (let i = 0; i < n; i++) items.push(randScalar(rng, depth));
    a.insert(0, items);
    return a;
  }
  const tx = new Y.Text();
  tx.insert(0, randStr(rng, 1 + ((rng() * 4) | 0)));
  return tx;
}

// Apply ONE random op. Includes Y.Text formatting (the parity gap the v1 gate
// did not exercise): format() over a range and insert() with attributes.
function applyRandomOp(rng, types, clientTag) {
  const { text, map, arr, xml } = types;
  const pick = rng();

  // ---- Y.Text (insert / delete / format / formatted-insert) ----
  if (pick < 0.32) {
    const len = text.length;
    const sub = rng();
    if (len === 0 || sub < 0.45) {
      const idx = len === 0 ? 0 : (rng() * (len + 1)) | 0;
      const attrs = rng() < 0.4 ? pickFmtAttr(rng) : undefined;
      text.insert(idx, randStr(rng, 1 + ((rng() * 5) | 0)), attrs);
    } else if (sub < 0.7) {
      const idx = (rng() * len) | 0;
      const dl = 1 + ((rng() * Math.max(1, len - idx)) | 0);
      text.delete(idx, Math.min(dl, len - idx));
    } else {
      // format a range with one attribute
      const idx = (rng() * len) | 0;
      const fl = 1 + ((rng() * Math.max(1, len - idx)) | 0);
      const attr = pickFmtAttr(rng);
      text.format(idx, Math.min(fl, len - idx), attr);
    }
    return;
  }

  // ---- Y.Map ----
  if (pick < 0.55) {
    const key = MAP_KEYS[(rng() * MAP_KEYS.length) | 0];
    if (rng() < 0.7) {
      if (NESTED && rng() < 0.25) map.set(key, makeChild(rng, 1));
      else map.set(key, randScalar(rng, 2));
    } else {
      map.delete(key);
    }
    return;
  }

  // ---- Y.Array ----
  if (pick < 0.8) {
    const len = arr.length;
    if (rng() < 0.65 || len === 0) {
      const idx = len === 0 ? 0 : (rng() * (len + 1)) | 0;
      const cnt = 1 + ((rng() * 3) | 0);
      const items = [];
      for (let i = 0; i < cnt; i++) {
        if (NESTED && rng() < 0.2) items.push(makeChild(rng, 1));
        else items.push(randScalar(rng, 2));
      }
      arr.insert(idx, items);
    } else {
      const idx = (rng() * len) | 0;
      const dl = 1 + ((rng() * Math.max(1, len - idx)) | 0);
      arr.delete(idx, Math.min(dl, len - idx));
    }
    return;
  }

  // ---- Y.XmlFragment ----
  {
    const len = xml.length;
    const r = rng();
    if (r < 0.45 || len === 0) {
      const idx = len === 0 ? 0 : (rng() * (len + 1)) | 0;
      if (rng() < 0.5) {
        const xt = new Y.XmlText();
        xt.insert(0, randStr(rng, 1 + ((rng() * 4) | 0)));
        xml.insert(idx, [xt]);
      } else {
        const el = new Y.XmlElement('n' + ((rng() * 5) | 0));
        if (rng() < 0.5) el.setAttribute('a' + ((rng() * 3) | 0), randStr(rng, 3));
        xml.insert(idx, [el]);
      }
    } else if (r < 0.8) {
      const idx = (rng() * len) | 0;
      const child = xml.get(idx);
      if (child instanceof Y.XmlText) {
        child.insert(child.length, randStr(rng, 2));
      } else if (child instanceof Y.XmlElement) {
        child.setAttribute('a' + ((rng() * 3) | 0), randStr(rng, 3));
      }
    } else {
      const idx = (rng() * len) | 0;
      xml.delete(idx, 1);
    }
  }
}

module.exports = { mulberry32, getTypes, applyRandomOp, Y };
