'use strict';
// Canonical JSON serializer shared by the JS generator side.
// MUST match the Go canonicalizer in fuzz_gate_test.go byte-for-byte.
//
// Rules:
//  - objects: keys sorted ascending (code-point), emitted as {"k":<v>,...}
//  - arrays:  [<v>,<v>,...] in order
//  - strings: JSON.stringify (standard JSON string escaping)
//  - booleans/null: true / false / null
//  - numbers: integers emitted as the decimal integer (we only feed integers)

function canon(v) {
  if (v === null) return 'null';
  if (v === undefined) return 'null';
  const t = typeof v;
  if (t === 'string') return JSON.stringify(v);
  if (t === 'boolean') return v ? 'true' : 'false';
  if (t === 'number') {
    if (!Number.isInteger(v)) {
      throw new Error('non-integer number reached canon(): ' + v);
    }
    return String(v);
  }
  if (Array.isArray(v)) {
    return '[' + v.map(canon).join(',') + ']';
  }
  if (t === 'object') {
    const keys = Object.keys(v).sort();
    return '{' + keys.map(k => JSON.stringify(k) + ':' + canon(v[k])).join(',') + '}';
  }
  throw new Error('uncanonicalizable value of type ' + t + ': ' + String(v));
}

module.exports = { canon };
