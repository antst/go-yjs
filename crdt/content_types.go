package crdt

import (
	"errors"
	"fmt"
	"slices"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"
)

// ---------------------------------------------------------------- from content_any.go
type contentAny struct {
	arr ArrayAny
}

func (c *contentAny) contentLength() Number {
	return len(c.arr)
}

func (c *contentAny) contentValues() ArrayAny {
	return c.arr
}

func (c *contentAny) isCountable() bool {
	return true
}

func (c *contentAny) copyContent() itemContent {
	// yjs contentAny.copy preserves the array reference. Nested any/object values must retain
	// reference identity across redo; deep-copying here is both observably wrong and expensive.
	//
	// The capacity bound is what makes sharing the array safe. yjs grows a content array with
	// concat, which always allocates, so no yjs contentAny can ever be written through another's
	// reference. This port grows it with append, which writes in place whenever spare capacity
	// exists — and ReadContentAny and the tail fast path both leave spare capacity. Bounding the
	// shared view forces append to reallocate, which is concat's guarantee restored structurally.
	return newContentAny(c.arr[:len(c.arr):len(c.arr)])
}

func (c *contentAny) spliceContent(offset Number) itemContent {
	// JavaScript Array.slice creates independent containers while preserving element
	// references. spliceArrayAny gives the same independence without copying either
	// half in the common case; see its comment for why that is safe here.
	left, right := spliceArrayAny(c.arr, offset)
	c.arr = left
	return newContentAny(right)
}

func (c *contentAny) mergeContentWith(right itemContent) bool {
	r, ok := right.(*contentAny)
	if !ok {
		return false
	}

	c.arr = append(c.arr, r.arr...)
	return true
}

func (c *contentAny) integrateContent(trans *Transaction, item *itemStruct) {

}

func (c *contentAny) deleteContent(trans *Transaction) {

}

func (c *contentAny) gcContent(store *structStore) {

}

func (c *contentAny) writeContent(encoder updateEncoder, offset Number) error {
	length := len(c.arr)
	if offset > length {
		return errors.New("offset is larger than length")
	}

	encoder.writeLength(length - offset)
	for i := offset; i < length; i++ {
		c := c.arr[i]
		if err := encoder.writeAnyValue(c); err != nil {
			return err
		}
	}

	return nil
}

func (c *contentAny) contentRef() uint8 {
	return refContentAny
}

func newContentAny(arr ArrayAny) *contentAny {
	return &contentAny{arr: arr}
}

func readContentAny(decoder updateDecoder) (itemContent, error) {
	length, err := decoder.readLength()
	if err != nil {
		return nil, err
	}

	var cs ArrayAny
	for i := 0; i < length; i++ {
		c, err := decoder.readAnyValue()
		if err != nil {
			return nil, err
		}

		cs = append(cs, c)
	}

	return newContentAny(cs), nil
}

// ---------------------------------------------------------------- from content_binary.go
type contentBinary struct {
	value []uint8
}

func (c *contentBinary) contentLength() Number {
	return 1
}

func (c *contentBinary) contentValues() ArrayAny {
	return ArrayAny{c.value}
}

func (c *contentBinary) isCountable() bool {
	return true
}

func (c *contentBinary) copyContent() itemContent {
	content := make([]uint8, 0, len(c.value))
	content = append(content, c.value...)

	return newContentBinary(content)
}

func (c *contentBinary) spliceContent(offset Number) itemContent {
	return nil
}

func (c *contentBinary) mergeContentWith(right itemContent) bool {
	return false
}

func (c *contentBinary) integrateContent(trans *Transaction, item *itemStruct) {

}

func (c *contentBinary) deleteContent(trans *Transaction) {

}

func (c *contentBinary) gcContent(store *structStore) {

}

func (c *contentBinary) writeContent(encoder updateEncoder, offset Number) error {
	return encoder.writeBuffer(c.value)
}

func (c *contentBinary) contentRef() uint8 {
	return refContentBinary
}

func newContentBinary(content []uint8) *contentBinary {
	return &contentBinary{
		value: content,
	}
}

func readContentBinary(decoder updateDecoder) (itemContent, error) {
	content, err := decoder.readBuffer()
	if err != nil {
		return nil, err
	}

	return newContentBinary(content), nil
}

// ---------------------------------------------------------------- from content_deleted.go
type contentDeleted struct {
	length Number
}

func (c *contentDeleted) contentLength() Number {
	return c.length
}

func (c *contentDeleted) contentValues() ArrayAny {
	return nil
}

func (c *contentDeleted) isCountable() bool {
	return false
}

func (c *contentDeleted) copyContent() itemContent {
	return newContentDeleted(c.length)
}

func (c *contentDeleted) spliceContent(offset Number) itemContent {
	if offset > c.length {
		offset = c.length
	}

	right := newContentDeleted(c.length - offset)
	c.length = offset
	return right
}

func (c *contentDeleted) mergeContentWith(right itemContent) bool {
	r, ok := right.(*contentDeleted)
	if !ok {
		return false
	}

	c.length += r.length
	return true
}

func (c *contentDeleted) integrateContent(trans *Transaction, item *itemStruct) {
	trans.addToDeleteSet(item.id.Client, item.id.Clock, c.length)
	item.markDeleted()
}

func (c *contentDeleted) deleteContent(trans *Transaction) {

}

func (c *contentDeleted) gcContent(store *structStore) {

}

