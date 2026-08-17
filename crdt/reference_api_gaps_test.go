package crdt

import (
	"encoding/hex"
	"testing"
)

// Six public operations of the reference had no Go counterpart under any name. Five are functional
// (a snapshot-aware map read, a snapshot/update containment predicate, delete-set equality, update
// introspection, and update obfuscation); one is a debug helper. Each is implemented against the
// yjs@13.6.31 source, and each is covered by the differential oracle as well as by the unit tests
// here — a unit test proves the function runs, only the differential proves it AGREES.
//
// Written test-first: every one of these failed to compile before the implementation landed.

// typeMapGetAllSnapshot reads a Y.Map AS OF a snapshot. It is the Map counterpart of
// ToDelta-with-a-snapshot, which this feature found had never once executed — so this sits in the
// same history/time-travel area and gets the same scrutiny.
func TestTypeMapGetAllSnapshot(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	m := doc.GetMap("m")
	m.Set("keep", "v1")
	m.Set("changed", "before")
	snap := NewSnapshotByDoc(doc)

	// Everything after the snapshot must be invisible to it.
	m.Set("changed", "after")
	m.Set("added", "new")
	m.Delete("keep")

	asOf := typeMapGetAllSnapshot(doc.GetMap("m"), snap)
	if got, _ := asOf.Get("keep"); got != "v1" {
		t.Errorf("keep = %v, want \"v1\" — a key deleted AFTER the snapshot must still be visible in it", got)
	}
	if got, _ := asOf.Get("changed"); got != "before" {
		t.Errorf("changed = %v, want \"before\" — the snapshot must see the pre-snapshot value", got)
	}
	if _, ok := asOf.Get("added"); ok {
		t.Error("a key added AFTER the snapshot is visible in it")
	}

	// A key set AND deleted BEFORE the snapshot must be absent: the left-walk lands on a
	// tombstone that the snapshot can see, and only the visibility gate excludes it.
	//
	// This case is covered here rather than by the differential because the oracle cannot isolate
	// it: across 300 seeds Go's walk already terminates at nil for exactly the keys the reference
	// drops via isVisible, so the two agree on OUTPUT while reaching it by different paths, and
	// mutating the gate away does not move the differential. Output parity is what the oracle
	// proves; this test is what holds the branch itself.
	doc2 := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	m2 := doc2.GetMap("m")
	m2.Set("gone", "value")
	m2.Delete("gone")
	m2.Set("stays", "here")
	snap2 := NewSnapshotByDoc(doc2)
	asOf2 := typeMapGetAllSnapshot(doc2.GetMap("m"), snap2)
	if _, ok := asOf2.Get("gone"); ok {
		t.Error("a key deleted BEFORE the snapshot is visible in it; the visibility gate is not applied")
	}
	if got, _ := asOf2.Get("stays"); got != "here" {
		t.Errorf("stays = %v, want \"here\"", got)
	}

	// A nil snapshot or nil type must yield an empty result rather than panicking — both are
	// reachable from a caller that decoded a snapshot unsuccessfully.
	if got := typeMapGetAllSnapshot(doc2.GetMap("m"), nil); got.Len() != 0 {
		t.Errorf("nil snapshot returned %d keys, want 0", got.Len())
	}
	if got := typeMapGetAllSnapshot(nil, snap2); got.Len() != 0 {
		t.Errorf("nil type returned %d keys, want 0", got.Len())
	}

	// The live read must be unaffected, so the snapshot read is not mutating the document.
	live := typeMapGetAll(doc.GetMap("m"))
	if got, _ := live.Get("changed"); got != "after" {
		t.Errorf("live changed = %v, want \"after\"", got)
	}
	if _, ok := live.Get("keep"); ok {
		t.Error("live read still shows a deleted key")
	}
}

