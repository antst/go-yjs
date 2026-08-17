package crdt

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// ---------------------------------------------------------------- from y_text_content_kind_test.go
// T067a / FR-014d — content-kind countability in the text layer.
//
// The reference's text-position walkers (`itemTextListPosition.forward` and `findNextPosition` in
// yjs@13.6.31 src/types/YText.js) switch on the content constructor with exactly two arms:
//
//	case ContentFormat: ...update attributes...
//	default:            if (!deleted) { index += length }
//
// Go instead whitelisted three kinds (*ContentEmbed, *ContentString, *ContentType), so any OTHER
// content kind under a text parent was skipped by the index arithmetic — it did not advance the
// walk, so every later index was off and inserts, formats and deletes landed in the wrong place.
//
// This is REACHABLE, not theoretical, and needs no crafted bytes: a document whose root 't' is read
// as a Y.Text can legitimately receive an update in which 't' carries array-shaped items, because
// the item list is the same machinery for both. Probed against the reference, this puts
// ContentAny (length 2) and ContentBinary (length 1) into the text alongside ContentString("AB"),
// giving length 5 — the two extra items are counted by the reference and were NOT counted by Go.
//
// Expectations below are generated from yjs@13.6.31 with clientIDs pinned (writer 2, reader 1) so
// both the integration order and the encoded bytes are reproducible, and with GC DISABLED on both
// docs to match the Go document under test (yjs defaults gc:true, which rewrites a deleted item's
// content to ContentDeleted and would diverge the bytes for reasons unrelated to countability). `state` is the full
// encodeStateAsUpdate, which is what makes this a byte-exactness check (SC-010) and not merely a
// string check.
const contentKindArrayUpdateHex = "0102020008010174027d01770173830201010900"

type contentKindCase struct {
	name   string
	length int
	str    string
	delta  string
	state  string
}

var contentKindCases = []contentKindCase{
	{name: "insert@0", length: 7, str: "|EAB", delta: "[{\"insert\":\"|EAB\"}]", state: "0202020008010174027d01770173830201010902010004010174024142440100027c4500"},
	{name: "insert@1", length: 7, str: "A|EB", delta: "[{\"insert\":\"A|EB\"}]", state: "0202020008010174027d0177017383020101090301000401017401418401000142c401000101027c4500"},
	{name: "insert@2", length: 7, str: "AB|E", delta: "[{\"insert\":\"AB|E\"}]", state: "0202020008010174027d01770173830201010902010004010174024142c401010200027c4500"},
	{name: "insert@3", length: 7, str: "AB|E", delta: "[{\"insert\":\"AB|E\"}]", state: "0203020008010174017d0188020001770173830201010902010004010174024142c402000201027c4500"},
	{name: "insert@4", length: 7, str: "AB|E", delta: "[{\"insert\":\"AB|E\"}]", state: "0202020008010174027d01770173830201010902010004010174024142c402010202027c4500"},
	{name: "insert@5", length: 7, str: "AB|E", delta: "[{\"insert\":\"AB|E\"}]", state: "0202020008010174027d01770173830201010902010004010174024142840202027c4500"},
	{name: "format(0,4)", length: 5, str: "AB", delta: "[{\"insert\":\"AB\",\"attributes\":{\"b\":true}}]", state: "0202020008010174027d0177017383020101090301000401017402414246010001620474727565c6020102020162046e756c6c00"},
	{name: "delete(1,3)", length: 4, str: "A", delta: "[{\"insert\":\"A\"}]", state: "0202020008010174027d01770173830201010902010004010174014184010001420101010101"},
	{name: "baseline", length: 5, str: "AB", delta: "[{\"insert\":\"AB\"}]", state: "0202020008010174027d0177017383020101090101000401017402414200"},
}

// buildMixedContentText reproduces the probed document: a text root holding ContentString,
// ContentAny and ContentBinary items.
func buildMixedContentText(t *testing.T) (*Doc, *YText) {
	t.Helper()
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	txt := doc.GetText("t")
	txt.Insert(0, "AB", Object{})
	upd, err := hex.DecodeString(contentKindArrayUpdateHex)
	if err != nil {
		t.Fatal(err)
	}
	_ = ApplyUpdate(doc, upd, nil)
	return doc, txt
}

func TestTextContentKindCountability(t *testing.T) {
	for _, tc := range contentKindCases {
		t.Run(tc.name, func(t *testing.T) {
			doc, txt := buildMixedContentText(t)
			switch tc.name {
			case "baseline":
			case "format(0,4)":
				attrs := newObject()
				attrs.Set("b", true)
				txt.Format(0, 4, attrs)
			case "delete(1,3)":
				txt.Delete(1, 3)
			default:
				rest, ok := strings.CutPrefix(tc.name, "insert@")
				if !ok {
					t.Fatalf("unrecognised case name %q", tc.name)
				}
				idx, err := strconv.Atoi(rest)
				if err != nil {
					t.Fatalf("unrecognised case name %q: %v", tc.name, err)
				}
				txt.Insert(idx, "|E", Object{})
			}

			if got := txt.GetLength(); got != tc.length {
				t.Errorf("length = %d, reference = %d", got, tc.length)
			}
			if got := txt.ToString(); got != tc.str {
				t.Errorf("ToString() = %q, reference = %q", got, tc.str)
			}
			gotDelta, err := json.Marshal(deltaShape(txt.ToDelta(nil, nil, nil)))
			if err != nil {
				t.Fatal(err)
			}
			if canonJSON(t, string(gotDelta)) != canonJSON(t, tc.delta) {
				t.Errorf("ToDelta() = %s, reference = %s", gotDelta, tc.delta)
			}
			// The byte-exactness bar (SC-010): equal strings are not enough, the encoded
			// document must match the reference exactly.
			st, err := EncodeStateAsUpdate(doc, nil)
			if err != nil {
				t.Fatal(err)
			}
			if hex.EncodeToString(st) != tc.state {
				t.Errorf("encoded state diverged\n got = %s\n ref = %s", hex.EncodeToString(st), tc.state)
			}
		})
	}
}