func (c *contentDeleted) writeContent(encoder updateEncoder, offset Number) error {
	if offset > c.length {
		return errors.New("offset is larger than length")
	}

	encoder.writeLength(c.length - offset)
	return nil
}

func (c *contentDeleted) contentRef() uint8 {
	return refContentDeleted
}

func newContentDeleted(length Number) *contentDeleted {
	return &contentDeleted{length: length}
}

func readContentDeleted(decoder updateDecoder) (itemContent, error) {
	length, err := decoder.readLength()
	if err != nil {
		return nil, err
	}

	return newContentDeleted(length), nil
}

// ---------------------------------------------------------------- from content_doc.go
const (
	optKeyGC       = "gc"
	optKeyAutoLoad = "autoLoad"
	optKeyMeta     = "meta"
)

type contentDoc struct {
	doc  *Doc
	opts Object
}

func (c *contentDoc) contentLength() Number {
	return 1
}

func (c *contentDoc) contentValues() ArrayAny {
	return ArrayAny{c.doc}
}

func (c *contentDoc) isCountable() bool {
	return true
}

func (c *contentDoc) copyContent() itemContent {
	// yjs ContentDoc.copy(): `new ContentDoc(createDocFromOpts(this.doc.guid, this.opts))`
	// — a FRESH doc built from guid+opts, NOT a deep copy of the live doc. It has no
	// content, no _item back-link, and no observers.
	//
	// The previous copystructure.Copy(c.Doc) approach was wrong: on an INTEGRATED
	// subdoc the live doc has a parent<->child reference cycle (Doc.Item -> parent
	// item -> ... ), which made copystructure hang (cycle) or return nil — so redo of
	// a subdoc insertion (RedoItem -> Content.Copy) crashed with a nil dereference.
	// Rebuild from the stored Opts (createDocFromOpts parity) via newSubdocFromOpts,
	// NOT from the live c.Doc.GC/Meta/AutoLoad fields: Opts is the immutable snapshot
	// captured at NewContentDoc, so a copy can't drift if those public Doc fields are
	// later mutated. (newSubdocFromOpts sets ShouldLoad = autoLoad, as before.)
	return newContentDoc(newSubdocFromOpts(c.doc.GUID, c.opts))
}

func (c *contentDoc) spliceContent(offset Number) itemContent {
	return nil
}

func (c *contentDoc) mergeContentWith(right itemContent) bool {
	return false
}

// integrateContent registers the nested document as a subdocument, matching yjs
// (src/structs/ContentDoc.js integrate): it back-links doc._item, adds the doc to
// transaction.subdocsAdded, and — when the doc should load — to subdocsLoaded.
func (c *contentDoc) integrateContent(trans *Transaction, item *itemStruct) {
	c.doc.item = item
	trans.addSubdocAdded(c.doc)
	if c.doc.ShouldLoad {
		trans.addSubdocLoaded(c.doc)
	}
}

// deleteContent withdraws a subdocument added in the same transaction, otherwise queues
// it for removal/destruction, matching yjs (src/structs/ContentDoc.js delete).
func (c *contentDoc) deleteContent(trans *Transaction) {
	if trans.subdocsAdded.Has(c.doc) {
		trans.subdocsAdded.Delete(c.doc)
	} else {
		trans.addSubdocRemoved(c.doc)
	}
}

// gcContent is intentionally a no-op: a subdocument is an independent document and its
// content must not be collected by the parent's gcContent (matches yjs ContentDoc.gc).
func (c *contentDoc) gcContent(store *structStore) {
}

func (c *contentDoc) writeContent(encoder updateEncoder, offset Number) error {
	err := encoder.writeStringValue(c.doc.GUID)
	if err != nil {
		return err
	}

	return encoder.writeAnyValue(c.opts)
}

func (c *contentDoc) contentRef() uint8 {
	return refContentDoc
}

func newContentDoc(doc *Doc) *contentDoc {
	c := &contentDoc{
		doc:  doc,
		opts: newObject(),
	}

	if !doc.GC {
		c.opts.Set(optKeyGC, false)
	}

	if doc.AutoLoad {
		c.opts.Set(optKeyAutoLoad, true)
	}

	if doc.Meta != nil {
		c.opts.Set(optKeyMeta, doc.Meta)
	}

	return c
}

// newSubdocFromOpts rebuilds a *Doc from a ContentDoc's opts payload, mirroring yjs
// createDocFromOpts(guid, opts) (ContentDoc.js): Opts stores only NON-default options
// (NewContentDoc writes optKeyGC only when gc is false, optKeyAutoLoad only when true),
// so an absent key defaults to gc=true / autoLoad=false. ShouldLoad is set to
// shouldLoad||autoLoad||false — the wire opts carry no shouldLoad, so autoLoad. Callers
// that need a different ShouldLoad (e.g. Doc.Destroy reconstruction → false) override it.
func newSubdocFromOpts(guid string, opts Object) *Doc {
	gc, ok := opts.GetOr(optKeyGC).(bool)
	if !ok {
		gc = true
	}
	autoLoad, ok := opts.GetOr(optKeyAutoLoad).(bool)
	if !ok {
		autoLoad = false
	}
	d := newDoc(guid, gc, defaultGCFilter, opts.GetOr(optKeyMeta), autoLoad)
	d.ShouldLoad = autoLoad
	return d
}

