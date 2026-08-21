package crdt

import (
	"errors"
	"fmt"
)

// ---------------------------------------------------------------- from struct_decode_stream.go
// maxYjsClientStructCount is the largest length accepted by JavaScript's Array
// constructor. Pinned yjs 13.6.31 allocates new Array(numberOfStructs) before it
// reads the client or base clock, so the shared stream rejects the first value
// that yjs itself cannot materialize at that same boundary.
const maxYjsClientStructCount int64 = (1 << 32) - 1

type structDecodeStreamOptions struct {
	mode                  structDecodeMode
	doc                   *Doc
	lenientMissingHeader  bool
	useItemArena          bool
	useStringContentArena bool
	useAnyContentArena    bool
	useFormatContentArena bool
}

// structDecodeBlock is the framing the stream reads before any structs in one
// client block. Keeping it explicit lets adapters make block-level allocation
// decisions without re-reading the wire grammar.
type structDecodeBlock struct {
	Client        Number
	StartClock    Number
	DeclaredCount Number
	ReserveCount  uint64
}

// structDecodeResult describes one declared wire primitive. Value is nil only
// for a zero-length GC; Kind remains available so collectors can preserve
// per-kind accounting without reclassifying or losing that zero-length case.
type structDecodeResult struct {
	Value abstractStruct
	Kind  decodedStructKind
}

// structDecodeStream owns client-block framing and per-struct progress for one
// update. UpdateDecoderV1 and UpdateDecoderV2 retain their wire-specific state;
// this stream begins above that interface and shares only the Yjs struct grammar.
type structDecodeStream struct {
	decoder updateDecoder
	grammar structDecoder

	lenientMissingHeader  bool
	useStringContentArena bool
	useAnyContentArena    bool
	useFormatContentArena bool

	headerRead      bool
	blocksRemaining Number
	block           structDecodeBlock
	blockRead       Number
	blockActive     bool
	clock           Number
	guard           stallGuard
	done            bool
	err             error

	totalInput     int
	structCap      uint64
	structsDecoded uint64

	contentReaders     []func(updateDecoder) (itemContent, error)
	contentArenasReady bool
}

func newStructDecodeStream(decoder updateDecoder, options structDecodeStreamOptions) structDecodeStream {
	var itemArena *lazyStructBlockArena[itemStruct]
	if options.useItemArena {
		// Sixty-four Items bound the unused tail to about 9 KiB per reader on
		// 64-bit systems.
		itemArena = &lazyStructBlockArena[itemStruct]{}
	}
	totalInput := decoderRemaining(decoder)
	return structDecodeStream{
		decoder: decoder,
		grammar: structDecoder{
			decoder:   decoder,
			mode:      options.mode,
			doc:       options.doc,
			lazyItems: itemArena,
		},
		lenientMissingHeader:  options.lenientMissingHeader,
		useStringContentArena: options.useStringContentArena,
		useAnyContentArena:    options.useAnyContentArena,
		useFormatContentArena: options.useFormatContentArena,
		totalInput:            totalInput,
		structCap:             structCountCap(totalInput),
		contentReaders:        contentRefs,
	}
}

func (s *structDecodeStream) fail(err error) error {
	s.done = true
	if s.err == nil {
		s.err = err
	}
	return s.err
}

