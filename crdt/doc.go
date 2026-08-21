// Package crdt implements the Yjs CRDT algorithms, wire-compatible with the
// JavaScript reference implementation.
//
// It provides Y.Doc and the shared types (Y.Text, Y.Array, Y.Map, Y.XmlFragment,
// Y.XmlElement, Y.XmlText), both update codecs (V1 and V2), transactions,
// snapshots, relative positions, an undo manager, awareness, and garbage
// collection. Byte-for-byte compatibility with the reference is the correctness
// test and is enforced by a differential oracle rather than by hand-written
// expectations.
package crdt

import "fmt"

// ---------------------------------------------------------------- from doc.go

// Doc is a Yjs document: the container for shared types, the transaction
// boundary, and the unit of synchronisation.
type Doc struct {
	*Observable
	GUID     string
	ClientID Number

	GC           bool
	gcFilter     func(item *itemStruct) bool
	share        map[string]abstractType
	store        *structStore
	trans        *Transaction
	transCleanup []*Transaction
	// mutationTransaction is scratch storage for a completed package-owned mutation that no
	// callback could retain. Public/observed transactions are never stored here.
	mutationTransaction *Transaction
	// textMutationScratch backs transient ordered attribute state for package-owned YText
	// mutations. It is reset before every use and never escapes the synchronous mutation callback.
	// Keeping one pointer here preserves Doc's allocation class.
	textMutationScratch *insertTextObjectScratch
	// positionIndexes is allocated only when a plain sequence reaches the 16k-Item activation
	// threshold. Keying by the embedded AbstractType keeps ordinary types at their original size.
	positionIndexes map[*abstractTypeBase]*listPositionIndex
	// mapItemBlock amortizes single primitive ContentAny allocations for map entries and list
	// inserts in bounded blocks. Items point into a fixed block, so their addresses remain stable;
	// at most 31 unused entries remain retained at the tail of a document.
	mapItemBlock        []itemWithSingleAny
	mapItemBlockUsed    int
	mapItemNextBlockCap int
	// typeItemBlock co-locates local ContentType wrappers with their Items. Like mapItemBlock, it
	// grows in stable bounded blocks because Items and ContentType values both escape by pointer.
	typeItemBlock        []itemWithContentType
	typeItemBlockUsed    int
	typeItemNextBlockCap int
	// stringItemBlock co-locates local ContentString values with their Items. Blocks grow from
	// one slot and cap at 32, so a one-item text retains no speculative tail while fragmented
	// text amortizes both escaping pointers into one allocation per block.
	stringItemBlock        []itemWithContentString
	stringItemBlockUsed    int
	stringItemNextBlockCap int
	// formatItemBlock co-locates local formatting Items with their ContentFormat. Rich-text
	// edits create two boundaries per attribute; stable blocks grow from one slot to eight so a
	// small edit retains no speculative tail while a fragmented document amortizes allocations.
	// Origins are stored separately because only about half the boundaries need one; embedding an
	// ID in every slot adds avoidable retained bytes.
	formatItemBlock     []itemWithContentFormat
	formatItemBlockUsed int
	// itemOriginBlock owns stable IDs referenced by locally-created items whose
	// origin is not already an addressable stored ID (format boundaries and
	// split right-hand items). Its stable blocks likewise grow from one slot, capped at 32.
	itemOriginBlock     []ID
	itemOriginBlockUsed int
	// embedItemBlock co-locates local text embeds with their Items. Embeds never
	// merge, so a fragmented embed run otherwise pays two escaping allocations
	// per element (Item plus ContentEmbed). The smaller cap of 16 limits unused
	// tail storage for the common short embed run while still amortizing heavily.
	embedItemBlock        []itemWithContentEmbed
	embedItemBlockUsed    int
	embedItemNextBlockCap int
	subDocs               Set
	item                  *itemStruct // If this document is a subdocument - a document integrated into another document - then _item is defined.
	ShouldLoad            bool
	AutoLoad              bool
	// readCacheEnabled is immutable after construction. It is packed beside the existing bools so
	// the rare position-index map reuses its former aligned slot without growing Doc.
	readCacheEnabled bool
	Meta             interface{}
}