func readContentDoc(decoder updateDecoder) (itemContent, error) {
	guid, err := decoder.readStringValue()
	if err != nil {
		return nil, err
	}

	decoded, err := decoder.readAnyValue()
	if err != nil {
		return nil, err
	}

	// The opts payload must be an object<string,any>. On hostile/malformed input
	// ReadAny can yield any lib0 type (a string, number, array, ...); an unchecked
	// type assertion would panic and crash the process. Surface a decode error
	// instead so it propagates up ReadItemContent -> the apply/merge/convert path.
	opts, ok := decoded.(Object)
	if !ok {
		return nil, fmt.Errorf("read content doc: opts is %T, want Object", decoded)
	}

	// yjs createDocFromOpts: a decoded subdoc loads only when shouldLoad || autoLoad;
	// the wire opts carry no shouldLoad, so ShouldLoad = autoLoad (set by the helper).
	doc := newSubdocFromOpts(guid, opts)
	return newContentDoc(doc), nil
}

// ---------------------------------------------------------------- from content_embed.go
type contentEmbed struct {
	embed interface{}
}

func (c *contentEmbed) contentLength() Number {
	return 1
}

func (c *contentEmbed) contentValues() ArrayAny {
	return ArrayAny{c.embed}
}

func (c *contentEmbed) isCountable() bool {
	return true
}

func (c *contentEmbed) copyContent() itemContent {
	return newContentEmbed(c.embed)
}

func (c *contentEmbed) spliceContent(offset Number) itemContent {
	return nil
}

func (c *contentEmbed) mergeContentWith(right itemContent) bool {
	return false
}

func (c *contentEmbed) integrateContent(trans *Transaction, item *itemStruct) {

}

func (c *contentEmbed) deleteContent(trans *Transaction) {

}

func (c *contentEmbed) gcContent(store *structStore) {

}

func (c *contentEmbed) writeContent(encoder updateEncoder, offset Number) error {
	return encoder.writeJSONValue(c.embed)
}

func (c *contentEmbed) contentRef() uint8 {
	return refContentEmbed
}

func newContentEmbed(embed interface{}) *contentEmbed {
	return &contentEmbed{
		embed: embed,
	}
}

func readContentEmbed(decoder updateDecoder) (itemContent, error) {
	embed, err := decoder.readJSONValue()
	if err != nil {
		return nil, err
	}

	return newContentEmbed(embed), nil
}

// ---------------------------------------------------------------- from content_format.go
type contentFormat struct {
	key   string
	value interface{}
}

func (c *contentFormat) contentLength() Number {
	return 1
}

func (c *contentFormat) contentValues() ArrayAny {
	return nil
}

func (c *contentFormat) isCountable() bool {
	return false
}

func (c *contentFormat) copyContent() itemContent {
	return newContentFormat(c.key, c.value)
}

func (c *contentFormat) spliceContent(offset Number) itemContent {
	// 不支持
	return nil
}

func (c *contentFormat) mergeContentWith(right itemContent) bool {
	return false
}

func (c *contentFormat) integrateContent(trans *Transaction, item *itemStruct) {
	// A formatting mutation invalidates both the reference marker cache and the block index's live
	// format aggregates. Discard the derived index rather than maintaining it through format-dense
	// churn; a later inherited edit rebuilds once from the resulting stable formatting run.
	(item.parent).(abstractType).setSearchMarker(nil)
}

func (c *contentFormat) deleteContent(trans *Transaction) {

}

func (c *contentFormat) gcContent(store *structStore) {

}

func (c *contentFormat) writeContent(encoder updateEncoder, offset Number) error {
	if err := encoder.writeKey(c.key); err != nil {
		return err
	}
	// WriteJson routes through lib0 any-encoding under V2, which can now surface
	// real errors (e.g. nested object write failures); propagate instead of
	// silently emitting a truncated format marker.
	return encoder.writeJSONValue(c.value)
}

func (c *contentFormat) contentRef() uint8 {
	return refContentFormat
}

func newContentFormat(key string, value interface{}) *contentFormat {
	return &contentFormat{
		key:   key,
		value: value,
	}
}

func readContentFormat(decoder updateDecoder) (itemContent, error) {
	// Must mirror ContentFormat.Write, which emits the key via WriteKey. Under V2
	// WriteKey advances the keyClock column (and writes the string only on a cache
	// miss), so the key MUST be read back via ReadKey — using ReadString would
	// leave the keyClock column entry unconsumed and desync every subsequent
	// ReadKey in the same decoder. V1 ReadKey delegates to ReadString, so this is
	// byte-compatible with the V1 path. (Matches Yjs readContentFormat.)
	key, err := decoder.readKey()
	if err != nil {
		return nil, err
	}

	value, err := decoder.readJSONValue()
	if err != nil {
		return nil, err
	}

	return newContentFormat(key, value), nil
}

// ---------------------------------------------------------------- from content_json.go
type contentJSON struct {
	arr ArrayAny
}

func (c *contentJSON) contentLength() Number {
	return len(c.arr)
}

func (c *contentJSON) contentValues() ArrayAny {
	return c.arr
}

func (c *contentJSON) isCountable() bool {
	return true
}

func (c *contentJSON) copyContent() itemContent {
	// Shared array, capacity-bounded so append cannot write through it; see contentAny.Copy.
	return newContentJSON(c.arr[:len(c.arr):len(c.arr)])
}