// ---------------------------------------------------------------- from y_text_delta_cache_test.go
func TestDeltaTextAccumulatorKeepsReturnedSegmentsImmutable(t *testing.T) {
	acc := deltaTextAccumulator{capacityHint: 4}
	acc.Add("ab")
	acc.Add("cd")
	first := acc.Take()

	// The second segment fills the existing backing array beyond first's range.
	acc.Add("ef")
	acc.Add("gh")
	second := acc.Take()

	// The third forces the append-only arena to grow. Earlier strings must keep
	// their old backing arrays alive and must never observe later writes.
	acc.Add("ijkl")
	acc.Add("mnop")
	third := acc.Take()
	if first != "abcd" || second != "efgh" || third != "ijklmnop" {
		t.Fatalf("append-only delta text changed an earlier result: %q, %q, %q", first, second, third)
	}
}

func requireSingleTextDelta(t *testing.T, text *YText, wantText string, wantBold any) {
	t.Helper()
	ops := text.ToDelta(nil, nil, nil)
	if len(ops) != 1 || ops[0].InsertValue() != wantText {
		t.Fatalf("ToDelta() = %+v, want one insert %q", ops, wantText)
	}
	if got := ops[0].Attributes.GetOr("bold"); got != wantBold {
		t.Fatalf("ToDelta() bold = %#v, want %#v", got, wantBold)
	}
}

func TestYTextDeltaCacheIsolatesResultsAndValidatesExportedContent(t *testing.T) {
	doc := newDoc("delta-cache", false, defaultGCFilter, nil, false, WithClientID(1))
	text := doc.GetText("t")
	bold := newObject()
	bold.Set("bold", true)
	text.Insert(0, "abc", bold)

	_ = text.ToDelta(nil, nil, nil) // first unchanged read deliberately only primes
	first := text.ToDelta(nil, nil, nil)
	if text.deltaCache.Load() == nil {
		t.Fatal("ToDelta did not populate its cache")
	}
	first[0].InsertText = "caller mutation"
	first[0].Attributes.Set("bold", false)
	requireSingleTextDelta(t, text, "abc", true)

	var stringContent *contentString
	var formatContent *contentFormat
	for item := text.start; item != nil; item = item.right {
		switch content := item.content.(type) {
		case *contentString:
			stringContent = content
		case *contentFormat:
			if content.key == "bold" && content.value == true {
				formatContent = content
			}
		}
	}
	if stringContent == nil || formatContent == nil {
		t.Fatal("test setup did not create string and format content")
	}
	stringContent.value = "xyz" // same byte and UTF-16 length; must not reuse "abc"
	requireSingleTextDelta(t, text, "xyz", true)
	formatContent.value = false // direct exported-field replacement must invalidate logically
	requireSingleTextDelta(t, text, "xyz", false)
}