func (doc *Doc) allocateMapItemStorage() *itemWithSingleAny {
	if doc.mapItemBlockUsed == len(doc.mapItemBlock) {
		blockSize := doc.mapItemNextBlockCap
		if blockSize == 0 {
			blockSize = 1
		}
		doc.mapItemBlock = make([]itemWithSingleAny, blockSize)
		doc.mapItemBlockUsed = 0
		if blockSize < 32 {
			doc.mapItemNextBlockCap = blockSize * 2
		} else {
			doc.mapItemNextBlockCap = 32
		}
	}

	storage := &doc.mapItemBlock[doc.mapItemBlockUsed]
	doc.mapItemBlockUsed++
	return storage
}

func (doc *Doc) allocateTypeItemStorage() *itemWithContentType {
	if doc.typeItemBlockUsed == len(doc.typeItemBlock) {
		blockSize := doc.typeItemNextBlockCap
		if blockSize == 0 {
			blockSize = 1
		}
		doc.typeItemBlock = make([]itemWithContentType, blockSize)
		doc.typeItemBlockUsed = 0
		if blockSize < 32 {
			doc.typeItemNextBlockCap = blockSize * 2
		} else {
			doc.typeItemNextBlockCap = 32
		}
	}

	storage := &doc.typeItemBlock[doc.typeItemBlockUsed]
	doc.typeItemBlockUsed++
	return storage
}

func (doc *Doc) allocateStringItemStorage() *itemWithContentString {
	if doc.stringItemBlockUsed == len(doc.stringItemBlock) {
		blockSize := doc.stringItemNextBlockCap
		if blockSize == 0 {
			blockSize = 1
		}
		doc.stringItemBlock = make([]itemWithContentString, blockSize)
		doc.stringItemBlockUsed = 0
		if blockSize < 32 {
			doc.stringItemNextBlockCap = blockSize * 2
		} else {
			doc.stringItemNextBlockCap = 32
		}
	}

	storage := &doc.stringItemBlock[doc.stringItemBlockUsed]
	doc.stringItemBlockUsed++
	return storage
}

func (doc *Doc) reserveStringItemStorage(count int) {
	if count <= len(doc.stringItemBlock)-doc.stringItemBlockUsed {
		return
	}
	doc.stringItemBlock = make([]itemWithContentString, count)
	doc.stringItemBlockUsed = 0
	nextBlockCap := count * 2
	if nextBlockCap > 32 {
		nextBlockCap = 32
	}
	if nextBlockCap > doc.stringItemNextBlockCap {
		doc.stringItemNextBlockCap = nextBlockCap
	}
}

func (doc *Doc) allocateFormatItemStorage() *itemWithContentFormat {
	if doc.formatItemBlockUsed == len(doc.formatItemBlock) {
		blockSize := len(doc.formatItemBlock) * 2
		if blockSize == 0 {
			blockSize = 1
		}
		if blockSize > 8 {
			blockSize = 8
		}
		doc.formatItemBlock = make([]itemWithContentFormat, blockSize)
		doc.formatItemBlockUsed = 0
	}

	storage := &doc.formatItemBlock[doc.formatItemBlockUsed]
	doc.formatItemBlockUsed++
	return storage
}

// reserveFormatItemStorage makes the next count formatting allocations share one
// exactly-sized backing block. Callers must know the number of immediately-following
// allocations: unlike the ordinary bounded growth path, this deliberately favors one
// large allocation and must not retain a speculative tail.
func (doc *Doc) reserveFormatItemStorage(count int) {
	if count <= len(doc.formatItemBlock)-doc.formatItemBlockUsed {
		return
	}
	doc.formatItemBlock = make([]itemWithContentFormat, count)
	doc.formatItemBlockUsed = 0
}

func (doc *Doc) allocateItemOriginStorage() *ID {
	if doc.itemOriginBlockUsed == len(doc.itemOriginBlock) {
		blockSize := len(doc.itemOriginBlock) * 2
		if blockSize == 0 {
			blockSize = 1
		}
		if blockSize > 32 {
			blockSize = 32
		}
		doc.itemOriginBlock = make([]ID, blockSize)
		doc.itemOriginBlockUsed = 0
	}
	origin := &doc.itemOriginBlock[doc.itemOriginBlockUsed]
	doc.itemOriginBlockUsed++
	return origin
}

func (doc *Doc) reserveItemOriginStorage(count int) {
	if count <= len(doc.itemOriginBlock)-doc.itemOriginBlockUsed {
		return
	}
	doc.itemOriginBlock = make([]ID, count)
	doc.itemOriginBlockUsed = 0
}