func (c *contentJSON) spliceContent(offset Number) itemContent {
	left, right := spliceArrayAny(c.arr, offset)
	c.arr = left
	return newContentJSON(right)
}

func (c *contentJSON) mergeContentWith(right itemContent) bool {
	r, ok := right.(*contentJSON)
	if !ok {
		return false
	}

	c.arr = append(c.arr, r.arr...)
	return true
}

func (c *contentJSON) integrateContent(trans *Transaction, item *itemStruct) {

}

func (c *contentJSON) deleteContent(trans *Transaction) {

}

func (c *contentJSON) gcContent(store *structStore) {

}

func (c *contentJSON) writeContent(encoder updateEncoder, offset Number) error {
	length := len(c.arr)
	encoder.writeLength(length - offset)
	for i := offset; i < length; i++ {
		e := c.arr[i]

		if isUndefined(e) {
			if err := encoder.writeStringValue(keywordUndefined); err != nil {
				return err
			}
			continue
		}

		// lib0/Yjs serializes contentJSON values with JSON.stringify, which emits
		// object keys in JS insertion order. marshalJSONOrdered walks the ordered
		// Object type so multi-key objects match the JS byte stream (plain
		// json.Marshal would sort the keys); scalars and arrays are byte-identical
		// to the previous json.Marshal path.
		data, err := marshalJSONOrdered(e)
		if err != nil {
			return err
		}

		if err := encoder.writeStringValue(string(data)); err != nil {
			return err
		}
	}

	return nil
}

func (c *contentJSON) contentRef() uint8 {
	return refContentJSON
}

func newContentJSON(arr ArrayAny) *contentJSON {
	return &contentJSON{
		arr: arr,
	}
}

func readContentJSON(decoder updateDecoder) (itemContent, error) {
	length, err := decoder.readLength()
	if err != nil {
		return nil, err
	}

	var cs ArrayAny
	for i := 0; i < length; i++ {
		c, err := decoder.readStringValue()
		if err != nil {
			return nil, err
		}

		if c == keywordUndefined {
			cs = append(cs, Undefined)
		} else {
			// Parse into the ordered Object type so a multi-key object's on-wire key
			// order survives into re-encoding (byte-parity round-trip).
			obj, err := unmarshalJSONOrdered([]byte(c))
			if err != nil {
				return nil, err
			}

			cs = append(cs, obj)
		}
	}

	return newContentJSON(cs), nil
}

// ---------------------------------------------------------------- from content_slice_split.go
// spliceArrayAny splits arr at offset into two halves that can be appended to
// independently, without copying either one in the common case.
//
// EXACTLY ONE VIEW PER BACKING ARRAY MAY KEEP SPARE CAPACITY, AND IT IS THE
// RIGHTMOST. Both halves index one array, so an append through the left would
// write over the right half's first element — the left is therefore
// capacity-bounded, which forces its appends to reallocate. The right half needs
// no such bound: it can only grow into the region PAST the original length, and
// no other live view reaches there. Left halves are bounded here and copies are
// bounded in Copy, so the invariant holds by construction rather than by
// convention. The append sites it protects are contentAny/contentJSON.MergeWith
// and the tail fast path in typeListInsertGenericsAfter, both of which grow arr
// in place.
//
// BOUNDING THE RIGHT HALF TOO WAS MEASURABLY WRONG, which is why the asymmetry
// is spelled out rather than assumed. The right half is the tail, and the tail is
// what the append fast path grows. An earlier version bounded it as well; a
// memory profile of a push-interleaved-with-delete workload then showed 99.46% of
// all bytes in typeListInsertGenericsAfter, because every push after a delete
// reallocated the whole run. That merely moved the quadratic from the delete path
// to the push path, and made it slightly worse.
//
// WHY IT STILL COPIES SOMETIMES, AND WHY THE TWO THRESHOLDS DIFFER. Reslicing
// pins the whole backing array for as long as either half is reachable, so a run
// whose pieces are mostly deleted would keep the original alive behind a handful
// of live elements. A half is copied back out once it holds too little of what it
// pins. The left is bounded and can therefore never reuse the pinned capacity, so
// pinning is pure waste for it and the threshold is tight: copy out below a half.
// The right WILL reuse that capacity on its next append — it is precisely the
// capacity append would have allocated anyway — so it is only copied out below a
// quarter, which is append's own growth tolerance. Neither copy returns to the
// quadratic: they form a geometric series over the life of a run, so the split
// stays amortised O(1) per element.
func spliceArrayAny(arr ArrayAny, offset Number) (ArrayAny, ArrayAny) {
	length, pinned := len(arr), cap(arr)

	left, right := arr[:offset:offset], arr[offset:]
	if 2*offset < pinned {
		left = slices.Clone(left)
	}
	if 4*(length-offset) < pinned {
		right = slices.Clone(right)
	}

	return left, right
}

// ---------------------------------------------------------------- from content_string.go
type contentString struct {
	value string

	// utf16Index maps sparse UTF-16 positions to byte offsets in long,
	// non-ASCII strings. The index is immutable and shared by fragments that
	// still alias its source string, so splitting it never rebuilds the index.
	// Short strings keep this nil and pay no index allocation. This occupies the
	// pointer slot formerly used by the ASCII fingerprint, preserving the
	// 24-byte contentString layout after value became package-private.
	utf16Index *contentStringUTF16Index
}