func (s *structDecodeStream) activateContentArenas(declaredCount Number) {
	if s.contentArenasReady || declaredCount < 32 ||
		(!s.useStringContentArena && !s.useAnyContentArena && !s.useFormatContentArena) {
		return
	}

	readers := append([]func(updateDecoder) (itemContent, error){}, contentRefs...)
	arenas := &lazyContentBlockArenas{
		stringArena: lazyStructBlockArena[contentString]{max: lazyContentBlockMax},
		anyArena:    lazyStructBlockArena[contentAny]{max: lazyContentBlockMax},
		formatArena: lazyStructBlockArena[contentFormat]{max: lazyFormatContentBlockMax},
	}
	if s.useStringContentArena && len(readers) > refContentString {
		readers[refContentString] = func(decoder updateDecoder) (itemContent, error) {
			str, err := decoder.readStringValue()
			if err != nil {
				return nil, err
			}
			content := arenas.stringArena.alloc()
			*content = contentString{value: str}
			return content, nil
		}
	}
	if s.useAnyContentArena && len(readers) > refContentAny {
		readers[refContentAny] = func(decoder updateDecoder) (itemContent, error) {
			length, err := decoder.readLength()
			if err != nil {
				return nil, err
			}
			var values ArrayAny
			for i := 0; i < length; i++ {
				value, err := decoder.readAnyValue()
				if err != nil {
					return nil, err
				}
				values = append(values, value)
			}
			// Only the wrapper is arena-backed. Arr keeps its independently
			// allocated element storage exactly as ReadContentAny did.
			content := arenas.anyArena.alloc()
			*content = contentAny{arr: values}
			return content, nil
		}
	}
	if s.useFormatContentArena && len(readers) > refContentFormat {
		readers[refContentFormat] = func(decoder updateDecoder) (itemContent, error) {
			key, err := decoder.readKey()
			if err != nil {
				return nil, err
			}
			value, err := decoder.readJSONValue()
			if err != nil {
				return nil, err
			}
			content := arenas.formatArena.alloc()
			*content = contentFormat{key: key, value: value}
			return content, nil
		}
	}
	s.contentReaders = readers
	s.contentArenasReady = true
}

func (s *structDecodeStream) reserveEagerIDs(declaredCount Number) uint64 {
	if s.grammar.mode != structDecodeEager {
		return 0
	}
	const maxPrealloc = 1 << 20
	prealloc := uint64(declaredCount)
	if prealloc > maxPrealloc {
		prealloc = maxPrealloc
	}
	if prealloc > s.structCap {
		prealloc = s.structCap
	}
	// AND by what is actually left to read. The two clamps above are constants —
	// maxPrealloc is fixed, and structCap allows 512 structs per input byte, which
	// is a sound ceiling on how many structs a whole update may DECODE but a
	// catastrophic one for what a single block may RESERVE: it permits 8KB of
	// reservation per input byte, so a ~1KB update can ask for megabytes.
	//
	// The honest bound is information-theoretic. Every decoded struct consumes at
	// least its info byte, so a block can hold no more structs than there are
	// bytes remaining. That leaves valid input untouched — a well-formed block
	// declaring N structs is always followed by at least N bytes — while removing
	// the gap a malformed count opens between what is claimed and what could
	// possibly follow.
	//
	// Found by FuzzApplyUpdate: a 4,055-byte input reserved 8.65MB across 43
	// objects, and halving the input did not reduce it, which is the signature of
	// a length prefix rather than of real content.
	if remaining, ok := s.decoder.(interface{ RemainingLen() int }); ok {
		if left := remaining.RemainingLen(); left >= 0 && prealloc > uint64(left) {
			prealloc = uint64(left)
		}
	}
	if reserver, ok := s.decoder.(interface{ reserveIDs(uint64) }); ok {
		reserver.reserveIDs(prealloc)
	}
	return prealloc
}