func (doc *Doc) allocateEmbedItemStorage() *itemWithContentEmbed {
	if doc.embedItemBlockUsed == len(doc.embedItemBlock) {
		blockSize := doc.embedItemNextBlockCap
		if blockSize == 0 {
			blockSize = 1
		}
		doc.embedItemBlock = make([]itemWithContentEmbed, blockSize)
		doc.embedItemBlockUsed = 0
		if blockSize < 16 {
			doc.embedItemNextBlockCap = blockSize * 2
		} else {
			doc.embedItemNextBlockCap = 16
		}
	}

	storage := &doc.embedItemBlock[doc.embedItemBlockUsed]
	doc.embedItemBlockUsed++
	return storage
}

// Load notifies the parent document that this subdocument requests its data be
// loaded (if it is a subdocument).
//
//	`load()` might be used in the future to request any provider to load the most current data.
//	It is safe to call `load()` multiple times.
func (doc *Doc) Load() {
	item := doc.item
	if item != nil && !doc.ShouldLoad {
		parent := item.parent.(abstractType)
		Transact(parent.getDoc(), func(trans *Transaction) {
			trans.addSubdocLoaded(doc)
		}, nil, true)
	}
	doc.ShouldLoad = true
}

func (doc *Doc) GetSubdocs() Set {
	return doc.subDocs
}

func (doc *Doc) GetSubdocGUIDs() Set {
	s := NewSet()
	for k := range doc.subDocs {
		// SubDocs holds *Doc (see Destroy/GetSubdocs); yjs getSubdocGUIDs maps
		// each subdoc to its guid (new Set(Array.from(this.subdocs).map(d => d.guid))).
		guid := k.(*Doc).GUID
		s.Add(guid)
	}
	return s
}

// Transact runs f inside a transaction.
//
// Changes that happen inside of a transaction are bundled. This means that
// the observer fires _after_ the transaction is finished and that all changes
// that happened inside of the transaction are sent as one message to the
// other peers.
func (doc *Doc) Transact(f func(trans *Transaction), origin interface{}) {
	Transact(doc, f, origin, true)
}

// Get defines a shared data type.
//
// Multiple calls of `y.get(name, TypeConstructor)` yield the same result
// and do not overwrite each other. I.e.
// `y.define(name, Y.Array) === y.define(name, Y.Array)`
//
// After this method is called, the type is also available on `y.share.get(name)`.
//
// Best Practices:
// Define all types right after the Yjs instance is created and store them in a separate object.
// Also use the typed methods `getText(name)`, `getArray(name)`, ..
//
// example
//
//	const y = new Y(..)
//	const appState = {
//	  document: y.getText('document')
//	  comments: y.getArray('comments')
//	}
func (doc *Doc) Get(name string, typeConstructor TypeConstructor) (SharedType, error) {
	constr, exist := doc.share[name]
	if !exist {
		requested := asAbstractType(typeConstructor())
		requested.integrate(doc, nil)
		doc.share[name] = requested
		return asSharedType(requested), nil
	}

	requested := asAbstractType(typeConstructor())
	_, requestedIsGeneric := requested.(*abstractTypeBase)
	if !requestedIsGeneric && !isSameType(constr, requested) {
		if _, existingIsGeneric := constr.(*abstractTypeBase); existingIsGeneric {
			requested.setMap(constr.getMap())
			if ymap, ok := requested.(*YMap); ok {
				ymap.recountSize()
			}
			for _, n := range requested.getMap() {
				for ; n != nil; n = n.left {
					n.parent = requested
				}
			}

			requested.setStartItem(constr.startItem())
			itemCount := Number(0)
			for n := requested.startItem(); n != nil; n = n.right {
				n.parent = requested
				itemCount++
			}
			setListItemCount(requested, itemCount)

			requested.setLength(constr.GetLength())
			requested.integrate(doc, nil)
			doc.share[name] = requested
			return asSharedType(requested), nil
		} else {
			return nil, fmt.Errorf("type with the name %s has already been defined with a different constructor ", name)
		}
	}

	return asSharedType(constr), nil
}