const (
	// The measured ASCII/non-ASCII crossover begins between 32 and 64 runes,
	// but constructing an index at exactly 64 still costs more than it saves.
	// Activating at 128 keeps that boundary on the scan path and starts paying
	// retained memory at the first size with a measured net win.
	contentStringUTF16IndexThreshold Number = 128
	contentStringUTF16IndexStride    Number = 64
	// Once paid for, keep sharing the immutable index down to the measured
	// crossover. Separate activation and retention prevent a 128-unit run from
	// discarding its index on the very first split while standalone short runs
	// still carry nothing.
	contentStringUTF16IndexRetention Number = 64
)

type contentStringUTF16Sample struct {
	utf16Offset uint32
	byteOffset  uint32
}

// contentStringUTF16Index is anchored to one immutable Go string. Split
// fragments share source and samples; their local byte and UTF-16 origins are
// recovered from the fragment's backing pointer. That keeps a split O(1) in
// index storage and avoids adding per-fragment offset fields to contentString.
type contentStringUTF16Index struct {
	source      string
	utf16Length Number
	samples     []contentStringUTF16Sample
}

func (c *contentString) contentLength() Number {
	if index := c.utf16Index; index != nil {
		if length, ok := index.fragmentLength(c.value); ok {
			return length
		}
		// value is mutable inside the package. Do not retain the old source
		// after its pointer/range fingerprint stops matching.
		c.utf16Index = nil
	}

	length := len(c.value)
	if len(c.value) != 1 || c.value[0] >= utf8.RuneSelf {
		if !isASCIIText(c.value) {
			c.value, length = normalizeNonASCIITextUTF8WithLength(c.value)
		}
	}
	return length
}

func (c *contentString) contentValues() ArrayAny {
	chars := utf16.Encode([]rune(c.value))

	content := make(ArrayAny, 0, len(chars))
	for _, c := range chars {
		// content[i] = c
		content = append(content, c)
	}

	return content
}

func (c *contentString) isCountable() bool {
	return true
}

func (c *contentString) copyContent() itemContent {
	return newContentStringUnchecked(c.value)
}

func (c *contentString) spliceContent(offset Number) itemContent {
	return c.spliceWithLength(offset, c.contentLength())
}

// spliceWithLength splits using the authoritative length already stored on the
// owning Item. Internal item-split paths use this to avoid rescanning the whole
// suffix merely to reconstruct a length they already know.
func (c *contentString) spliceWithLength(offset, length Number) itemContent {
	return c.spliceWithLengthInto(offset, length, &contentString{})
}

// spliceWithLengthInto is the allocation-free split primitive for callers that
// already own stable storage for the right-hand content.
func (c *contentString) spliceWithLengthInto(offset, length Number, rightContent *contentString) *contentString {
	right, _ := c.spliceWithLengthIntoBacking(offset, length, rightContent)
	return right
}

// spliceWithLengthIntoBacking also reports whether both results are views into
// the same immutable backing string. Item splitting records that provenance so
// a later merge can safely rejoin the views without confusing two independent
// heap strings that merely happen to be adjacent in memory.
func (c *contentString) spliceWithLengthIntoBacking(
	offset, length Number,
	rightContent *contentString,
) (*contentString, bool) {
	if offset < 0 {
		panic("offset out of range")
	}

	var left, right string
	validatedASCII := c.hasASCIIWidth(length)
	keepsBacking := validatedASCII
	var sharedUTF16Index *contentStringUTF16Index
	leftLength, rightLength := offset, length-offset
	switch {
	case validatedASCII:
		// UTF-8 uses at least as many bytes as UTF-16 uses code units, with
		// equality only when every unit occupies one byte (ASCII; invalid UTF-8
		// bytes also advance Go's range by one byte and one replacement rune).
		// The cached length therefore proves that a UTF-16 offset is the same
		// byte offset, so the overwhelmingly common text path is O(1).
		if offset > length {
			offset = length
		}
		leftLength, rightLength = offset, length-offset
		left, right = c.value[:offset], c.value[offset:]
	case offset <= 8:
		// Front splits were already O(offset). Keep very small offsets on that
		// bounded path instead of building or consulting an index merely to
		// avoid a scan of one or two runes. An existing index still follows the
		// zero-copy fragments; its backing range is checked before a later
		// indexed lookup.
		left, right, keepsBacking = splitStringUTF16AtBoundary(c.value, offset)
		if c.utf16Index != nil {
			if keepsBacking {
				sharedUTF16Index = c.utf16Index
			}
		}
	case c.utf16Index == nil && length < contentStringUTF16IndexThreshold:
		// Do not scan a short value once to discover that it is below the
		// activation threshold and then scan it again to perform the split.
		left, right, keepsBacking = splitStringUTF16AtBoundary(c.value, offset)
	default:
		index := c.validUTF16Index()
		if index == nil {
			index = buildContentStringUTF16Index(c.value, length)
		}
		if index != nil {
			left, right, leftLength, rightLength, keepsBacking = index.split(c.value, offset, length)
			if keepsBacking {
				sharedUTF16Index = index
			}
		} else {
			left, right, keepsBacking = splitStringUTF16AtBoundary(c.value, offset)
			leftLength, rightLength = stringLength(left), stringLength(right)
		}
	}
	c.value = left
	*rightContent = contentString{value: right}
	if !validatedASCII && sharedUTF16Index != nil {
		if leftLength >= contentStringUTF16IndexRetention {
			c.utf16Index = sharedUTF16Index
		} else {
			c.utf16Index = nil
		}
		if rightLength >= contentStringUTF16IndexRetention {
			rightContent.utf16Index = sharedUTF16Index
		}
	} else {
		c.utf16Index = nil
	}
	return rightContent, keepsBacking
}