func TestYTextDeltaCacheInvalidatesOnLocalAndRemoteChanges(t *testing.T) {
	source := newDoc("delta-cache", false, defaultGCFilter, nil, false, WithClientID(1))
	text := source.GetText("t")
	text.Insert(0, "abc", Object{})
	_ = text.ToDelta(nil, nil, nil)
	_ = text.ToDelta(nil, nil, nil)
	if text.deltaCache.Load() == nil {
		t.Fatal("local ToDelta did not populate its cache")
	}
	text.Insert(3, "d", Object{})
	if text.deltaCache.Load() != nil {
		t.Fatal("local insert did not invalidate the delta cache")
	}
	if got := text.ToDelta(nil, nil, nil); len(got) != 1 || got[0].InsertValue() != "abcd" {
		t.Fatalf("local mutation returned stale delta: %+v", got)
	}

	initial, err := EncodeStateAsUpdateV2(source, nil)
	if err != nil {
		t.Fatal(err)
	}
	replica := newDoc("delta-cache", false, defaultGCFilter, nil, false, WithClientID(2))
	_ = ApplyUpdateV2(replica, initial, nil)
	replicaText := replica.GetText("t")
	_ = replicaText.ToDelta(nil, nil, nil)
	_ = replicaText.ToDelta(nil, nil, nil)
	if replicaText.deltaCache.Load() == nil {
		t.Fatal("remote ToDelta did not populate its cache")
	}

	text.Insert(4, "!", Object{})
	next, err := EncodeStateAsUpdateV2(source, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = ApplyUpdateV2(replica, next, nil)
	if replicaText.deltaCache.Load() != nil {
		t.Fatal("remote apply did not invalidate the delta cache")
	}
	if got := replicaText.ToDelta(nil, nil, nil); len(got) != 1 || got[0].InsertValue() != "abcd!" {
		t.Fatalf("remote mutation returned stale delta: %+v", got)
	}
}

func TestYTextDeltaCacheBypassesSnapshots(t *testing.T) {
	doc := newDoc("delta-cache", false, defaultGCFilter, nil, false, WithClientID(1))
	text := doc.GetText("t")
	text.Insert(0, "abc", Object{})
	_ = text.ToDelta(nil, nil, nil)
	_ = text.ToDelta(nil, nil, nil)
	cache := text.deltaCache.Load()
	if cache == nil || len(cache.ops) != 1 {
		t.Fatal("ToDelta did not populate its cache")
	}
	cache.ops[0].InsertText = "cache sentinel"
	snapshot := NewSnapshotByDoc(doc)
	got := text.ToDelta(snapshot, snapshot, nil)
	if len(got) != 1 || got[0].InsertValue() != "abc" {
		t.Fatalf("snapshot ToDelta reused the plain cache: %+v", got)
	}
}

// ---------------------------------------------------------------- from y_text_delta_parity_test.go
// MAX-gateway finding: YTextEvent.GetDelta's ContentFormat handling diverged from
// yjs 13.6.31 (src/types/YText.js `get delta()`): the non-deleted branch INVERTED
// the `value === null ? delete : set` rule and dropped the `attr !== null` /
// `value !== null` delete guards. The fuzz gate compares document state/encoding,
// not observer deltas, so these were uncaught. Expected deltas below are captured
// from yjs itself (fuzz/node_modules/yjs). Teeth: with the pre-fix GetDelta these
// assertions fail (wrong attribute values / dropped attrs).

func deltaString(ops []EventOperator) string {
	parts := make([]string, 0, len(ops))
	for _, op := range ops {
		var b strings.Builder
		switch op.Kind {
		case EventOperatorInsertText, EventOperatorInsertValue:
			fmt.Fprintf(&b, "insert(%v)", op.InsertValue())
		case EventOperatorRetain:
			fmt.Fprintf(&b, "retain(%d)", op.Length)
		case EventOperatorDelete:
			fmt.Fprintf(&b, "delete(%d)", op.Length)
		}
		if op.HasAttributes() {
			keys := op.Attributes.Keys()
			sort.Strings(keys)
			kv := make([]string, 0, len(keys))
			for _, k := range keys {
				v, _ := op.Attributes.Get(k)
				if isNull(v) {
					kv = append(kv, k+"=null")
				} else {
					kv = append(kv, fmt.Sprintf("%s=%v", k, v))
				}
			}
			b.WriteString("{" + strings.Join(kv, ",") + "}")
		}
		parts = append(parts, b.String())
	}
	return strings.Join(parts, ",")
}

// captureFormatDelta sets up a pre-formatted text, then runs op inside an observed
// transaction and returns the emitted YTextEvent delta as a normalized string.
func captureFormatDelta(t *testing.T, setup func(*YText), op func(*YText)) string {
	t.Helper()
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	txt := doc.GetText("t")
	setup(txt)

	var got string
	captured := false
	txt.Observe(func(ev interface{}, _ interface{}) {
		te, ok := ev.(*YTextEvent)
		if !ok {
			t.Fatalf("event is %T, want *YTextEvent", ev)
		}
		got = deltaString(te.GetDelta())
		captured = true
	})
	op(txt)
	if !captured {
		t.Fatal("no YTextEvent emitted")
	}
	return got
}

func boldAttr(v any) Object { o := newObject(); o.Set("bold", v); return o }

func liveFormatMarkers(tx *YText) int {
	n := 0
	for it := tx.startItem(); it != nil; it = it.right {
		if _, ok := it.content.(*contentFormat); ok && !it.isDeleted() {
			n++
		}
	}
	return n
}

// MAX-gateway native-op finding: insertAttributes/minimizeAttributeChanges compared
// currentAttributes via GetOr (Go nil when absent), but yjs reads `get(key) ?? null`.
// equalAttrs(nil, Null) is false where yjs equalAttrs(null, null) is true, so
// formatting a key to null on text that lacks it inserted a redundant ContentFormat
// marker yjs never creates — corrupting the item chain (and the encoded update).
// The fuzz gate missed this because it only replays yjs updates, never Go-native
// format(). Teeth: pre-fix this leaves 1+ live color markers; yjs leaves 0.
func TestFormatNullOnUnformattedNoMarker(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	tx := doc.GetText("t")
	tx.Insert(0, "ab", Object{})
	o := newObject()
	o.Set("color", Null)
	tx.Format(0, 2, o) // clear color on text that never had color
	if m := liveFormatMarkers(tx); m != 0 {
		t.Errorf("format(color:null) on uncolored text created %d redundant markers, want 0 (yjs)", m)
	}
	if got := tx.ToString(); got != "ab" {
		t.Errorf("text = %q, want %q", got, "ab")
	}
}

// MAX-gateway native-op finding: findMarker ignored the disabled (nil) search-marker
// state that ContentFormat.Integrate sets, because GetSearchMarker returns a never-nil
// pointer. So a formatted YText kept resolving positions via a search marker that
// skipped the opening formats → empty CurrentAttributes → an unattributed insert did
// NOT inherit the surrounding run. yjs disables markers once formatting exists, so it
// walks from start (accurate attributes). Verified vs yjs: inserting "d" after "e"
// inside a bold+italic run yields {insert:"ed", bold+italic}. Teeth: pre-fix "d" lands
// outside the run (no inherit).
func TestYTextUnattributedInsertInheritsInFormattedRun(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	tx := doc.GetText("t")
	col := newObject()
	col.Set("color", Null)
	tx.Insert(0, "e", col)
	bi := newObject()
	bi.Set("bold", true)
	bi.Set("italic", true)
	tx.Format(0, 1, bi)
	tx.Insert(1, "d", Object{}) // unattributed: must inherit bold+italic

	ops := tx.ToDelta(nil, nil, nil)
	if len(ops) != 1 || ops[0].InsertValue() != "ed" {
		t.Fatalf("toDelta = %+v, want one insert \"ed\" (yjs)", ops)
	}
	if ops[0].Attributes.GetOr("bold") != true || ops[0].Attributes.GetOr("italic") != true {
		t.Errorf("inserted \"d\" did not inherit bold+italic: attrs=%v", ops[0].Attributes.ToMap())
	}
}

// MAX-gateway native-op finding: deleting content must NOT delete the closing
// (negated) format markers that still bound the surviving run. The old value-based
// cleanupFormattingGap over-deleted them; the faithful yjs port (endFormats by
// reference) keeps them. Verified vs yjs: after insert "e"; insert "b"{bold,italic}@0;
// delete "e", the chain keeps all 4 format markers (bold/italic open + null close).
// Teeth: the old algorithm leaves only 2 (the closes deleted).
func TestYTextDeleteKeepsClosingFormatMarkers(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	tx := doc.GetText("t")
	tx.Insert(0, "e", Object{})
	bi := newObject()
	bi.Set("bold", true)
	bi.Set("italic", true)
	tx.Insert(0, "b", bi)
	tx.Delete(1, 1) // delete "e"

	if m := liveFormatMarkers(tx); m != 4 {
		t.Errorf("live format markers after delete = %d, want 4 (yjs keeps the closing null markers)", m)
	}
	// "b" must still be bold+italic.
	ops := tx.ToDelta(nil, nil, nil)
	if len(ops) != 1 || ops[0].InsertValue() != "b" || ops[0].Attributes.GetOr("bold") != true || ops[0].Attributes.GetOr("italic") != true {
		t.Errorf("toDelta = %+v, want one insert \"b\" with bold+italic (yjs)", ops)
	}
}

// MAX-gateway/inner finding: deleteText must pass the LIVE currPos.CurrentAttributes
// to cleanupFormattingGap (yjs deleteText), not a throwaway clone — ApplyDelta shares
// one currPos across ops, so a delete op's cleanup mutations to currAttributes must
// persist for later ops. Verified vs yjs (seed 188 of the ApplyDelta differential):
// base "abcde" + format(2,1,{italic:true}), then applyDelta [retain{bold:true},
// delete 2, retain{bold:true}, retain, insert "Z"{bold:null}] yields
// [{a,bold},{d,bold},{eZ}]. Teeth: the throwaway clone diverges the encoded state.
func TestApplyDeltaDeleteCurrAttributesPersist(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	tx := doc.GetText("t")
	tx.Insert(0, "abcde", Object{})
	it := newObject()
	it.Set("italic", true)
	tx.Format(2, 1, it)

	bt := func() Object { o := newObject(); o.Set("bold", true); return o }
	ret := func(n int, attr Object) EventOperator {
		return NewRetainDeltaOp(n, attr)
	}
	bn := newObject()
	bn.Set("bold", Null)
	tx.ApplyDelta([]EventOperator{
		ret(1, bt()),
		NewDeleteDeltaOp(2),
		ret(1, bt()),
		ret(1, Object{}),
		NewTextDeltaOp("Z", bn),
	}, true)

	if got := tx.ToString(); got != "adeZ" {
		t.Fatalf("toString = %q, want \"adeZ\"", got)
	}
	// Teeth is at the ENCODED-STATE level: the throwaway-clone bug leaves a different
	// format/tombstone structure. Compare byte-exact to yjs (encodeStateAsUpdate).
	const yjsState = "010c01000401017401618401000162840101016384010201648401030165c601010102066974616c69630474727565c601020103066974616c6963046e756c6c46010004626f6c640474727565c60100010104626f6c64046e756c6cc60106010304626f6c640474727565c60103010404626f6c64046e756c6c840104015a01010201020502"
	got, err := EncodeStateAsUpdate(doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if h := hex.EncodeToString(got); h != yjsState {
		t.Errorf("encoded state mismatch with yjs:\n got %s\nwant %s", h, yjsState)
	}
}

// Inner-loop finding: ApplyDelta's sanitize=false trailing-newline strip was doubly
// dead — it asserted op.Insert.(*yString) (inserts are plain strings) and checked
// `currPos == nil` (yjs checks currPos.right === null). yjs drops a trailing '\n' on
// the LAST string op at end-of-doc when sanitize=false. Teeth: pre-fix the '\n' stays.
func TestApplyDeltaSanitizeFalseStripsTrailingNewline(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	tx := doc.GetText("t")
	tx.ApplyDelta([]EventOperator{NewTextDeltaOp("x\n", Object{})}, false)
	if got := tx.ToString(); got != "x" {
		t.Errorf("sanitize=false ApplyDelta(\"x\\n\") = %q, want \"x\" (yjs strips trailing newline)", got)
	}
}

// Inner-loop finding: InsertEmbed/Delete/Format guarded on `if y != nil` (the
// always-non-nil receiver) instead of `if doc != nil`, so on a detached (pre-doc)
// YText they entered Transact(nil) (panic) instead of queueing to Pending like yjs.
// Teeth: pre-fix these panic.
func TestYTextDetachedOpsQueueWithoutPanic(t *testing.T) {
	tx := NewYText("") // detached: Doc == nil
	bold := newObject()
	bold.Set("bold", true)
	// None of these may panic; each must queue a pending op (yjs _pending).
	tx.InsertEmbed(0, func() Object { o := newObject(); o.Set("e", 1); return o }(), Object{})
	tx.Format(0, 1, bold)
	tx.Delete(0, 1)
	if len(tx.pending) != 3 {
		t.Errorf("detached InsertEmbed/Format/Delete queued %d pending ops, want 3", len(tx.pending))
	}
}

// Inner-loop regression guard: the per-parent remote-cleanup decision (CallObserver)
// uses isObservedText, which must bridge the YText/YXmlText embedding — a YXmlText's
// items reference *YXmlText while CallObserver's receiver is the embedded *YText. With
// a plain `item.Parent == y` the cleanup is a no-op for YXmlText, so it accumulates
// redundant markers a plain YText prunes. Verified vs yjs: after a concurrent
// identical bold format is merged remotely, both YText and YXmlText converge to 3 live
// markers. Teeth: with `item.Parent == y` the YXmlText keeps 4.
func TestRemoteConcurrentFormatXmlTextCleanup(t *testing.T) {
	mk := func(cid Number) (*Doc, *YText, *YXmlText) {
		d := newDoc("g", false, nil, nil, false, WithClientID(cid))
		txt := d.GetText("T")
		frag := d.GetXmlFragment("F")
		xt := NewYXmlText()
		frag.Insert(0, ArrayAny{xt})
		return d, txt, xt
	}
	bold := func() Object { o := newObject(); o.Set("bold", true); return o }

	d1, t1, x1 := mk(1)
	t1.Insert(0, "ab", Object{})
	x1.Insert(0, "ab", Object{})
	d2, _, _ := mk(2)
	base, _ := EncodeStateAsUpdate(d1, nil)
	_ = ApplyUpdate(d2, base, "remote")
	t2 := d2.GetText("T")
	x2 := d2.GetXmlFragment("F").Get(0).(*YXmlText)

	// concurrent identical bold format on both peers
	t1.Format(0, 2, bold())
	x1.Format(0, 2, bold())
	t2.Format(0, 2, bold())
	x2.Format(0, 2, bold())

	// exchange (remote apply triggers cleanup)
	u1, _ := EncodeStateAsUpdate(d1, nil)
	u2, _ := EncodeStateAsUpdate(d2, nil)
	_ = ApplyUpdate(d1, u2, "remote")
	_ = ApplyUpdate(d2, u1, "remote")

	for _, c := range []struct {
		name string
		txt  *YText
		xml  *YXmlText
	}{{"d1", t1, x1}, {"d2", t2, x2}} {
		mt := liveFormatMarkers(c.txt)
		mx := liveFormatMarkers(&c.xml.YText)
		if mt != mx {
			t.Errorf("%s: YXmlText cleanup diverged from YText (markers xml=%d text=%d)", c.name, mx, mt)
		}
	}
}

func TestYTextDeltaFormatParity(t *testing.T) {
	// S1: format(1,2,{bold:false}) over a bold:true run.
	// yjs: [{"retain":1},{"retain":2,"attributes":{"bold":false}}]
	got := captureFormatDelta(t,
		func(tx *YText) { tx.Insert(0, "abcd", Object{}); tx.Format(0, 4, boldAttr(true)) },
		func(tx *YText) { tx.Format(1, 2, boldAttr(false)) },
	)
	if want := "retain(1),retain(2){bold=false}"; got != want {
		t.Errorf("S1 delta = %q, want %q (yjs)", got, want)
	}

	// S2: format(1,2,{bold:null}) over a bold:true run (clear bold on sub-range).
	// yjs: [{"retain":1},{"retain":2,"attributes":{"bold":null}}]
	got = captureFormatDelta(t,
		func(tx *YText) { tx.Insert(0, "abcd", Object{}); tx.Format(0, 4, boldAttr(true)) },
		func(tx *YText) { tx.Format(1, 2, boldAttr(Null)) },
	)
	if want := "retain(1),retain(2){bold=null}"; got != want {
		t.Errorf("S2 delta = %q, want %q (yjs)", got, want)
	}

	// E1: extend bold backward — pre-existing bold on [1,2), format(0,1,{bold:true}).
	// yjs: [{"retain":2,"attributes":{"bold":true}}]
	got = captureFormatDelta(t,
		func(tx *YText) { tx.Insert(0, "ab", Object{}); tx.Format(1, 1, boldAttr(true)) },
		func(tx *YText) { tx.Format(0, 1, boldAttr(true)) },
	)
	if want := "retain(2){bold=true}"; got != want {
		t.Errorf("E1 delta = %q, want %q (yjs)", got, want)
	}

	// E4: format whole run to a value already partly present — pre-existing bold on
	// [0,2), format(0,4,{bold:true}). yjs: [{"retain":2},{"retain":2,"attributes":{"bold":true}}]
	got = captureFormatDelta(t,
		func(tx *YText) { tx.Insert(0, "abcd", Object{}); tx.Format(0, 2, boldAttr(true)) },
		func(tx *YText) { tx.Format(0, 4, boldAttr(true)) },
	)
	if want := "retain(2),retain(2){bold=true}"; got != want {
		t.Errorf("E4 delta = %q, want %q (yjs)", got, want)
	}

	// D1: add bold:false before a pre-existing bold:true run (negated format clears
	// the accumulated attr between them). yjs: [{"retain":1,"attributes":{"bold":false}}]
	got = captureFormatDelta(t,
		func(tx *YText) { tx.Insert(0, "abc", Object{}); tx.Format(2, 1, boldAttr(true)) },
		func(tx *YText) { tx.Format(0, 1, boldAttr(false)) },
	)
	if want := "retain(1){bold=false}"; got != want {
		t.Errorf("D1 delta = %q, want %q (yjs)", got, want)
	}
}

// ---------------------------------------------------------------- from y_text_mutation_scratch_test.go
func TestYTextMutationScratchResetsWithoutLeakingAttributes(t *testing.T) {
	doc := newDoc("text-mutation-scratch", false, defaultGCFilter, nil, false, WithClientID(1))
	text := doc.GetText("t")
	rich := MakeObject("bold", true, "italic", true, "color", "red")
	text.Insert(0, "ab", rich)

	// Exercise repeated scratch reuse inside one public transaction. The first insert inherits a
	// three-key (promoted Object) run; the second sits before all leading formats and must not retain
	// any state from that prior position walk.
	doc.Transact(func(*Transaction) {
		text.Insert(1, "I", Object{})
		text.Insert(0, "P", Object{})
	}, nil)

	delta := text.ToDelta(nil, nil, nil)
	if len(delta) != 2 || delta[0].InsertValue() != "P" || !delta[0].Attributes.IsNil() ||
		delta[1].InsertValue() != "aIb" {
		t.Fatalf("scratch reuse delta = %s, want plain P then rich aIb", deltaString(delta))
	}
	for _, key := range []string{"bold", "italic", "color"} {
		if _, ok := delta[1].Attributes.Get(key); !ok {
			t.Fatalf("inherited run lost %q after scratch reuse: %s", key, deltaString(delta))
		}
	}

	// A later Format call reuses the same Doc scratch. It must not mutate either already-integrated
	// formatting or the caller-owned Object supplied to the earlier insert.
	underline := MakeObject("underline", true)
	text.Format(2, 1, underline)
	if rich.Len() != 3 || rich.GetOr("bold") != true || rich.GetOr("italic") != true ||
		rich.GetOr("color") != "red" {
		t.Fatalf("caller attributes changed through scratch reuse: %v", rich.ToMap())
	}
	if got := text.ToString(); got != "PaIb" {
		t.Fatalf("text after scratch reuse = %q, want PaIb", got)
	}
}

// ---------------------------------------------------------------- from y_text_phase1_test.go
// Regression (TEETH-PROVEN by A/B against the reverted fix) for the second-pass
// review finding: the formatting-cleanup boundary walks (cleanupFormattingGap /
// cleanupContextlessFormattingGap) used a ContentString/ContentEmbed allow-list
// instead of yjs's `!countable`, so they skipped PAST a nested ContentType when
// finding the next content boundary. This path only runs on a REMOTE change
// (CallObserver, `if !trans.Local`). doc1 builds "[A bold][map bold][B italic]" and
// syncs to doc2; a remote formatting change then triggers doc2's cleanupYTextFormatting
// over the chain containing the map. B must keep its italic — pre-fix the end-walk
// skipped the map and deleted B's italic format marker (B lost italic; verified).
func TestTextRemoteCleanupPreservesFormattingAroundNestedType(t *testing.T) {
	d1 := newDoc("g", false, nil, nil, false, WithClientID(1))
	x1 := d1.GetText("t")
	inner := NewYMap(map[string]interface{}{"k": "v"})
	bold := newObject()
	bold.Set("bold", true)
	italic := newObject()
	italic.Set("italic", true)
	x1.ApplyDelta([]EventOperator{
		NewTextDeltaOp("A", bold),
		NewValueDeltaOp(inner, bold),
		NewTextDeltaOp("B", italic),
	}, false)

	d2 := newDoc("g", false, nil, nil, false, WithClientID(2))
	u1, err := EncodeStateAsUpdate(d1, nil)
	if err != nil {
		t.Fatalf("EncodeStateAsUpdate d1: %v", err)
	}
	_ = ApplyUpdate(d2, u1, "remote")
	x2 := d2.GetText("t")

	// A remote formatting change (inserts a ContentFormat) makes doc2's CallObserver
	// run cleanupYTextFormatting across the run containing the nested map.
	x1.Format(0, 3, bold)
	u2, err := EncodeStateAsUpdate(d1, nil)
	if err != nil {
		t.Fatalf("EncodeStateAsUpdate d1 (2): %v", err)
	}
	_ = ApplyUpdate(d2, u2, "remote")

	d := x2.ToDelta(nil, nil, nil)
	var bOp *EventOperator
	for i := range d {
		if s, _ := d[i].InsertValue().(string); s == "B" {
			bOp = &d[i]
		}
	}
	if bOp == nil {
		t.Fatalf("no \"B\" op in delta: %+v", d)
	}
	if bOp.Attributes.GetOr("italic") != true {
		t.Errorf("B lost its italic after the remote cleanup skipped past the nested type; attrs=%+v", bOp.Attributes)
	}
}

// A nested collaborative type embedded in a YText (a *ContentType, created when
// ApplyDelta inserts an IAbstractType) must round-trip through the wire and ToDelta
// with its content and order intact — the feature US6 enabled. (Note: the index
// position helpers Forward/findNextPosition were also aligned to count ContentType
// like yjs's forward() `default`; that omission proved behaviorally compensated by the
// deleteText/formatText main loops for the operations tested here, but the helpers are
// kept consistent so every YText traversal treats a countable nested type uniformly.)
func TestTextNestedTypeRoundTrips(t *testing.T) {
	d1 := newDoc("g", false, nil, nil, false, WithClientID(1))
	x1 := d1.GetText("t")
	inner := NewYMap(map[string]interface{}{"k": "v"})
	x1.ApplyDelta([]EventOperator{
		NewTextDeltaOp("AB", Object{}),
		NewValueDeltaOp(inner, Object{}),
		NewTextDeltaOp("CD", Object{}),
	}, false)

	d2 := newDoc("g", false, nil, nil, false, WithClientID(2))
	u1, err := EncodeStateAsUpdate(d1, nil)
	if err != nil {
		t.Fatalf("EncodeStateAsUpdate: %v", err)
	}
	_ = ApplyUpdate(d2, u1, "remote")

	d := d2.GetText("t").ToDelta(nil, nil, nil)
	if len(d) != 3 {
		t.Fatalf("expected 3 ops (AB, <map>, CD), got %d: %+v", len(d), d)
	}
	if s, _ := d[0].InsertValue().(string); s != "AB" {
		t.Errorf("op0 = %v, want AB", d[0].InsertValue())
	}
	m, ok := d[1].InsertValue().(*YMap)
	if !ok {
		t.Fatalf("op1 = %T, want *YMap (nested type survived the wire)", d[1].InsertValue())
	}
	if m.Get("k") != "v" {
		t.Errorf("nested map lost its content: k=%v", m.Get("k"))
	}
	if s, _ := d[2].InsertValue().(string); s != "CD" {
		t.Errorf("op2 = %v, want CD", d[2].InsertValue())
	}
}

// US6 / FR-024 (work item 1.3A). An unattributed insert inside a formatted run must
// reset (negate) the surrounding formatting on the inserted content, matching yjs
// (src/types/YText.js insertText: the currentAttributes negation pre-pass). The
// pre-pass was deleted with a "golang no need" comment; without it, text inserted
// into a bold run inherits bold (formatting bleed), and the bug is reachable via
// ApplyDelta/InsertEmbed which bypass the top-level Insert masking.

// US6 / FR-026 (work item 1.3B). A nested collaborative type embedded in a YText
// must be stored as a ContentType and surfaced as an insert op in deltas
// (GetDelta) and full-delta export (ToDelta), matching yjs (the combined
// `case ContentType: case ContentEmbed:` branches using getContent()[0]).
func TestTextNestedTypeSurfacedInDelta(t *testing.T) {
	doc := newDoc("guid", false, nil, nil, false, WithClientID(1))
	ytext := doc.GetText("t")
	ytext.Insert(0, "ab", Object{})

	var observed []EventOperator
	ytext.Observe(func(e interface{}, _ interface{}) {
		observed = e.(*YTextEvent).GetDelta()
	})

	nested := NewYMap(nil)
	Transact(doc, func(trans *Transaction) {
		pos := findPosition(trans, ytext, 1, false)
		insertText(trans, ytext, pos, nested, Object{})
	}, nil, true)

	// the inserted content must be a *ContentType (a real nested type), not a ContentEmbed
	foundContentType := false
	for it := ytext.startItem(); it != nil; it = it.right {
		if _, ok := it.content.(*contentType); ok {
			foundContentType = true
		}
	}
	if !foundContentType {
		t.Fatal("inserting an AbstractType into a YText did not create a *ContentType")
	}

	hasNestedInsert := func(delta []EventOperator) bool {
		for _, op := range delta {
			if m, ok := op.InsertValue().(*YMap); ok && m == nested {
				return true
			}
		}
		return false
	}

	if !hasNestedInsert(ytext.ToDelta(nil, nil, nil)) {
		t.Error("ToDelta missing the nested-type insert op (no *ContentType case)")
	}
	if !hasNestedInsert(observed) {
		t.Errorf("observed event delta missing the nested-type insert op; delta=%+v", observed)
	}
}

// US6 / FR-025 (work item 1.3B). Emitted text-event deltas must never contain a
// no-op operation (empty insert, zero delete, or zero retain) — the addOp guards.
func TestTextDeltaNoZeroOps(t *testing.T) {
	doc := newDoc("guid", false, nil, nil, false, WithClientID(1))
	ytext := doc.GetText("t")
	bold := newObject()
	bold.Set("bold", true)
	ytext.Insert(0, "Hello", bold)

	var observed []EventOperator
	ytext.Observe(func(e interface{}, _ interface{}) {
		observed = append(observed, e.(*YTextEvent).GetDelta()...)
	})

	// a mix of retain + unattributed insert + format toggles inside a formatted run
	ytext.ApplyDelta([]EventOperator{
		NewRetainDeltaOp(2, Object{}),
		NewTextDeltaOp("X", Object{}),
	}, false)
	ytext.Format(0, 5, bold) // re-apply an already-present format (redundant)
	ytext.Delete(1, 1)

	for _, op := range observed {
		if op.IsInsert() {
			if s, ok := op.InsertValue().(string); ok && s == "" {
				t.Errorf("delta contains an empty-insert no-op: %+v", op)
			}
		}
		if op.Kind == EventOperatorRetain && op.Length == 0 {
			t.Errorf("delta contains a zero-retain no-op: %+v", op)
		}
		if op.Kind == EventOperatorDelete && op.Length == 0 {
			t.Errorf("delta contains a zero-delete no-op: %+v", op)
		}
	}
}

// US6 / FR-025: an observed insert that carries active formatting must surface its
// attributes in the event delta (covers the addOp insert-with-currentAttributes path).
func TestTextDeltaInsertCarriesAttributes(t *testing.T) {
	doc := newDoc("guid", false, nil, nil, false, WithClientID(1))
	ytext := doc.GetText("t")
	var observed []EventOperator
	ytext.Observe(func(e interface{}, _ interface{}) {
		observed = e.(*YTextEvent).GetDelta()
	})
	bold := newObject()
	bold.Set("bold", true)
	ytext.Insert(0, "Hi", bold)

	found := false
	for _, op := range observed {
		if s, ok := op.InsertValue().(string); ok && s == "Hi" {
			if op.HasAttributes() && op.Attributes.Has("bold") {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("observed bold insert did not carry the bold attribute; delta=%+v", observed)
	}
}

func TestInsertTextNegationNoFormattingBleed(t *testing.T) {
	doc := newDoc("guid", false, nil, nil, false, WithClientID(1))
	ytext := doc.GetText("t")
	ytext.Insert(0, "ab", Object{})

	bold := newObject()
	bold.Set("bold", true)
	ytext.Format(0, 2, bold) // "ab" -> both bold

	// Insert an unattributed "X" inside the bold run via a delta (retain 1, insert
	// "X" with NO attributes). It must NOT inherit bold.
	ytext.ApplyDelta([]EventOperator{
		NewRetainDeltaOp(1, Object{}),
		NewTextDeltaOp("X", Object{}),
	}, false)

	delta := ytext.ToDelta(nil, nil, nil)

	var xOp *EventOperator
	for i := range delta {
		if s, ok := delta[i].InsertValue().(string); ok && s == "X" {
			xOp = &delta[i]
		}
	}
	if xOp == nil {
		t.Fatalf("no standalone op inserting \"X\" — it merged into the bold run (formatting bleed). delta=%+v", delta)
	}
	if xOp.Attributes.Has("bold") {
		t.Errorf("inserted \"X\" inherited bold (formatting bleed); want unformatted. delta=%+v", delta)
	}
}

// ---------------------------------------------------------------- from y_text_string_cache_test.go
func TestYTextStringCacheInvalidatesOnLocalAndRemoteChanges(t *testing.T) {
	source := newDoc("text-cache", false, defaultGCFilter, nil, false, WithClientID(1))
	text := source.GetText("t")
	text.Insert(0, "abcdef", newObject())
	bold := newObject()
	bold.Set("bold", true)
	text.Format(1, 3, bold) // fragment the item list so ToString uses the cache
	if got := text.ToString(); got != "abcdef" {
		t.Fatalf("initial ToString = %q", got)
	}
	if text.stringCache.Load() == nil {
		t.Fatal("fragmented text did not populate its string cache")
	}
	for item := text.start; item != nil; item = item.right {
		if content, ok := item.content.(*contentString); ok && !item.isDeleted() && len(content.value) > 0 {
			content.value = "Z" + content.value[1:] // direct same-length exported-field replacement
			if got := text.ToString(); got[0] != 'Z' {
				t.Fatalf("direct ContentString.Str replacement returned stale cache: %q", got)
			}
			content.value = "a" + content.value[1:]
			break
		}
	}

	text.Delete(2, 1)
	text.Insert(2, "Z", newObject()) // same visible length, different content
	if got := text.ToString(); got != "abZdef" {
		t.Fatalf("local same-length replacement returned stale cache: %q", got)
	}

	initial, err := EncodeStateAsUpdateV2(source, nil)
	if err != nil {
		t.Fatal(err)
	}
	replica := newDoc("text-cache", false, defaultGCFilter, nil, false, WithClientID(2))
	_ = ApplyUpdateV2(replica, initial, nil)
	replicaText := replica.GetText("t")
	if got := replicaText.ToString(); got != "abZdef" {
		t.Fatalf("replica initial ToString = %q", got)
	}
	if replicaText.stringCache.Load() == nil {
		t.Fatal("fragmented replica did not populate its string cache")
	}

	text.Insert(text.Length(), "!", newObject())
	next, err := EncodeStateAsUpdateV2(source, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = ApplyUpdateV2(replica, next, nil)
	if got := replicaText.ToString(); got != "abZdef!" {
		t.Fatalf("remote apply returned stale cache: %q", got)
	}
}

func TestYXmlTextStringCacheInvalidates(t *testing.T) {
	doc := newDoc("xml-text-cache", false, defaultGCFilter, nil, false, WithClientID(1))
	fragment := doc.GetXmlFragment("f")
	text := NewYXmlText()
	fragment.Insert(0, ArrayAny{text})
	text.Insert(0, "abcd", newObject())
	attrs := newObject()
	attrs.Set("italic", true)
	text.Format(1, 2, attrs)
	if got := text.YText.ToString(); got != "abcd" {
		t.Fatalf("initial XML text = %q", got)
	}
	if text.stringCache.Load() == nil {
		t.Fatal("fragmented XML text did not populate its string cache")
	}
	text.Delete(1, 1)
	if got := text.YText.ToString(); got != "acd" {
		t.Fatalf("mutated XML text returned stale cache: %q", got)
	}
}

// ---------------------------------------------------------------- from y_text_to_delta_ychange_test.go
func TestToDeltaClearsDefinedYChangeOnOrdinaryText(t *testing.T) {
	doc := newDoc("delta-ychange", false, defaultGCFilter, nil, false, WithClientID(1))
	text := doc.GetText("t")
	attrs := newObject()
	attrs.Set("ychange", "user-value")
	text.Insert(0, "x", attrs)

	delta := text.ToDelta(nil, nil, nil)
	if len(delta) != 1 || delta[0].InsertValue() != "x" || delta[0].HasAttributes() {
		t.Fatalf("ordinary delta retained the internal ychange attribute: %#v", delta)
	}
}