// getShared returns the type already registered under name when its dynamic type
// already satisfies the caller, so the caller never constructs a value it is
// about to discard.
//
// WHY. Doc.Get takes a constructor and calls it UNCONDITIONALLY, including on the
// path where the type already exists and the constructed value is used only to
// compare types and then dropped. That is not a rare path: the eager struct
// decoder resolves every named parent through Get, so applying an update to a
// map-heavy document constructs and discards one AbstractType, with its map, per
// root-parented struct. Measured on a 4,000-key document, throwaway
// NewAbstractType values were 29.48% of every object allocated during a connect —
// the single largest allocation site in the whole apply path, ahead of ReadString.
//
// Get keeps its general behaviour, including the generic-to-concrete migration
// and the conflicting-constructor error, because those need the requested value.
// These helpers only skip ahead when the answer cannot be anything else.
func (doc *Doc) getShared(name string) (abstractType, bool) {
	existing, ok := doc.share[name]
	return existing, ok
}

// getGeneric is Get with NewAbstractType, minus the construction that variant
// always discards: a generic request never triggers migration and never
// conflicts, so an existing type is always the answer.
func (doc *Doc) getGeneric(name string) abstractType {
	if existing, ok := doc.getShared(name); ok {
		return existing
	}
	created := asAbstractType(newAbstractType())
	created.integrate(doc, nil)
	doc.share[name] = created
	return created
}

func (doc *Doc) GetArray(name string) *YArray {
	if existing, ok := doc.getShared(name); ok {
		if a, isArray := existing.(*YArray); isArray {
			return a
		}
	}

	arr, err := doc.Get(name, newYArrayType)
	if err != nil {
		return nil
	}

	a, ok := arr.(*YArray)
	if ok {
		return a
	}

	return nil
}

func (doc *Doc) GetText(name string) *YText {
	if existing, ok := doc.getShared(name); ok {
		if t, isText := existing.(*YText); isText {
			return t
		}
	}
	text, err := doc.Get(name, newYTextType)
	if err != nil {
		return nil
	}

	a, ok := text.(*YText)
	if ok {
		return a
	}

	return nil
}

func (doc *Doc) GetMap(name string) *YMap {
	if existing, ok := doc.getShared(name); ok {
		if m, isMap := existing.(*YMap); isMap {
			return m
		}
	}
	m, err := doc.Get(name, newYMapType)
	if err != nil {
		return nil
	}
	result, _ := m.(*YMap)
	return result
}

func (doc *Doc) GetXMLFragment(name string) *YXmlFragment {
	if existing, ok := doc.getShared(name); ok {
		if fragment, isFragment := existing.(*YXmlFragment); isFragment {
			return fragment
		}
	}
	xml, err := doc.Get(name, newYXmlFragmentType)
	if err != nil {
		return nil
	}
	result, _ := xml.(*YXmlFragment)
	return result
}

// ToJSON converts the entire document into a js object, recursively traversing each yjs type
// Doesn't log types that have not been defined (using ydoc.getType(..)).
//
// Do not use this method and rather call toJSON directly on the shared types.
func (doc *Doc) ToJSON() Object {
	object := newObject()
	for key, value := range doc.share {
		object.Set(key, value.toJSONValue())
	}
	return object
}

