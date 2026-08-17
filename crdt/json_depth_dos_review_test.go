package crdt

import (
	"bytes"
	"strings"
	"testing"
)

// json_depth_dos_review_test.go reproduces DoS 2 from the SECOND code-review of
// PR antst/y-crdt#2: the order-preserving JSON decoder
// (decodeJSONValue/decodeJSONFromToken in json_ordered.go) recurses with NO
// depth bound. The streaming json.Decoder.Token() API does NOT enforce
// json.Unmarshal's nesting limit, so a deeply-nested JSON document — reachable
// through the PUBLIC ApplyAwarenessUpdate (awareness state JSON) and V1
// ApplyUpdate (ContentJson / ContentFormat values) — drives unbounded native
// recursion to a `fatal error: stack overflow` (unrecoverable, process-fatal).
//
// The fix threads a depth counter capped at maxAnyDepth (100, matching the
// binary readAnyDepth bound) and returns an error past it. We CANNOT trigger the
// real ~2M-deep crash in-process (it would kill the test runner), so the repro
// asserts the FIX boundary: a moderately-but-illegally deep document
// (depth 2000, far past 100) must ERROR. On the unpatched tree the same document
// decodes with a nil error (verified separately) — i.e. the recursion is
// unbounded; only its depth (and the available stack) decides crash vs. accept.

func nestedArrayJSON(depth int) string {
	return strings.Repeat("[", depth) + strings.Repeat("]", depth)
}

func nestedObjectJSON(depth int) string {
	// {"a":{"a":{ ... }}} — depth nested objects.
	return strings.Repeat(`{"a":`, depth) + "null" + strings.Repeat("}", depth)
}

// TestUnmarshalJSONOrderedRejectsDeepNesting is the core repro: depth 2000
// arrays/objects must be rejected with an error (was accepted on the unpatched
// tree → unbounded recursion → crash at higher depth).
func TestUnmarshalJSONOrderedRejectsDeepNesting(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
	}{
		{"deep-array", nestedArrayJSON(2000)},
		{"deep-object", nestedObjectJSON(2000)},
	} {
		_, err := unmarshalJSONOrdered([]byte(tc.data))
		if err == nil {
			t.Errorf("%s: expected a nesting-depth error for depth 2000, got nil (unbounded recursion DoS)", tc.name)
		}
	}
}

// TestUnmarshalJSONOrderedBoundIsMaxAnyDepth pins the JSON depth boundary to the
// SAME value the binary any-decoder admits. Both check the depth at the point of
// ENTERING a container (current depth, recursing children at depth+1) using
// `depth > maxAnyDepth`; for N nested '[' the innermost container is entered at
// depth N-1, so the deepest accepted document has N-1 == maxAnyDepth, i.e.
// N == maxAnyDepth+1 brackets decode and N == maxAnyDepth+2 errors.
//
// The previous assertion (N==maxAnyDepth+1 must error) was calibrated to the old
// `>=` JSON check, which rejected one level SHALLOWER than the binary
// readAnyDepth bound — the very inconsistency this change removes. The
// TestJSONAndBinaryDepthBoundsMatch test below cross-checks against the binary
// ReadAny path so the two can never drift again.
func TestUnmarshalJSONOrderedBoundIsMaxAnyDepth(t *testing.T) {
	// maxAnyDepth+1 opening brackets: innermost container entered at depth
	// maxAnyDepth, which passes `depth > maxAnyDepth` — must decode.
	atBound := nestedArrayJSON(maxAnyDepth + 1)
	if _, err := unmarshalJSONOrdered([]byte(atBound)); err != nil {
		t.Fatalf("%d brackets (innermost entry at depth maxAnyDepth) should decode, got error: %v", maxAnyDepth+1, err)
	}
	// maxAnyDepth+2 brackets: innermost container entered at depth maxAnyDepth+1,
	// which trips `depth > maxAnyDepth` — must error.
	overBound := nestedArrayJSON(maxAnyDepth + 2)
	if _, err := unmarshalJSONOrdered([]byte(overBound)); err == nil {
		t.Fatalf("%d brackets (innermost entry at depth maxAnyDepth+1) should error, got nil", maxAnyDepth+2)
	}
}