func (c *contentString) hasASCIIWidth(length Number) bool {
	// Every package-owned value is normalized before its Item length becomes
	// authoritative. Valid UTF-8 uses exactly one byte per UTF-16 unit only for
	// ASCII, so equality proves that byte and UTF-16 offsets are identical.
	return len(c.value) == length
}

func (c *contentString) validUTF16Index() *contentStringUTF16Index {
	index := c.utf16Index
	if index == nil {
		return nil
	}
	if _, _, ok := index.fragmentByteRange(c.value); !ok {
		c.utf16Index = nil
		return nil
	}
	return index
}

func buildContentStringUTF16Index(source string, lengthHint Number) *contentStringUTF16Index {
	// Compact samples halve retained index memory on 64-bit hosts. Extremely
	// large strings fall back to the scan rather than truncating either offset.
	if len(source) == 0 || uint64(len(source)) > uint64(^uint32(0))/2 {
		return nil
	}

	// The owning Item's length is authoritative on internal paths and gives an
	// exact capacity hint. Clamp it for callers that retained a stale length
	// after replacing value directly inside the package.
	maxHint := len(source) * 2
	if lengthHint < contentStringUTF16IndexThreshold {
		lengthHint = contentStringUTF16IndexThreshold
	}
	if lengthHint > maxHint {
		lengthHint = maxHint
	}
	sampleCapacity := lengthHint/contentStringUTF16IndexStride + 2

	var samples []contentStringUTF16Sample
	units := Number(0)
	nextSample := contentStringUTF16IndexStride
	for byteOffset, r := range source {
		if units >= nextSample {
			if samples == nil {
				samples = make([]contentStringUTF16Sample, 1, sampleCapacity)
			}
			samples = append(samples, contentStringUTF16Sample{
				utf16Offset: uint32(units),
				byteOffset:  uint32(byteOffset),
			})
			for nextSample <= units {
				nextSample += contentStringUTF16IndexStride
			}
		}
		units++
		if r >= 0x10000 {
			units++
		}
	}
	if units < contentStringUTF16IndexThreshold {
		return nil
	}
	if samples == nil {
		samples = make([]contentStringUTF16Sample, 1, sampleCapacity)
	}
	if last := samples[len(samples)-1]; int(last.byteOffset) != len(source) {
		samples = append(samples, contentStringUTF16Sample{
			utf16Offset: uint32(units),
			byteOffset:  uint32(len(source)),
		})
	}
	return &contentStringUTF16Index{source: source, utf16Length: units, samples: samples}
}

func (index *contentStringUTF16Index) fragmentLength(fragment string) (Number, bool) {
	byteStart, byteEnd, ok := index.fragmentByteRange(fragment)
	if !ok {
		return 0, false
	}
	utf16Start, ok := index.utf16OffsetAtByte(byteStart)
	if !ok {
		return 0, false
	}
	utf16End, ok := index.utf16OffsetAtByte(byteEnd)
	if !ok {
		return 0, false
	}
	return utf16End - utf16Start, true
}

func (index *contentStringUTF16Index) fragmentByteRange(fragment string) (int, int, bool) {
	if len(fragment) == 0 || len(index.source) == 0 {
		return 0, 0, false
	}
	sourceAddress := uintptr(unsafe.Pointer(unsafe.StringData(index.source)))
	fragmentAddress := uintptr(unsafe.Pointer(unsafe.StringData(fragment)))
	if fragmentAddress < sourceAddress {
		return 0, 0, false
	}
	delta := fragmentAddress - sourceAddress
	if delta > uintptr(len(index.source)) {
		return 0, 0, false
	}
	byteStart := int(delta)
	if len(fragment) > len(index.source)-byteStart {
		return 0, 0, false
	}
	return byteStart, byteStart + len(fragment), true
}

func (index *contentStringUTF16Index) utf16OffsetAtByte(target int) (Number, bool) {
	if target < 0 || target > len(index.source) {
		return 0, false
	}
	sample := index.sampleAtOrBeforeByte(target)
	units, byteOffset := Number(sample.utf16Offset), int(sample.byteOffset)
	for byteOffset < target {
		r, size := utf8.DecodeRuneInString(index.source[byteOffset:])
		if byteOffset+size > target {
			return 0, false
		}
		units++
		if r >= 0x10000 {
			units++
		}
		byteOffset += size
	}
	return units, byteOffset == target
}

// byteOffsetAtUTF16 returns the source byte boundary at target. The bool is
// true only when target bisects a supplementary rune's UTF-16 surrogate pair.
func (index *contentStringUTF16Index) byteOffsetAtUTF16(target Number) (int, bool) {
	if target <= 0 {
		return 0, false
	}
	if target >= index.utf16Length {
		return len(index.source), false
	}
	sample := index.sampleAtOrBeforeUTF16(target)
	units, byteOffset := Number(sample.utf16Offset), int(sample.byteOffset)
	for byteOffset < len(index.source) {
		if units == target {
			return byteOffset, false
		}
		r, size := utf8.DecodeRuneInString(index.source[byteOffset:])
		width := Number(1)
		if r >= 0x10000 {
			width = 2
		}
		if units+width > target {
			return byteOffset, true
		}
		units += width
		byteOffset += size
	}
	return len(index.source), false
}