// snapshotContainsUpdate answers "does this snapshot already include everything in this update".
func TestSnapshotContainsUpdate(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	txt := doc.GetText("t")
	txt.Insert(0, "hello", Object{})

	early, err := EncodeStateAsUpdate(doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	snap := NewSnapshotByDoc(doc)

	// A snapshot taken here contains the update taken here.
	if ok, err := SnapshotContainsUpdate(snap, early); err != nil || !ok {
		t.Errorf("snapshot does not contain the update it was taken from (ok=%v err=%v)", ok, err)
	}

	// Content added AFTER the snapshot must NOT be contained.
	txt.Insert(5, " world", Object{})
	later, err := EncodeStateAsUpdate(doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := SnapshotContainsUpdate(snap, later); err != nil || ok {
		t.Errorf("snapshot claims to contain an update with later content (ok=%v err=%v)", ok, err)
	}

	// A deletion after the snapshot is also not contained — the delete-set half of the predicate,
	// which a state-vector-only check would miss.
	snap2 := NewSnapshotByDoc(doc)
	txt.Delete(0, 1)
	deleted, err := EncodeStateAsUpdate(doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := SnapshotContainsUpdate(snap2, deleted); err != nil || ok {
		t.Errorf("snapshot claims to contain an update carrying a later DELETE (ok=%v err=%v)", ok, err)
	}

	// V2 must agree with V1 on the same document.
	v2, err := EncodeStateAsUpdateV2(doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	full := NewSnapshotByDoc(doc)
	if ok, err := SnapshotContainsUpdateV2(full, v2); err != nil || !ok {
		t.Errorf("V2: snapshot does not contain the update it was taken from (ok=%v err=%v)", ok, err)
	}
}

func TestEqualDeleteSets(t *testing.T) {
	mk := func(spec ...[3]Number) *deleteSet {
		ds := newDeleteSet()
		for _, s := range spec {
			addToDeleteSet(ds, s[0], s[1], s[2])
		}
		return ds
	}
	a := mk([3]Number{1, 0, 2}, [3]Number{2, 5, 1})

	if !equalDeleteSets(a, mk([3]Number{1, 0, 2}, [3]Number{2, 5, 1})) {
		t.Error("identical delete sets compared unequal")
	}
	// Client insertion order must NOT affect equality — it is a set comparison, and Go map
	// iteration order would otherwise make the result nondeterministic.
	if !equalDeleteSets(a, mk([3]Number{2, 5, 1}, [3]Number{1, 0, 2})) {
		t.Error("delete sets differing only in client insertion order compared unequal")
	}
	if equalDeleteSets(a, mk([3]Number{1, 0, 2})) {
		t.Error("a delete set with fewer clients compared equal")
	}
	if equalDeleteSets(a, mk([3]Number{1, 0, 2}, [3]Number{2, 5, 9})) {
		t.Error("differing lengths compared equal")
	}
	if equalDeleteSets(a, mk([3]Number{1, 0, 2}, [3]Number{3, 5, 1})) {
		t.Error("a different client id compared equal")
	}
	if !equalDeleteSets(newDeleteSet(), newDeleteSet()) {
		t.Error("two empty delete sets compared unequal")
	}
}

func TestDecodeUpdate(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	txt := doc.GetText("t")
	txt.Insert(0, "abcdef", Object{})
	txt.Delete(1, 2)

	v1, err := EncodeStateAsUpdate(doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := decodeUpdate(v1)
	if err != nil {
		t.Fatal(err)
	}
	if len(dec.structs) == 0 {
		t.Error("decodeUpdate returned no structs for a non-empty update")
	}
	if dec.ds == nil || len(dec.ds.clients) == 0 {
		t.Error("decodeUpdate returned no delete set for an update carrying a deletion")
	}

	// V2 must describe the SAME document, since the two encodings carry the same content.
	v2, err := EncodeStateAsUpdateV2(doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	dec2, err := decodeUpdateV2(v2)
	if err != nil {
		t.Fatal(err)
	}
	if len(dec2.structs) != len(dec.structs) {
		t.Errorf("V1 decoded %d structs, V2 decoded %d — the same document must describe the same way",
			len(dec.structs), len(dec2.structs))
	}
	if !equalDeleteSets(dec.ds, dec2.ds) {
		t.Error("V1 and V2 decoded different delete sets from the same document")
	}

	// Malformed input must error rather than return a half-built result a caller reads as success.
	if _, err := decodeUpdate([]uint8{0xFF, 0xFF, 0xFF}); err == nil {
		t.Error("decodeUpdate accepted malformed bytes")
	}
}

// obfuscateUpdate strips content while preserving CRDT structure, so a document can be shared in a
// bug report. The structural invariant is what makes it useful and what must hold: the obfuscated
// update must still apply, and still produce the same shape.
func TestObfuscateUpdate(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	txt := doc.GetText("t")
	txt.Insert(0, "secret text", Object{})
	m := doc.GetMap("m")
	m.Set("password", "hunter2")
	arr := doc.GetArray("a")
	arr.Insert(0, ArrayAny{"confidential"})

	orig, err := EncodeStateAsUpdate(doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	obf, err := obfuscateUpdate(orig)
	if err != nil {
		t.Fatal(err)
	}

	// It must still be a valid update.
	out := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(9))
	_ = ApplyUpdate(out, obf, nil)

	// Structure preserved: same text LENGTH, same number of array entries, same key count.
	if got, want := out.GetText("t").Length(), txt.Length(); got != want {
		t.Errorf("obfuscated text length = %d, want %d — structure must survive", got, want)
	}
	if got, want := out.GetArray("a").GetLength(), arr.GetLength(); got != want {
		t.Errorf("obfuscated array length = %d, want %d", got, want)
	}
	om := out.GetMap("m")
	if got, want := om.GetSize(), m.GetSize(); got != want {
		t.Errorf("obfuscated map size = %d, want %d", got, want)
	}

	// Content removed: none of the secrets may survive anywhere in the obfuscated document.
	rendered := out.GetText("t").ToString()
	if rendered == "secret text" {
		t.Error("obfuscated text still contains the original string")
	}
	for _, secret := range []string{"secret", "hunter2", "confidential", "password"} {
		if containsBytes(obf, secret) {
			t.Errorf("the obfuscated update still contains %q", secret)
		}
	}

	// V2 must obfuscate too, and remain applicable.
	v2, err := EncodeStateAsUpdateV2(doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	obf2, err := obfuscateUpdateV2(v2)
	if err != nil {
		t.Fatal(err)
	}
	out2 := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(9))
	_ = ApplyUpdateV2(out2, obf2, nil)
	if got, want := out2.GetText("t").Length(), txt.Length(); got != want {
		t.Errorf("V2 obfuscated text length = %d, want %d", got, want)
	}
	for _, secret := range []string{"secret", "hunter2", "confidential"} {
		if containsBytes(obf2, secret) {
			t.Errorf("the obfuscated V2 update still contains %q", secret)
		}
	}
}

func TestLogType(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	txt := doc.GetText("t")
	txt.Insert(0, "abc", Object{})
	txt.Delete(1, 1)

	got := logType(txt)
	if got == "" {
		t.Fatal("logType returned nothing for a non-empty type")
	}
	// It must describe every child INCLUDING deleted ones (the reference logs all children, then
	// the content of the undeleted ones) — a debug helper that hides tombstones is useless for
	// debugging exactly the problems tombstones cause.
	if !containsStr(got, "deleted") {
		t.Errorf("logType output does not mark tombstones: %s", got)
	}
	if logType(nil) == "" {
		t.Error("logType(nil) must describe the nil type rather than return empty")
	}
}

func containsBytes(b []uint8, s string) bool { return containsStr(string(b), s) }

func containsStr(hay, needle string) bool {
	if len(needle) > len(hay) {
		return false
	}
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// YArray.Push must append after the last ITEM, tombstones included — not at the visible index.
//
// Go implemented it as Insert(length, ...), which lands BEFORE trailing tombstones and gives the
// new item a right origin where the reference gives it a left origin: one byte different in the
// item info byte (0x48 vs 0x88), so the documents are wire-incompatible.
//
// Found by the differential the moment `push` was added to the array generator. The reference is
// genuinely inconsistent here — YArray.push uses typeListPushGenerics while YXmlFragment.push uses
// insert(this.length, ...) — so both behaviours are pinned, and making them agree would be a
// deviation rather than a tidy-up.
func TestPushAppendsAfterTombstones(t *testing.T) {
	// Byte vector from yjs@13.6.31 for: unshift([40]); delete(0,1); push(["z"]) with clientID 1.
	const ref = "0102010008010161017d288801000177017a0101010001"

	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	arr := doc.GetArray("a")
	arr.Unshift(ArrayAny{40})
	arr.Delete(0, 1)
	arr.Push(ArrayAny{"z"})

	got, err := EncodeStateAsUpdate(doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(got) != ref {
		t.Errorf("Push after a tombstone diverged from the reference\n go  = %s\n ref = %s",
			hex.EncodeToString(got), ref)
	}

	// And the XML fragment must keep the reference's OTHER behaviour: index-based, so the new node
	// lands before the tombstone.
	const refXML = "0102010007010166030364697647010003036469760101010001"
	xdoc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	frag := xdoc.GetXmlFragment("f")
	frag.Insert(0, ArrayAny{NewYXmlElement("div")})
	frag.Delete(0, 1)
	frag.Push(ArrayAny{NewYXmlElement("div")})
	gotXML, err := EncodeStateAsUpdate(xdoc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(gotXML) != refXML {
		t.Errorf("XmlFragment.Push diverged from the reference\n go  = %s\n ref = %s",
			hex.EncodeToString(gotXML), refXML)
	}
}