// TestJSONAndBinaryDepthBoundsMatch is the consistency guard for this change: it
// drives the IDENTICAL logical nesting through both the JSON decoder
// (unmarshalJSONOrdered) and the binary any-decoder (ReadAny) and asserts they
// accept/reject at exactly the same depth, so the `>=` vs `>` drift cannot
// silently come back.
func TestJSONAndBinaryDepthBoundsMatch(t *testing.T) {
	// nestedArrayAny(k) wraps k array levels around an innermost empty array, i.e.
	// k+1 nested arrays total — the same shape as nestedArrayJSON(k+1).
	nestedArrayAny := func(k int) any {
		var v any = ArrayAny{}
		for i := 0; i < k; i++ {
			v = ArrayAny{v}
		}
		return v
	}

	for _, k := range []int{maxAnyDepth - 1, maxAnyDepth, maxAnyDepth + 1, maxAnyDepth + 2} {
		jsonBrackets := k + 1 // equivalent JSON nesting

		_, jsonErr := unmarshalJSONOrdered([]byte(nestedArrayJSON(jsonBrackets)))

		enc := newEncoder()
		if err := writeAny(enc, nestedArrayAny(k)); err != nil {
			t.Fatalf("k=%d WriteAny: %v", k, err)
		}
		buf := bytes.NewBuffer(enc.Bytes())
		_, binErr := readAny(buf)

		if (jsonErr == nil) != (binErr == nil) {
			t.Fatalf("depth bound DRIFT at k=%d (%d JSON brackets): JSON err=%v, binary err=%v — the two decoders disagree on this nesting depth",
				k, jsonBrackets, jsonErr, binErr)
		}
	}
}

// TestApplyAwarenessUpdateRejectsDeepNestedState drives the deep-JSON payload
// through the PUBLIC ApplyAwarenessUpdate entry — the remote-reachable path —
// and asserts it is rejected with an error (the awareness state JSON decode goes
// through jsonObject → unmarshalJSONOrdered). On the unpatched tree a deep
// enough frame crashes the process; with the bound it returns an error.
func TestApplyAwarenessUpdateRejectsDeepNestedState(t *testing.T) {
	// Build a one-entry awareness frame whose state is a depth-2000 nested object.
	enc := newEncoder()
	writeVarUint(enc, 1) // one client
	writeVarUint(enc, 7) // clientID
	writeVarUint(enc, 1) // clock
	_ = writeString(enc, nestedObjectJSON(2000))
	frame := enc.Bytes()

	aw := NewAwareness(newDoc("", false, nil, nil, false))
	defer aw.Destroy() // NewAwareness auto-starts the reaper goroutine; stop it
	err := ApplyAwarenessUpdate(aw, frame, nil)
	if err == nil {
		t.Fatalf("ApplyAwarenessUpdate: expected an error for a depth-2000 nested state, got nil (unbounded recursion DoS)")
	}
	// And nothing was mutated (all-or-nothing apply): no client added.
	if _, ok := aw.states[7]; ok {
		t.Fatalf("ApplyAwarenessUpdate: state for client 7 was applied despite the malformed frame")
	}
}

// TestUnmarshalJSONOrderedAcceptsLegitNestedShallow guards against a
// false-reject: a genuinely nested but shallow document (well under the bound)
// must still decode, preserving structure and key order.
func TestUnmarshalJSONOrderedAcceptsLegitNestedShallow(t *testing.T) {
	// A realistic awareness-style nested state: cursor with a nested selection.
	data := `{"user":{"name":"Ann","color":"#f00"},"cursor":{"anchor":{"path":[0,1],"offset":3},"head":{"path":[0,1],"offset":7}}}`
	v, err := unmarshalJSONOrdered([]byte(data))
	if err != nil {
		t.Fatalf("legit nested-but-shallow JSON should decode, got error: %v", err)
	}
	obj, ok := v.(Object)
	if !ok {
		t.Fatalf("expected Object, got %T", v)
	}
	if obj.Len() != 2 {
		t.Fatalf("expected 2 top-level keys, got %d", obj.Len())
	}
	// Re-encode must be byte-identical (order-preserving), proving the depth bound
	// did not perturb the value tree for legitimate input.
	out := jsonString(v)
	if out != data {
		t.Fatalf("round-trip mismatch:\n got: %s\nwant: %s", out, data)
	}
}