func (index *contentStringUTF16Index) sampleAtOrBeforeByte(target int) contentStringUTF16Sample {
	low, high := 0, len(index.samples)
	for low < high {
		middle := int(uint(low+high) >> 1)
		if int(index.samples[middle].byteOffset) <= target {
			low = middle + 1
		} else {
			high = middle
		}
	}
	return index.samples[low-1]
}

func (index *contentStringUTF16Index) sampleAtOrBeforeUTF16(target Number) contentStringUTF16Sample {
	low, high := 0, len(index.samples)
	for low < high {
		middle := int(uint(low+high) >> 1)
		if Number(index.samples[middle].utf16Offset) <= target {
			low = middle + 1
		} else {
			high = middle
		}
	}
	return index.samples[low-1]
}

func (index *contentStringUTF16Index) split(fragment string, offset, length Number) (
	left, right string, leftLength, rightLength Number, keepsIndex bool,
) {
	byteStart, byteEnd, ok := index.fragmentByteRange(fragment)
	if !ok {
		left, right = splitStringUTF16(fragment, offset)
		return left, right, stringLength(left), stringLength(right), false
	}
	if offset >= length {
		return fragment, "", length, 0, true
	}
	if offset == 0 {
		return "", fragment, 0, length, true
	}
	utf16Start, startOK := index.utf16OffsetAtByte(byteStart)
	if !startOK {
		left, right = splitStringUTF16(fragment, offset)
		return left, right, stringLength(left), stringLength(right), false
	}

	absoluteByteOffset, bisectsSurrogate := index.byteOffsetAtUTF16(utf16Start + offset)
	if absoluteByteOffset >= byteEnd {
		return fragment, "", length, 0, true
	}
	localByteOffset := absoluteByteOffset - byteStart
	if bisectsSurrogate {
		_, runeBytes := utf8.DecodeRuneInString(fragment[localByteOffset:])
		return fragment[:localByteOffset] + "�", "�" + fragment[localByteOffset+runeBytes:],
			offset, length - offset, false
	}
	return fragment[:localByteOffset], fragment[localByteOffset:], offset, length - offset, true
}

func (c *contentString) mergeContentWith(right itemContent) bool {
	r, ok := right.(*contentString)
	if !ok {
		return false
	}

	c.value = mergeString(c.value, r.value)
	c.utf16Index = nil
	return true
}

// mergeSplitRight rejoins adjacent fragments produced by one zero-copy split.
// The caller must supply that provenance; pointer adjacency alone is not enough
// because two independent heap allocations can happen to be neighbours.
func (c *contentString) mergeSplitRight(right *contentString, leftLength, rightLength Number) bool {
	if len(c.value) == 0 || len(right.value) == 0 {
		return false
	}
	leftData := unsafe.StringData(c.value)
	// Compare the two spans as integer addresses rather than computing
	// unsafe.Add(leftData, len(c.Str)). When the fragments are NOT adjacent —
	// precisely the case this check exists to reject — that addition lands one
	// past the end of the left allocation, which is not a valid unsafe.Pointer.
	// checkptr (on under -race) kills the process with "pointer arithmetic result
	// points to invalid allocation", and the pointer is ill-defined with or
	// without the check, since the rule is a property of the language rather than
	// of the detector. Doing the arithmetic in uintptr space never materialises
	// that pointer: the result is only compared, never converted back.
	if uintptr(unsafe.Pointer(leftData))+uintptr(len(c.value)) !=
		uintptr(unsafe.Pointer(unsafe.StringData(right.value))) {
		return false
	}
	merged := unsafe.String(leftData, len(c.value)+len(right.value))
	index := c.utf16Index
	if index == nil {
		index = right.utf16Index
	}
	if index != nil {
		if length, ok := index.fragmentLength(merged); !ok || length != leftLength+rightLength {
			index = nil
		}
	}
	c.value = merged
	c.utf16Index = index
	return true
}

func (c *contentString) setMergedString(str string) {
	c.value = str
	c.utf16Index = nil
}

func (c *contentString) integrateContent(trans *Transaction, item *itemStruct) {

}

func (c *contentString) deleteContent(trans *Transaction) {

}

func (c *contentString) gcContent(store *structStore) {

}

func (c *contentString) writeContent(encoder updateEncoder, offset Number) error {
	if offset == 0 {
		return encoder.writeStringValue(c.value)
	}
	return encoder.writeStringValue(stringTail(c.value, offset))
}

func (c *contentString) contentRef() uint8 {
	return refContentString
}

func newContentString(str string) *contentString {
	return newContentStringUnchecked(str)
}

// newContentStringUnchecked constructs content without computing its length.
// NewItem remains the authority that asks GetLength when needed; split paths
// that already know the right-hand length pair this with newItemWithLength.
func newContentStringUnchecked(str string) *contentString {
	return &contentString{value: str}
}

func readContentString(decoder updateDecoder) (itemContent, error) {
	str, err := decoder.readStringValue()
	if err != nil {
		return nil, err
	}

	return newContentString(str), nil
}