// Destroy emits the `destroy` event and unregisters all event handlers.
func (doc *Doc) Destroy() {
	// A destroyed document may remain reachable to the caller even after its parent removes it.
	// Discard its writer-only accelerators now; retaining them cannot preserve CRDT history, and a
	// later mutation of the still-readable Doc will rebuild an index lazily if it needs one.
	doc.destroyListPositionIndexes()

	// Snapshot the subdocs before destroying them (yjs: array.from(this.subdocs)).
	// subDoc.Destroy() reconstructs a replacement and re-adds it into THIS doc's
	// SubDocs (via the nested transaction's SubdocsAdded -> cleanupTransactions), so
	// ranging the live map would re-visit the replacements (Go leaves add-during-range
	// undefined) and re-destroy them non-deterministically.
	subs := make([]*Doc, 0, len(doc.subDocs))
	for k := range doc.subDocs {
		subs = append(subs, k.(*Doc))
	}
	for _, subDoc := range subs {
		subDoc.Destroy()
	}

	item := doc.item
	if item != nil {
		doc.item = nil
		// The item's content is NOT guaranteed to still be a *ContentDoc: once the item is
		// tombstoned, tryGcDeleteSet (transaction.go) rewrites Content to *ContentDeleted.
		// That is reachable in ordinary use — deleting a subdoc key from a GC-enabled (the
		// default) Doc populates trans.SubdocsRemoved, whose cleanup loop calls Destroy()
		// AFTER the GC pass — and an unchecked assertion panicked there. yjs survives only
		// because its `/** @type {ContentDoc} */ (item.content)` is an erased TS cast and
		// the subsequent property write on a ContentDeleted is inert. There is nothing to
		// reconstruct once the content is gone, so skip the rebuild to match that effect.
		// (Skips only the rebuild — the destroy events below MUST still fire.)
		content, isDoc := item.content.(*contentDoc)
		if isDoc {
			// yjs Doc.destroy UNCONDITIONALLY reconstructs the replacement subdoc from opts
			// (`content.doc = new Doc({...opts, shouldLoad:false}); content.doc._item = item`)
			// — regardless of item.deleted. Previously the deleted case set content.Doc = nil,
			// which left a nil that crashed redo of a subdoc insertion (RedoItem ->
			// ContentDoc.Copy dereferenced the nil doc). Opts stores only NON-default options
			// (NewContentDoc writes optKeyGC only when gc is false, optKeyAutoLoad only when
			// true), so default gc=true / autoLoad=false on an absent key, like ReadContentDoc.
			// Rebuild via the shared newSubdocFromOpts (gc=true/autoLoad=false defaults, like
			// ReadContentDoc), then force ShouldLoad=false — yjs Doc.destroy reconstructs the
			// subdoc with `shouldLoad: false` (not load-pending until Load()).
			content.doc = newSubdocFromOpts(doc.GUID, content.opts)
			content.doc.ShouldLoad = false
			content.doc.item = item

			Transact(item.parent.(abstractType).getDoc(), func(trans *Transaction) {
				if !item.isDeleted() {
					trans.addSubdocAdded(content.doc)
				}
				// Always record the destroyed doc as removed (yjs subdocsRemoved.add(this)),
				// so the parent drops its stale SubDocs pointer and emits the 'removed' set.
				trans.addSubdocRemoved(doc)
			}, nil, true)
		}
	}

	doc.Emit("destroyed", true)
	doc.Emit("destroy", doc)
	doc.Observable.Destroy()
}

func (doc *Doc) On(eventName string, handler *ObserverHandler) {
	doc.Observable.On(eventName, handler)
}

func (doc *Doc) Off(eventName string, handler *ObserverHandler) {
	doc.Observable.Off(eventName, handler)
}

// DocOption customizes a Doc at construction. It exists primarily to inject a
// deterministic ClientID (WithClientID) so byte-parity tests can pin the client
// id to a fixture value without monkey-patching generateNewClientID. Production
// callers pass no options and get a random client id as before.
type DocOption func(*Doc)

// WithClientID pins the document's ClientID to a fixed value instead of the
// random generateNewClientID(). Used by the V2 byte-parity tests to match the JS
// fixtures' fixed client id deterministically (and without the fragile mockey
// gcflags-dependent patch).
func WithClientID(clientID Number) DocOption {
	return func(doc *Doc) {
		doc.ClientID = clientID
	}
}

// WithReadCache controls bounded, mutation-invalidated projections used by repeated ToString,
// ToDelta, map and XML reads. It is enabled by default. Disable it for large fleets of mostly-idle
// documents where retained heap matters more than repeated-read latency; see docs/PERFORMANCE.md.
func WithReadCache(enabled bool) DocOption {
	return func(doc *Doc) {
		doc.readCacheEnabled = enabled
	}
}

// WithGC controls garbage collection of deleted content. It is ENABLED by
// default, matching the yjs Doc constructor. Disable it when you need deleted
// items to stay addressable — snapshots and time-travel over a document's
// history both require that.
func WithGC(enabled bool) DocOption {
	return func(doc *Doc) {
		doc.GC = enabled
	}
}

// WithMeta attaches arbitrary application data to the document. The library
// never reads it; it is carried so a service can associate a document with its
// own record without a side table.
func WithMeta(meta interface{}) DocOption {
	return func(doc *Doc) {
		doc.Meta = meta
	}
}

// WithAutoLoad makes a subdocument load its content as soon as it is integrated
// into a parent, rather than waiting for an explicit Load. It is off by default.
func WithAutoLoad(enabled bool) DocOption {
	return func(doc *Doc) {
		doc.AutoLoad = enabled
	}
}