// NextBlock reads the next client-block header. The active block must be fully
// consumed first; this keeps block boundaries observable to migration tests and
// prevents an adapter from silently discarding declared structs.
func (s *structDecodeStream) NextBlock() (structDecodeBlock, bool, error) {
	if s.err != nil {
		return structDecodeBlock{}, false, s.err
	}
	if s.done {
		return structDecodeBlock{}, false, nil
	}
	if s.blockActive {
		return structDecodeBlock{}, false, s.fail(fmt.Errorf(
			"advance client block with %d of %d structs unread",
			s.block.DeclaredCount-s.blockRead, s.block.DeclaredCount,
		))
	}

	if !s.headerRead {
		value, err := readVarUintAny(s.decoder.restDecoder())
		if err != nil {
			if s.lenientMissingHeader {
				s.done = true
				return structDecodeBlock{}, false, nil
			}
			return structDecodeBlock{}, false, s.fail(fmt.Errorf("number of state updates: %w", err))
		}
		count, err := toNumber(value.(uint64))
		if err != nil {
			return structDecodeBlock{}, false, s.fail(fmt.Errorf("number of state updates: %w", err))
		}
		s.blocksRemaining = count
		s.headerRead = true
	}
	if s.blocksRemaining == 0 {
		s.done = true
		return structDecodeBlock{}, false, nil
	}

	declaredCount, err := readVarUintAsNumber(s.decoder.restDecoder())
	if err != nil {
		return structDecodeBlock{}, false, s.fail(fmt.Errorf("number of structs: %w", err))
	}
	if int64(declaredCount) > maxYjsClientStructCount {
		return structDecodeBlock{}, false, s.fail(fmt.Errorf(
			"number of structs: %d exceeds JavaScript array length limit %d",
			declaredCount, maxYjsClientStructCount,
		))
	}
	// Tiny blocks do not contain enough wrappers to repay a private dispatch
	// table. Activation remains before client/clock reads, exactly where the lazy
	// parser made the allocation decision before this extraction.
	s.activateContentArenas(declaredCount)
	client, err := s.decoder.readClient()
	if err != nil {
		return structDecodeBlock{}, false, s.fail(fmt.Errorf("client: %w", err))
	}
	// The eager decoder reserves IDs after reading the client but before reading
	// the base clock. Keeping that boundary makes malformed-input allocation and
	// every valid block's exact reserve identical to the former eager loop.
	reserveCount := s.reserveEagerIDs(declaredCount)
	clock, err := readVarUintAsNumber(s.decoder.restDecoder())
	if err != nil {
		return structDecodeBlock{}, false, s.fail(fmt.Errorf("clock: %w", err))
	}

	s.blocksRemaining--
	s.block = structDecodeBlock{
		Client: client, StartClock: clock, DeclaredCount: declaredCount, ReserveCount: reserveCount,
	}
	s.blockRead = 0
	s.blockActive = declaredCount != 0
	s.clock = clock
	s.guard = stallGuard{decoder: s.decoder, max: maxStallIterations}
	return s.block, true, nil
}

// NextStruct consumes one declared struct under the lazy yielding policy.
// consumed is true even for a zero-length GC, which is valid framing but has a
// nil Value. Eager collection uses collectEagerBlock: keeping its loop bulked
// avoids a non-inlinable call per decoded struct.
func (s *structDecodeStream) NextStruct() (result structDecodeResult, consumed bool, err error) {
	if s.grammar.mode != structDecodeLazy {
		return structDecodeResult{}, false, s.fail(fmt.Errorf("single-struct advance requires lazy decode mode"))
	}
	if s.err != nil {
		return structDecodeResult{}, false, s.err
	}
	if !s.blockActive {
		return structDecodeResult{}, false, nil
	}

	s.structsDecoded++
	if s.structsDecoded > s.structCap {
		return structDecodeResult{}, false, s.fail(fmt.Errorf(
			"decoded struct count exceeds cap %d for %d input bytes (corrupt struct count / amplification DoS)",
			s.structCap, s.totalInput,
		))
	}

	beforeRemaining, beforeClock := s.guard.snapshot(s.clock)
	info, err := s.decoder.readInfo()
	if err != nil {
		return structDecodeResult{}, false, s.fail(fmt.Errorf("info: %w", err))
	}
	value, nextClock, kind, err := s.grammar.decode(
		info, s.block.Client, s.clock, nil, s.contentReaders,
	)
	if err != nil {
		return structDecodeResult{}, false, s.fail(err)
	}

	s.clock = nextClock
	s.blockRead++
	if s.blockRead == s.block.DeclaredCount {
		s.blockActive = false
	}
	if value == nil {
		if !s.guard.progressed(beforeRemaining, beforeClock, s.clock) {
			return structDecodeResult{}, false, s.fail(fmt.Errorf("struct decode stalled: corrupt struct count"))
		}
	} else {
		// The former lazy generator allocated a fresh guard after every yielded
		// value. Resetting here preserves that boundary while the guard now lives
		// for the stream's whole client block.
		s.guard.stalled = 0
	}
	return structDecodeResult{Value: value, Kind: kind}, true, nil
}