// ---------------------------------------------------------------- from content_type.go
var typeRefs = []func(decoder updateDecoder) (abstractType, error){
	readYArray,
	readYMap,
	readYText,
	readYXmlElement,
	readYXmlFragment,
	readYXmlHook,
	readYXmlText,
}

type contentType struct {
	value abstractType
}

func (c *contentType) contentLength() Number {
	return 1
}

func (c *contentType) contentValues() ArrayAny {
	return ArrayAny{c.value}
}

func (c *contentType) isCountable() bool {
	return true
}

// copyContent returns a ContentType wrapping a FRESH EMPTY type of the same kind, matching yjs
// (src/structs/ContentType.js): `new ContentType(this.type._copy())`. Go's per-type
// `copyContent() IAbstractType` is exactly that `_copy()`.
//
// It must NOT deep-clone. The sole caller is RedoItem (item.go), and during a redo the
// nested type's children are re-created by their own redoItem calls — so cloning here
// double-materialises them AND resurrects tombstoned entries the redo should leave
// deleted. Reproduced: insert a nested Y.Map, set a+b, delete b, all inside one capture
// window, then undo+redo → Go yielded keys [a b] where yjs yields [a]. The previous
// deep-clone (plus a YMap special case that walked GetMap() precisely to INCLUDE
// tombstoned fields) was scaffolding for the older RedoItem and outlived it.
func (c *contentType) copyContent() itemContent {
	return newContentType(c.value.copyType())
}

func (c *contentType) spliceContent(offset Number) itemContent {
	return nil
}

func (c *contentType) mergeContentWith(right itemContent) bool {
	return false
}

func (c *contentType) integrateContent(trans *Transaction, item *itemStruct) {
	c.value.integrate(trans.doc, item)
}

// deleteContent cascades the deletion into the nested type's children, matching yjs
// (src/structs/ContentType.js delete). It walks the _start linked list and the
// _map; live children are deleted, and already-tombstoned children whose clock is
// below the transaction's beforeState are queued for merge (gc'd later). Absent
// clients yield clock 0 via the Go map zero value (== yjs `(… || 0)`).
func (c *contentType) deleteContent(trans *Transaction) {
	// The nested type can no longer be addressed once its containing Item is deleted, so its
	// writer-only position index is pure retained memory. Tear it down here rather than waiting
	// for ContentType.GC: documents with GC disabled deliberately retain history and may never run
	// that path. Do this before cascading so deleting thousands of children does not maintain an
	// accelerator that is about to be discarded.
	destroyListPositionIndex(c.value)
	for item := c.value.startItem(); item != nil; item = item.right {
		if !item.isDeleted() {
			item.deleteItemStruct(trans)
		} else if item.id.Clock < trans.beforeClock(item.id.Client) {
			trans.mergeStructs = append(trans.mergeStructs, item)
		}
	}
	for _, item := range c.value.getMap() {
		if !item.isDeleted() {
			item.deleteItemStruct(trans)
		} else if item.id.Clock < trans.beforeClock(item.id.Client) {
			trans.mergeStructs = append(trans.mergeStructs, item)
		}
	}
	trans.deleteChangedType(c.value)
}

// gcContent cascades garbage collection into the nested type's children, matching yjs
// (src/structs/ContentType.js gc): each child in _start and _map is gcContent'd (replaced
// with a gcContent struct), then _start and _map are reset.
func (c *contentType) gcContent(store *structStore) {
	for item := c.value.startItem(); item != nil; item = item.right {
		item.gcItem(store, true)
	}
	c.value.setStartItem(nil)
	for _, item := range c.value.getMap() {
		for item != nil {
			item.gcItem(store, true)
			item = item.left
		}
	}
	c.value.setMap(make(map[string]*itemStruct))
}

func (c *contentType) writeContent(encoder updateEncoder, offset Number) error {
	c.value.writeType(encoder)
	return nil
}

func (c *contentType) contentRef() uint8 {
	return refContentType
}

func newContentType(t abstractType) *contentType {
	return &contentType{value: t}
}

func readContentType(decoder updateDecoder) (itemContent, error) {
	refID, err := decoder.readTypeRef()
	if err != nil {
		return nil, err
	}

	if int(refID) >= len(typeRefs) {
		return nil, fmt.Errorf("index out of range. refID:%d len:%d", refID, len(typeRefs))
	}

	refType, err := typeRefs[refID](decoder)
	if err != nil {
		return nil, err
	}

	return newContentType(refType), nil
}

func readYArray(decoder updateDecoder) (abstractType, error) {
	return NewYArray(), nil
}

func readYMap(decoder updateDecoder) (abstractType, error) {
	return NewYMap(nil), nil
}

func readYText(decoder updateDecoder) (abstractType, error) {
	return NewYText(""), nil
}

func readYXmlElement(decoder updateDecoder) (abstractType, error) {
	key, err := decoder.readKey()
	if err != nil {
		return nil, err
	}

	return NewYXmlElement(key), nil
}

func readYXmlFragment(decoder updateDecoder) (abstractType, error) {
	return NewYXmlFragment(), nil
}

func readYXmlHook(decoder updateDecoder) (abstractType, error) {
	key, err := decoder.readKey()
	if err != nil {
		return nil, err
	}

	return newYXmlHook(key), nil
}

func readYXmlText(decoder updateDecoder) (abstractType, error) {
	return NewYXmlText(), nil
}