func newDoc(guid string, gc bool, gcFilter func(item *itemStruct) bool, meta interface{}, autoLoad bool, opts ...DocOption) *Doc {
	doc := &Doc{
		Observable:       NewObservable(),
		ClientID:         generateNewClientID(),
		GUID:             guid,
		GC:               gc,
		gcFilter:         gcFilter,
		Meta:             meta,
		AutoLoad:         autoLoad,
		readCacheEnabled: true,
		// Match the yjs Doc constructor default (Doc.js: shouldLoad = true): a
		// locally-created doc is "should load", so an inserted subdoc loads on
		// integrate. Decoded subdocs override this in ReadContentDoc.
		ShouldLoad: true,
		store:      newStructStore(),
		share:      make(map[string]abstractType),
		// SubDocs must be a non-nil Set: Doc.Destroy's subdoc-reconstruct path runs
		// cleanupTransactions, which does `doc.SubDocs.Add(subdoc)` on the parent
		// doc — an Add into a nil map panics. A nil Set and an empty Set are
		// indistinguishable for every SubDocs read (range in GetSubdocs /
		// GetSubdocGUIDs / Destroy, and Delete), so initializing it here only removes
		// that panic and changes nothing else.
		subDocs: NewSet(),
	}

	for _, opt := range opts {
		opt(doc)
	}

	return doc
}

// NewDoc constructs a document. Everything optional is a DocOption, so the
// common case is NewDoc("room-1") and each departure from the defaults is named
// at the call site.
//
// The previous signature took gc, meta and autoLoad positionally. Every one of
// the seventy-odd call sites in this repository passed nil for meta and false
// for autoLoad — two parameters that existed only to be defaulted, in front of
// a reader who had to look up which bool was which.
//
// Defaults: garbage collection ON (matching the yjs Doc constructor), no meta,
// no auto-load. The package's reference-compatible GC filter is always used;
// the internal item graph stays unexposed.
func NewDoc(guid string, opts ...DocOption) *Doc {
	return newDoc(guid, true, defaultGCFilter, nil, false, opts...)
}

// ---------------------------------------------------------------- from doc_update_subscription.go

// OnUpdate subscribes to the document's byte-level V1 update stream: the exact
// bytes to persist or broadcast, plus the origin the mutating transaction was
// given. It returns the handler so it can be passed to OffUpdate.
//
// WHY THIS EXISTS RATHER THAN On("update", ...). The generic observer delivers
// ...interface{}, so every consumer of the most important server-side hook in
// the library begins with a type assertion. That assertion is the whole seam a
// persistence layer hangs off, and getting it wrong fails SILENTLY: the handler
// runs, the assertion does not match, nothing is stored, no error is returned,
// and the document itself is perfectly correct. A relay wired that way loses
// every write and looks healthy doing it.
//
// The mistake is not hypothetical, because the event name is not unique.
// Awareness also emits "update", with an Object payload rather than []byte, so
// the assertion someone copies from the awareness path compiles, runs, matches
// nothing, and drops the entire update stream. See
// TestUpdateSeamCanSilentlyDropEverything.
//
// The generic Doc.On remains for the other events; this only removes the
// guesswork from the one whose payload a server cannot afford to mis-handle.
func (doc *Doc) OnUpdate(handler func(update []byte, origin any)) *ObserverHandler {
	return doc.onUpdateStream("update", handler)
}

// OffUpdate removes a handler registered by OnUpdate.
func (doc *Doc) OffUpdate(handler *ObserverHandler) {
	doc.Off("update", handler)
}

// OnUpdateV2 is OnUpdate for the V2 update stream. A document emits both, so a
// consumer picks the encoding it stores and subscribes to that one only —
// subscribing to both persists every change twice.
func (doc *Doc) OnUpdateV2(handler func(update []byte, origin any)) *ObserverHandler {
	return doc.onUpdateStream("updateV2", handler)
}

// OffUpdateV2 removes a handler registered by OnUpdateV2.
func (doc *Doc) OffUpdateV2(handler *ObserverHandler) {
	doc.Off("updateV2", handler)
}

func (doc *Doc) onUpdateStream(event string, handler func(update []byte, origin any)) *ObserverHandler {
	observer := NewObserverHandler(func(args ...interface{}) {
		if len(args) == 0 {
			return
		}
		update, ok := args[0].([]uint8)
		if !ok {
			// Unreachable from this package: transaction cleanup always emits the
			// encoded bytes first. Dropping silently is what this API exists to
			// prevent, so make an impossible payload loud rather than invisible.
			panic("y-crdt: " + event + " emitted a non-[]byte payload")
		}
		var origin any
		if len(args) > 1 {
			origin = args[1]
		}
		handler(update, origin)
	})
	doc.On(event, observer)
	return observer
}