// collectEagerBlock drains the active block into refs in one call. Keeping the
// eager loop inside the concrete stream avoids a non-inlinable method call per
// struct (about 2-3% on ApplyV1) while the stream still owns framing, bounds,
// progress and the shared grammar. Lazy remains one-at-a-time because yielding
// each value is its API and its stall reset boundary.
func (s *structDecodeStream) collectEagerBlock(
	eagerStrings *decodedStringItemArena,
	refs *clientStructRef,
) (gcCount, skipCount, itemCount int, err error) {
	if s.grammar.mode != structDecodeEager || eagerStrings == nil || !s.blockActive {
		return 0, 0, 0, s.fail(fmt.Errorf("collect eager block without eager policy or active block"))
	}
	for s.blockActive {
		s.structsDecoded++
		if s.structsDecoded > s.structCap {
			return gcCount, skipCount, itemCount, s.fail(fmt.Errorf(
				"decoded struct count exceeds cap %d for %d input bytes (corrupt struct count / amplification DoS)",
				s.structCap, s.totalInput,
			))
		}

		beforeRemaining, beforeClock := s.guard.snapshot(s.clock)
		info, readErr := s.decoder.readInfo()
		if readErr != nil {
			return gcCount, skipCount, itemCount, s.fail(fmt.Errorf("info: %w", readErr))
		}
		value, nextClock, kind, decodeErr := s.grammar.decode(
			info, s.block.Client, s.clock, eagerStrings, s.contentReaders,
		)
		if decodeErr != nil {
			return gcCount, skipCount, itemCount, s.fail(decodeErr)
		}

		s.clock = nextClock
		s.blockRead++
		if s.blockRead == s.block.DeclaredCount {
			s.blockActive = false
		}
		switch kind {
		case decodedStructGC:
			gcCount++
		case decodedStructSkip:
			skipCount++
		case decodedStructItem:
			itemCount++
		}
		if value != nil {
			refs.refs = append(refs.refs, value)
		}
		// Preserve the former eager collector's partial-result boundary: it
		// appended the decoded value before rejecting a zero-progress run.
		if !s.guard.progressed(beforeRemaining, beforeClock, s.clock) {
			return gcCount, skipCount, itemCount, s.fail(fmt.Errorf("struct decode stalled: corrupt struct count"))
		}
	}
	return gcCount, skipCount, itemCount, nil
}

type lazyStructCursor struct {
	stream structDecodeStream
	err    error
	done   bool
}

func (c *lazyStructCursor) fail(err error) {
	c.done = true
	if c.err == nil {
		c.err = fmt.Errorf("lazy struct reader: %w", err)
	}
}

func (c *lazyStructCursor) Err() error { return c.err }

func (c *lazyStructCursor) Next() abstractStruct {
	if c.done {
		return nil
	}
	for {
		if !c.stream.blockActive {
			_, ok, err := c.stream.NextBlock()
			if err != nil {
				c.fail(err)
				return nil
			}
			if !ok {
				c.done = true
				return nil
			}
			if !c.stream.blockActive {
				continue
			}
		}

		result, consumed, err := c.stream.NextStruct()
		if err != nil {
			c.fail(err)
			return nil
		}
		if !consumed {
			continue
		}
		if result.Value != nil {
			return result.Value
		}
	}
}

// ---------------------------------------------------------------- from struct_decoder.go
// structDecodeMode selects only the policies that genuinely differ between
// eager apply and lazy update utilities. The wire grammar below is shared.
// Keep this concrete: an interface dispatch on every struct is measurable on
// large ApplyUpdate calls.
type structDecodeMode uint8

const (
	structDecodeEager structDecodeMode = iota
	structDecodeLazy
)

type decodedStructKind uint8

const (
	decodedStructGC decodedStructKind = iota
	decodedStructSkip
	decodedStructItem
)

// structDecoder owns the allocation and parent-resolution policy around one
// shared struct grammar. Its fields point at the same per-update arenas the two
// original loops owned, so extracting the grammar does not widen their lifetime.
type structDecoder struct {
	decoder   updateDecoder
	mode      structDecodeMode
	doc       *Doc
	lazyItems *lazyStructBlockArena[itemStruct]
}

// decode reads the fields after info and returns the decoded struct plus the
// next client clock. A zero-length GC is valid but yields nil; callers retain
// their existing stall and block-loop policies around that result.
func (d structDecoder) decode(
	info uint8,
	client, clock Number,
	eagerStrings *decodedStringItemArena,
	contentReaders []func(updateDecoder) (itemContent, error),
) (abstractStruct, Number, decodedStructKind, error) {
	refID := int(info & bits5)
	switch refID {
	case structGCRefNumber:
		length, err := d.decoder.readLength()
		if err != nil {
			return nil, clock, decodedStructGC, fmt.Errorf("gc length: %w", err)
		}
		if length == 0 {
			return nil, clock, decodedStructGC, nil
		}
		nextClock, err := addClock(clock, length)
		if err != nil {
			return nil, clock, decodedStructGC, fmt.Errorf("gc clock advance: %w", err)
		}
		return newGC(GenID(client, clock), length), nextClock, decodedStructGC, nil

	case structSkipRefNumber:
		length, err := readVarUintAsNumber(d.decoder.restDecoder())
		if err != nil {
			return nil, clock, decodedStructSkip, fmt.Errorf("skip length: %w", err)
		}
		nextClock, err := addClock(clock, length)
		if err != nil {
			return nil, clock, decodedStructSkip, fmt.Errorf("skip clock advance: %w", err)
		}
		return newSkip(GenID(client, clock), length), nextClock, decodedStructSkip, nil
	}

	cantCopyParentInfo := info&(bit7|bit8) == 0
	var origin *ID
	if info&bit8 != 0 {
		value, err := d.decoder.readLeftID()
		if err != nil {
			return nil, clock, decodedStructItem, fmt.Errorf("origin (left id): %w", err)
		}
		origin = value
	}

	var rightOrigin *ID
	if info&bit7 != 0 {
		value, err := d.decoder.readRightID()
		if err != nil {
			return nil, clock, decodedStructItem, fmt.Errorf("right origin (right id): %w", err)
		}
		rightOrigin = value
	}

	var parent interface{}
	if cantCopyParentInfo {
		hasParentKey, err := d.decoder.readParentInfo()
		if err != nil {
			return nil, clock, decodedStructItem, fmt.Errorf("parent info: %w", err)
		}
		if hasParentKey {
			name, err := d.decoder.readStringValue()
			if err != nil {
				return nil, clock, decodedStructItem, fmt.Errorf("parent key: %w", err)
			}
			if d.mode == structDecodeEager {
				if d.doc == nil {
					return nil, clock, decodedStructItem, errors.New("parent key: eager decoder has no document")
				}
				parent = d.doc.getGeneric(name)
			} else {
				parent = newYString(name)
			}
		} else {
			value, err := d.decoder.readLeftID()
			if err != nil {
				return nil, clock, decodedStructItem, fmt.Errorf("parent (left id): %w", err)
			}
			parent = value
		}
	}

	var parentSub string
	if cantCopyParentInfo && info&bit6 != 0 {
		value, err := d.decoder.readStringValue()
		if err != nil {
			return nil, clock, decodedStructItem, fmt.Errorf("parent sub: %w", err)
		}
		parentSub = value
	}
	if refID >= len(contentReaders) {
		err := fmt.Errorf("read item content failed: info %d refID %d out of range", info, refID)
		return nil, clock, decodedStructItem, fmt.Errorf("item content: %w", err)
	}

	var item *itemStruct
	if d.mode == structDecodeEager && refID == refContentString {
		str, err := d.decoder.readStringValue()
		if err != nil {
			return nil, clock, decodedStructItem, fmt.Errorf("item content: %w", err)
		}
		length := len(str)
		if !isASCIIText(str) {
			str, length = normalizeNonASCIITextUTF8WithLength(str)
		}
		if eagerStrings == nil {
			return nil, clock, decodedStructItem, errors.New("item content: eager string arena is nil")
		}
		storage := eagerStrings.alloc(str)
		item = initItemWithLength(
			&storage.item, GenID(client, clock), nil, origin, nil, rightOrigin,
			parent, parentSub, &storage.content, length,
		)
	} else {
		content, err := contentReaders[refID](d.decoder)
		if err != nil {
			return nil, clock, decodedStructItem, fmt.Errorf("item content (info %d): %w", info, err)
		}
		if d.mode == structDecodeLazy && d.lazyItems != nil {
			item = initItemWithLength(
				d.lazyItems.alloc(), GenID(client, clock), nil, origin, nil,
				rightOrigin, parent, parentSub, content, content.contentLength(),
			)
		} else {
			item = newItem(
				GenID(client, clock), nil, origin, nil, rightOrigin,
				parent, parentSub, content,
			)
		}
	}
	if item == nil {
		return nil, clock, decodedStructItem, errors.New("item content produced nil item")
	}

	nextClock, err := addClock(clock, item.length)
	if err != nil {
		return nil, clock, decodedStructItem, fmt.Errorf("item clock advance: %w", err)
	}
	return item, nextClock, decodedStructItem, nil
}
