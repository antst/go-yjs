package crdt

import (
	"container/heap"
	"fmt"
	"strings"
)

const (
	recordPositionUnit = 1024
)

type lazyWriteState struct {
	s      abstractStruct
	offset Number
}

type lazyStructReader struct {
	gen         func() abstractStruct
	curr        abstractStruct
	done        bool
	filterSkips bool
	cursor      *lazyStructCursor
}

// decodeError returns the decode error recorded by the stream cursor, or nil. Valid to
// call at any time; meaningful once the reader has advanced past the malformed
// struct (typically after the consume loop).
func (r *lazyStructReader) decodeError() error {
	if r.cursor == nil {
		return nil
	}
	return r.cursor.Err()
}

func (r *lazyStructReader) nextStruct() abstractStruct {
	// ignore "Skip" structs
	r.curr = r.gen()
	for r.filterSkips && r.curr != nil && isSameType(r.curr, &skipStruct{}) {
		r.curr = r.gen()
	}

	return r.curr
}

type positionInfo struct {
	clock     Number
	startByte int
	structNo  int
}

type clientStruct struct {
	written      Number
	restEncoder  []uint8
	positionList []positionInfo
	client       Number
	start        int
	end          int
}

type lazyStructWriter struct {
	currClient Number
	written    Number
	encoder    updateEncoder

	// We want to write operations lazily, but also we need to know beforehand how many operations we want to write for each client.
	//
	//  This kind of meta-information (#clients, #structs-per-client-written) is written to the restEncoder.
	//
	//  We fragment the restEncoder and store a slice of it per-client until we know how many clients there are.
	//  When we flush (toUint8Array) we write the restEncoder using the fragments and the meta-information.
	clientStructs      []clientStruct
	needRecordPosition bool
	positionList       []positionInfo
}

func newLazyStructReader(decoder updateDecoder, filterSkips bool) *lazyStructReader {
	if arena, ok := decoder.(interface{ enableLazyIDArena() }); ok {
		arena.enableLazyIDArena()
	}
	useItemArena := true
	if _, disabled := decoder.(interface{ disableLazyItemArena() }); disabled {
		useItemArena = false
	}
	useStringContentArena := true
	if _, disabled := decoder.(interface{ disableLazyStringContentArena() }); disabled {
		useStringContentArena = false
	}
	useAnyContentArena := true
	if _, disabled := decoder.(interface{ disableLazyAnyContentArena() }); disabled {
		useAnyContentArena = false
	}
	useFormatContentArena := true
	if _, disabled := decoder.(interface{ disableLazyFormatContentArena() }); disabled {
		useFormatContentArena = false
	}
	cursor := &lazyStructCursor{
		stream: newStructDecodeStream(decoder, structDecodeStreamOptions{
			mode:                  structDecodeLazy,
			lenientMissingHeader:  true,
			useItemArena:          useItemArena,
			useStringContentArena: useStringContentArena,
			useAnyContentArena:    useAnyContentArena,
			useFormatContentArena: useFormatContentArena,
		}),
	}
	r := &lazyStructReader{
		gen:         cursor.Next,
		filterSkips: filterSkips,
		done:        false,
		cursor:      cursor,
	}

	r.nextStruct()
	return r
}

func newLazyStructWriter(encoder updateEncoder) *lazyStructWriter {
	return &lazyStructWriter{
		encoder: encoder,
	}
}

func mergeUpdatesWith(updates [][]uint8, YDecoder func([]byte) updateDecoder, YEncoder func() updateEncoder) ([]uint8, error) {
	return mergeUpdatesCore(updates, YDecoder, YEncoder)
}

// MergeUpdates merges V1 updates into one canonical V1 update.
func MergeUpdates(updates [][]byte) ([]byte, error) {
	return mergeUpdatesCore(updates, newDecoderV1, newEncoderV1)
}

// MergeUpdatesV2 merges V2 updates into one canonical V2 update.
func MergeUpdatesV2(updates [][]byte) ([]byte, error) {
	return mergeUpdatesCore(updates, newDecoderV2, newEncoderV2)
}

// encodeStateVectorFromUpdateWith returns an error so a cap-breach / decode
// error in the lazy reader surfaces instead of yielding a SILENTLY-TRUNCATED
// state vector (which would corrupt sync — the peer would believe it holds more
// state than it does and skip structs it actually needs). The state vector is
// emitted only after the reader is confirmed clean via reader.Err().
func encodeStateVectorFromUpdateWith(update []uint8, YEncoder func() updateEncoder, YDecoder func([]byte) updateDecoder) ([]uint8, error) {
	encoder := YEncoder()
	updateDecoder := newLazyStructReader(YDecoder(update), false)
	curr := updateDecoder.curr
	if curr != nil {
		size := 0
		currClient := curr.getID().Client
		stopCounting := curr.getID().Clock != 0 // must start at 0
		var currClock Number
		if !stopCounting {
			currClock = curr.getID().Clock + curr.structLength()
		}

		for ; curr != nil; curr = updateDecoder.nextStruct() {
			if currClient != curr.getID().Client {
				if currClock != 0 {
					size++
					// We found a new client
					// write what we have to the encoder
					writeVarUint(encoder.restEncoder(), uint64(currClient))
					writeVarUint(encoder.restEncoder(), uint64(currClock))

				}

				currClient = curr.getID().Client
				currClock = 0
				stopCounting = curr.getID().Clock != 0
			}

			// we ignore skips
			if isSameType(curr, &skipStruct{}) {
				stopCounting = true
			}

			if !stopCounting {
				currClock = curr.getID().Clock + curr.structLength()
			}
		}

		// write what we have
		if currClock != 0 {
			size++
			writeVarUint(encoder.restEncoder(), uint64(currClient))
			writeVarUint(encoder.restEncoder(), uint64(currClock))
		}

		// The reader may have aborted early on a struct-count-cap breach or a
		// field-decode error; surface it rather than emit a state vector that
		// silently omits the un-read tail.
		if err := updateDecoder.decodeError(); err != nil {
			return nil, fmt.Errorf("encode state vector from update: %w", err)
		}

		// prepend the size of the state vector
		enc := newUpdateEncoderV1()
		writeVarUint(enc.rest, uint64(size))
		writeUint8Array(enc.rest, encoder.restEncoder().Bytes())
		return enc.toBytes(), nil
	} else {
		// The state vector is ALWAYS V1-encoded (per the Yjs spec — only update
		// payloads and delete sets have V2 variants). The non-empty branch above
		// re-wraps in NewUpdateEncoderV1() for exactly this reason; the empty branch
		// must too. Returning `encoder` directly here would, for the V2 variant
		// (EncodeStateVectorFromUpdateV2), emit the 12-byte V2 columnar envelope
		// instead of the 1-byte V1 state vector [0], so a consumer decoding via
		// decodeStateVector would misread the framing.
		// curr==nil can mean a genuinely empty update OR a reader that aborted on
		// the very first struct (cap breach / malformed header). Distinguish them:
		// only a clean-empty update yields the canonical 1-byte [0] state vector.
		if err := updateDecoder.decodeError(); err != nil {
			return nil, fmt.Errorf("encode state vector from update: %w", err)
		}
		enc := newUpdateEncoderV1()
		writeVarUint(enc.rest, 0)
		return enc.toBytes(), nil
	}
}

func encodeStateVectorFromUpdate(update []uint8) ([]uint8, error) {
	return encodeStateVectorFromUpdateWith(update, newEncoderV1, newDecoderV1)
}

// EncodeStateVectorFromUpdate extracts the state vector from a V1-encoded
// update without materializing a document.
func EncodeStateVectorFromUpdate(update []uint8) ([]uint8, error) {
	return encodeStateVectorFromUpdate(update)
}

// encodeStateVectorFromUpdateV2 extracts the state vector from a V2-encoded
// update. The returned state vector is itself always V1-encoded (per the Yjs
// spec — only update payloads and delete sets have V2 variants).
func encodeStateVectorFromUpdateV2(update []uint8) ([]uint8, error) {
	return encodeStateVectorFromUpdateWith(update, newEncoderV2, newDecoderV2)
}

// EncodeStateVectorFromUpdateV2 extracts the state vector from a V2-encoded
// update without materializing a document.
func EncodeStateVectorFromUpdateV2(update []uint8) ([]uint8, error) {
	return encodeStateVectorFromUpdateV2(update)
}

// parseUpdateMetaWith returns an error so a cap-breach / decode error in the
// lazy reader surfaces instead of yielding SILENTLY-TRUNCATED from/to meta maps
// (which would misreport the update's clock ranges on malicious input).
func parseUpdateMetaWith(update []uint8, YDecoder func([]byte) updateDecoder) (map[Number]Number, map[Number]Number, error) {
	from := make(map[Number]Number)
	to := make(map[Number]Number)

	updateDecoder := newLazyStructReader(YDecoder(update), false)
	curr := updateDecoder.curr

	if curr != nil {
		currClient := curr.getID().Client
		currClock := curr.getID().Clock
		// write the beginning to `from`
		from[currClient] = currClock
		for ; curr != nil; curr = updateDecoder.nextStruct() {
			if currClient != curr.getID().Client {
				// We found a new client
				// write the end to `to`
				to[currClient] = currClock
				// write the beginning to `from`
				from[curr.getID().Client] = curr.getID().Clock
				currClient = curr.getID().Client
			}
			currClock = curr.getID().Clock + curr.structLength()
		}
		// write the end to `to`
		to[currClient] = currClock
	}
	// Surface a reader abort (cap breach / malformed struct) rather than return
	// partial meta that silently omits the un-read tail of the update.
	if err := updateDecoder.decodeError(); err != nil {
		return nil, nil, fmt.Errorf("parse update meta: %w", err)
	}
	return from, to, nil
}

func parseUpdateMeta(update []uint8) (map[Number]Number, map[Number]Number, error) {
	return parseUpdateMetaWith(update, newDecoderV1)
}

// ParseUpdateMeta returns the state-vector range covered by a V1 update
// without materializing a document.
func ParseUpdateMeta(update []uint8) (map[Number]Number, map[Number]Number, error) {
	return parseUpdateMeta(update)
}

func sliceStruct(left abstractStruct, diff Number) abstractStruct {
	client, clock := left.getID().Client, left.getID().Clock

	if isSameType(left, &gcStruct{}) {
		return newGC(GenID(client, clock+diff), left.structLength()-diff)
	}

	if isSameType(left, &skipStruct{}) {
		return newSkip(GenID(client, clock+diff), left.structLength()-diff)
	}

	leftItem := left.(*itemStruct)
	originID := GenID(client, clock+diff-1)
	// Pass the parent through UNCHANGED rather than asserting to IAbstractType. In this
	// lazy-struct path the item is never integrated, so Parent may still be an unresolved *ID.
	// Before ID was made compact the assertion happened to succeed (ID embedded AbstractType), so
	// the *ID survived by accident; now it would silently become nil and the split item would lose
	// its parent. NewItem takes interface{}, matching Item.Parent, so no assertion is needed.
	parent := leftItem.parent
	rightLength := leftItem.length - diff
	rightContent := spliceContentWithLength(leftItem.content, diff, leftItem.length)
	return newItemWithLength(
		GenID(client, clock+diff),
		nil,
		&originID,
		nil,
		leftItem.rightOrigin,
		parent,
		leftItem.parentSub,
		rightContent,
		rightLength,
	)
}

type lazyStructReaderHeapEntry struct {
	reader *lazyStructReader
	// tie preserves the old stable insertion rule for equal client/clock/type
	// priorities. A reader requeued after advancing receives a smaller tie so it
	// stays ahead of a reader it has just caught up with.
	tie int64
}

type lazyStructReaderHeap []*lazyStructReaderHeapEntry

func (h lazyStructReaderHeap) Len() int { return len(h) }

func (h lazyStructReaderHeap) Less(i, j int) bool {
	leftEntry := h[i]
	rightEntry := h[j]
	left := leftEntry.reader.curr
	right := rightEntry.reader.curr
	leftID := left.getID()
	rightID := right.getID()
	if leftID.Client != rightID.Client {
		return leftID.Client > rightID.Client
	}
	if leftID.Clock != rightID.Clock {
		return leftID.Clock < rightID.Clock
	}
	_, leftSkip := left.(*skipStruct)
	_, rightSkip := right.(*skipStruct)
	if leftSkip != rightSkip {
		return !leftSkip
	}
	return leftEntry.tie < rightEntry.tie
}

func (h lazyStructReaderHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *lazyStructReaderHeap) Push(value interface{}) {
	*h = append(*h, value.(*lazyStructReaderHeapEntry))
}

func (h *lazyStructReaderHeap) Pop() interface{} {
	old := *h
	last := len(old) - 1
	value := old[last]
	old[last] = nil
	*h = old[:last]
	return value
}

func mergeUpdatesCore(updates [][]uint8, YDecoder func([]byte) updateDecoder, YEncoder func() updateEncoder) ([]uint8, error) {
	// Strict decoding is always enforced now (the stopIfError flag and its
	// panic-vs-error switch were removed): even a single update is run through the
	// lazy struct reader so a malformed/DoS payload surfaces via its Err() instead
	// of being passed through verbatim.
	updateDecoders := make([]updateDecoder, 0, len(updates))
	allReaders := make([]*lazyStructReader, 0, len(updates))
	// Entries live in one fixed-size block so the heap can store their addresses
	// without allocating one object per reader. Go would keep old backing arrays
	// alive if this grew, so growth is safe but would forfeit the allocation bound.
	schedulerEntries := make([]lazyStructReaderHeapEntry, len(updates))
	scheduler := make(lazyStructReaderHeap, 0, len(updates))
	for i, update := range updates {
		decoder := YDecoder(update)
		updateDecoders = append(updateDecoders, decoder)
		reader := newLazyStructReader(decoder, true)
		allReaders = append(allReaders, reader)
		schedulerEntries[i] = lazyStructReaderHeapEntry{reader: reader, tie: int64(i)}
		if reader.curr != nil {
			scheduler = append(scheduler, &schedulerEntries[i])
		}
	}

	// DoS 1: the merge loop below removes a reader from the scheduler once it is
	// drained, so keep a stable list of every reader to inspect for a
	// struct-count-cap breach after the merge. checkLazyCap surfaces the first
	// such error so a corrupt/amplification-DoS V2 update is rejected instead of
	// having silently materialized structs toward an attacker-supplied count.
	checkLazyCap := func() error {
		for _, r := range allReaders {
			if err := r.decodeError(); err != nil {
				return err
			}
		}
		return nil
	}

	// todo we don't need offset because we always slice before
	var currWrite *lazyWriteState
	// Use the caller-supplied encoder factory so the merged output is emitted in
	// the requested wire format. Hardcoding V1 here silently downgraded
	// EncodeStateAsUpdateV2's pending-update merge path to V1.
	updateEncoder := YEncoder()
	// write structs lazily
	lazyStructEncoder := newLazyStructWriter(updateEncoder)

	// writeErr captures the first content-encode failure from any
	// writeStructToLazyStructWriter below. writeStruct records it and reports
	// whether the caller should bail; a content-encode failure must surface as an
	// error from the merge, never a silently-truncated update.
	var writeErr error
	writeStruct := func(s abstractStruct, offset Number) bool {
		if writeErr != nil {
			return true
		}
		writeErr = writeStructToLazyStructWriter(lazyStructEncoder, s, offset)
		return writeErr != nil
	}

	// Note: We need to ensure that all lazyStructDecoders are fully consumed
	// Note: Should merge document updates whenever possible - even from different updates
	// Note: Should handle that some operations cannot be applied yet ()

	heap.Init(&scheduler)
	nextTie := int64(-1)
	requeue := func(entry *lazyStructReaderHeapEntry) {
		if entry.reader.curr != nil {
			entry.tie = nextTie
			nextTie--
			heap.Push(&scheduler, entry)
		}
	}

	for scheduler.Len() > 0 {
		entry := heap.Pop(&scheduler).(*lazyStructReaderHeapEntry)
		currDecoder := entry.reader
		// write from currDecoder until the next operation is from another client or if filler-struct
		// then we need to reorder the decoders and find the next operation to write
		firstClient := currDecoder.curr.getID().Client
		if currWrite != nil {
			curr := currDecoder.curr
			iterated := false

			// iterate until we find something that we haven't written already
			// remember: first the high client-ids are written
			for curr != nil &&
				curr.getID().Clock+curr.structLength() <= currWrite.s.getID().Clock+currWrite.s.structLength() &&
				curr.getID().Client >= currWrite.s.getID().Client {

				curr = currDecoder.nextStruct()
				iterated = true
			}

			if curr == nil || // current decoder is empty
				curr.getID().Client != firstClient || // check whether there is another decoder that has has updates from `firstClient`
				(iterated && curr.getID().Clock > currWrite.s.getID().Clock+currWrite.s.structLength()) { // the above while loop was used and we are potentially missing updates
				requeue(entry)
				continue
			}

			if firstClient != currWrite.s.getID().Client {
				if writeStruct(currWrite.s, currWrite.offset) {
					break
				}
				currWrite = &lazyWriteState{
					s:      curr,
					offset: 0,
				}

				currDecoder.nextStruct()
			} else {
				if currWrite.s.getID().Clock+currWrite.s.structLength() < curr.getID().Clock {
					// todo write currStruct & set currStruct = Skip(clock = currStruct.id.clock + currStruct.length, length = curr.id.clock - self.clock)
					if isSameType(currWrite.s, &skipStruct{}) {
						// extend existing skip
						currWrite.s.setStructLength(curr.getID().Clock + curr.structLength() - currWrite.s.getID().Clock)
					} else {
						if writeStruct(currWrite.s, currWrite.offset) {
							break
						}
						diff := curr.getID().Clock - currWrite.s.getID().Clock - currWrite.s.structLength()
						s := newSkip(GenID(firstClient, currWrite.s.getID().Clock+currWrite.s.structLength()), diff)
						currWrite = &lazyWriteState{
							s:      s,
							offset: 0,
						}
					}
				} else { // if (currWrite.struct.id.clock + currWrite.struct.length >= curr.id.clock) {
					diff := currWrite.s.getID().Clock + currWrite.s.structLength() - curr.getID().Clock
					if diff > 0 {
						if isSameType(currWrite.s, &skipStruct{}) {
							// prefer to slice Skip because the other struct might contain more information
							currWrite.s.setStructLength(currWrite.s.structLength() - diff)
						} else {
							curr = sliceStruct(curr, diff)
						}
					}

					if !currWrite.s.mergeStructWith(curr) {
						if writeStruct(currWrite.s, currWrite.offset) {
							break
						}
						currWrite = &lazyWriteState{
							s:      curr,
							offset: 0,
						}
						currDecoder.nextStruct()
					}
				}
			}
		} else {
			currWrite = &lazyWriteState{
				s:      currDecoder.curr,
				offset: 0,
			}
			currDecoder.nextStruct()
		}

		for next := currDecoder.curr; next != nil && next.getID().Client == firstClient &&
			next.getID().Clock == currWrite.s.getID().Clock+currWrite.s.structLength() &&
			!isSameType(next, &skipStruct{}); next = currDecoder.nextStruct() {
			if writeStruct(currWrite.s, currWrite.offset) {
				break
			}
			currWrite = &lazyWriteState{
				s:      next,
				offset: 0,
			}
		}
		if writeErr != nil {
			break
		}
		requeue(entry)
	}
	if writeErr != nil {
		return nil, writeErr
	}
	// DoS 1: a lazy reader that hit the struct-count cap stopped early; surface
	// that as a hard error rather than emit a partial/misleading merge.
	if err := checkLazyCap(); err != nil {
		return nil, fmt.Errorf("merge updates: %w", err)
	}
	if currWrite != nil {
		if err := writeStructToLazyStructWriter(lazyStructEncoder, currWrite.s, currWrite.offset); err != nil {
			return nil, err
		}
		currWrite = nil
	}

	finishLazyStructWriting(lazyStructEncoder)

	dss := make([]*deleteSet, 0, len(updateDecoders))
	for _, decoder := range updateDecoders {
		// A truncated/malformed delete set now errors instead of returning a
		// silent nil that MergeDeleteSets would skip (dropping deletes).
		ds, err := readDeleteSet(decoder)
		if err != nil {
			return nil, fmt.Errorf("merge updates: read delete set: %w", err)
		}
		dss = append(dss, ds)
	}

	ds := mergeDeleteSets(dss)
	// A malformed source delete set (e.g. a V1 0-length range merged then
	// re-encoded as V2) cannot be written; fail the merge with an error rather
	// than panic, emit a corrupt stream, or return a silent nil that a caller
	// reads as "empty/success".
	if err := writeDeleteSet(updateEncoder, ds); err != nil {
		return nil, fmt.Errorf("merge updates: delete set encode failed: %w", err)
	}
	out := updateEncoder.toBytes()
	// Surface a deferred column-encode error (e.g. a clock diff overflow from a
	// hostile update merged into V2) instead of returning a truncated update.
	if err := updateEncoder.encodeError(); err != nil {
		return nil, fmt.Errorf("merge updates: encode failed: %w", err)
	}
	return out, nil
}

func generateUpdates(lazyWriter *lazyStructWriter, maxUpdateSize int) [][]uint8 {
	updates := make([][]uint8, 0)
	for {
		update := generateUpdate(lazyWriter, maxUpdateSize)
		if nil == update {
			break
		}
		updates = append(updates, update)
	}
	return updates
}

func generateUpdate(lazyWriter *lazyStructWriter, maxUpdateSize int) []uint8 {
	clientCnt := 0
	for i := 0; i < len(lazyWriter.clientStructs); i++ {
		partStructs := &lazyWriter.clientStructs[i]
		if partStructs.end < len(partStructs.positionList) {
			clientCnt++
		}
		partStructs.start = partStructs.end
	}

	if clientCnt <= 0 {
		return nil
	}

	totalSize := 0
	for {
		totalSize = 0
		isFinished := true
		for i := 0; i < len(lazyWriter.clientStructs); i++ {
			partStructs := &lazyWriter.clientStructs[i]
			if partStructs.end < len(partStructs.positionList)-1 {
				partStructs.end++
				totalSize += partStructs.positionList[partStructs.end].startByte - partStructs.positionList[partStructs.start].startByte
				isFinished = false
			} else if partStructs.start < len(partStructs.positionList) {
				totalSize += len(partStructs.restEncoder) - partStructs.positionList[partStructs.start].startByte
				if partStructs.end < len(partStructs.positionList) {
					partStructs.end++
					isFinished = false
				}
			}
		}
		if isFinished || totalSize >= maxUpdateSize {
			break
		}
	}

	// data format：update_count count/client/clock ... count/client/clock ... count/client/clock ds
	updateEncoder := newUpdateEncoderV1()
	updateEncoder.rest.Grow(totalSize + 8*(1+clientCnt*3) + 1)
	writeVarUint(updateEncoder.rest, uint64(clientCnt))
	for i := 0; i < len(lazyWriter.clientStructs); i++ {
		partStructs := lazyWriter.clientStructs[i]
		if partStructs.end <= partStructs.start {
			continue
		}
		structCnt := 0
		var data []uint8
		if partStructs.end >= len(partStructs.positionList) {
			structCnt = partStructs.written - partStructs.positionList[partStructs.start].structNo
			data = partStructs.restEncoder[partStructs.positionList[partStructs.start].startByte:]
		} else {
			structCnt = partStructs.positionList[partStructs.end].structNo - partStructs.positionList[partStructs.start].structNo
			data = partStructs.restEncoder[partStructs.positionList[partStructs.start].startByte:partStructs.positionList[partStructs.end].startByte]
		}

		writeVarUint(updateEncoder.rest, uint64(structCnt))
		updateEncoder.writeClient(partStructs.client)
		writeVarUint(updateEncoder.rest, uint64(partStructs.positionList[partStructs.start].clock))
		writeUint8Array(updateEncoder.rest, data)
	}

	return updateEncoder.rest.Bytes()
}

func diffUpdatesWith(update []uint8, sv []uint8, YDecoder func([]byte) updateDecoder, YEncoder func() updateEncoder, maxUpdateSize int) ([][]uint8, error) {
	updates := make([][]uint8, 0)

	if len(update) <= maxUpdateSize {
		updates = append(updates, update)
		return updates, nil
	}

	// An empty/nil state vector means "the remote has nothing" — diff against the
	// empty map (send everything). Normalize to the canonical [0]-client encoding
	// (matching EncodeStateAsUpdateWith) so this legitimate case decodes cleanly,
	// while a genuinely truncated non-empty sv still surfaces a decode error.
	if len(sv) == 0 {
		sv = []byte{0}
	}
	state, err := decodeStateVector(sv)
	if err != nil {
		return nil, fmt.Errorf("diff updates: decode state vector: %w", err)
	}
	encoder := YEncoder()
	lazyStructWriter := newLazyStructWriter(encoder)
	lazyStructWriter.needRecordPosition = true
	decoder := YDecoder(update)
	reader := newLazyStructReader(decoder, false)
	for reader.curr != nil {
		curr := reader.curr
		currClient := curr.getID().Client
		svClock := state[currClient]
		if isSameType(reader.curr, &skipStruct{}) {
			// the first written struct shouldn't be a skip
			reader.nextStruct()
			continue
		}

		if curr.getID().Clock+curr.structLength() > svClock {
			if err := writeStructToLazyStructWriter(lazyStructWriter, curr, maxNumber(svClock-curr.getID().Clock, 0)); err != nil {
				return nil, err
			}
			reader.nextStruct()

			for reader.curr != nil && reader.curr.getID().Client == currClient {
				if err := writeStructToLazyStructWriter(lazyStructWriter, reader.curr, 0); err != nil {
					return nil, err
				}
				reader.nextStruct()
			}
		} else {
			// read until something new comes up
			for reader.curr != nil && reader.curr.getID().Client == currClient && reader.curr.getID().Clock+reader.curr.structLength() <= svClock {
				reader.nextStruct()
			}
		}
	}

	// DoS 1: surface a struct-count-cap breach from the lazy reader rather than
	// emit a partial diff.
	if err := reader.decodeError(); err != nil {
		return nil, fmt.Errorf("diff updates: %w", err)
	}

	flushLazyStructWriter(lazyStructWriter)

	updates = generateUpdates(lazyStructWriter, maxUpdateSize)

	if len(updates) > 0 {
		// ds only stores the clock and length of items, and will be merged after transaction, so the length will not be too long, temporarily not optimized
		dsBytes := decoder.restDecoder().Bytes()
		updates[0] = append(updates[0], dsBytes...)
		for i := 1; i < len(updates); i++ {
			updates[i] = append(updates[i], 0)
		}
	}

	return updates, nil
}

func diffUpdateWith(update []uint8, sv []uint8, YDecoder func([]byte) updateDecoder, YEncoder func() updateEncoder) ([]uint8, error) {
	// An empty/nil state vector means "the remote has nothing" — diff against the
	// empty map (send everything). Normalize to the canonical [0]-client encoding
	// (matching EncodeStateAsUpdateWith) so this legitimate case decodes cleanly,
	// while a genuinely truncated non-empty sv still surfaces a decode error.
	if len(sv) == 0 {
		sv = []byte{0}
	}
	state, err := decodeStateVector(sv)
	if err != nil {
		return nil, fmt.Errorf("diff update: decode state vector: %w", err)
	}
	encoder := YEncoder()
	lazyStructWriter := newLazyStructWriter(encoder)
	decoder := YDecoder(update)
	reader := newLazyStructReader(decoder, false)

	for reader.curr != nil {
		curr := reader.curr
		currClient := curr.getID().Client
		svClock := state[currClient]
		if isSameType(reader.curr, &skipStruct{}) {
			// the first written struct shouldn't be a skip
			reader.nextStruct()
			continue
		}

		if curr.getID().Clock+curr.structLength() > svClock {
			if err := writeStructToLazyStructWriter(lazyStructWriter, curr, maxNumber(svClock-curr.getID().Clock, 0)); err != nil {
				return nil, err
			}
			reader.nextStruct()

			for reader.curr != nil && reader.curr.getID().Client == currClient {
				if err := writeStructToLazyStructWriter(lazyStructWriter, reader.curr, 0); err != nil {
					return nil, err
				}
				reader.nextStruct()
			}
		} else {
			// read until something new comes up
			for reader.curr != nil && reader.curr.getID().Client == currClient && reader.curr.getID().Clock+reader.curr.structLength() <= svClock {
				reader.nextStruct()
			}
		}
	}
	// DoS 1: surface a struct-count-cap breach from the lazy reader rather than
	// emit a partial diff.
	if err := reader.decodeError(); err != nil {
		return nil, fmt.Errorf("diff update: %w", err)
	}
	finishLazyStructWriting(lazyStructWriter)

	// write ds
	// A truncated/malformed delete set now fails the diff at decode time rather
	// than yielding a silent nil that a caller (e.g. WriteSyncStep2FromUpdate)
	// reads as an empty/synced update.
	ds, err := readDeleteSet(decoder)
	if err != nil {
		return nil, fmt.Errorf("diff update: read delete set: %w", err)
	}
	// Fail with an error (rather than panic or a silent nil) if a malformed delete
	// range cannot be re-encoded into the target format.
	if err := writeDeleteSet(encoder, ds); err != nil {
		return nil, fmt.Errorf("diff update: delete set encode failed: %w", err)
	}
	out := encoder.toBytes()
	if err := encoder.encodeError(); err != nil {
		return nil, fmt.Errorf("diff update: encode failed: %w", err)
	}
	return out, nil
}

func DiffUpdate(update []uint8, sv []uint8) ([]uint8, error) {
	return diffUpdateWith(update, sv, newDecoderV1, newEncoderV1)
}

// DiffUpdateV2 computes the differential V2 update of a V2-encoded update
// against the V1-encoded state vector sv.
func DiffUpdateV2(update []uint8, sv []uint8) ([]uint8, error) {
	return diffUpdateWith(update, sv, newDecoderV2, newEncoderV2)
}

func diffUpdates(update []uint8, sv []uint8, maxUpdateSize int) ([][]uint8, error) {
	return diffUpdatesWith(update, sv, newDecoderV1, newEncoderV1, maxUpdateSize)
}

// convertUpdateFormatWith re-encodes an update from the source format (the
// decoder built by mkDecoder) into the target format (the encoder built by
// mkEncoder), via a full lazy decode/re-encode cycle. It mirrors Yjs's
// convertUpdateFormat with an identity block transformer.
//
// It returns an error (never a silent nil) when the update cannot be re-encoded:
// a content-encode failure, a clock diff that overflows the target column codec
// (a >2^61 V1 clock delta converted to V2), or a delete range the target format
// cannot represent (a V1-legal 0-length range converted to V2, which stores
// len-1). A nil-without-error would be read by a caller as "empty/success" and
// silently drop the malformed update.
func convertUpdateFormatWith(update []uint8, mkDecoder func([]byte) updateDecoder, mkEncoder func() updateEncoder) ([]uint8, error) {
	updateDecoder := mkDecoder(update)
	lazyDecoder := newLazyStructReader(updateDecoder, false)
	updateEncoder := mkEncoder()
	lazyWriter := newLazyStructWriter(updateEncoder)

	for curr := lazyDecoder.curr; curr != nil; curr = lazyDecoder.nextStruct() {
		if err := writeStructToLazyStructWriter(lazyWriter, curr, 0); err != nil {
			return nil, fmt.Errorf("convert update format: %w", err)
		}
	}
	// DoS 1: surface a struct-count-cap breach from the lazy reader rather than
	// emit a partial converted update.
	if err := lazyDecoder.decodeError(); err != nil {
		return nil, fmt.Errorf("convert update format: %w", err)
	}
	finishLazyStructWriting(lazyWriter)

	// A truncated/malformed delete set now errors at decode time instead of
	// returning a silent nil a caller would read as "empty/success".
	ds, err := readDeleteSet(updateDecoder)
	if err != nil {
		return nil, fmt.Errorf("convert update format: read delete set: %w", err)
	}
	if err := writeDeleteSet(updateEncoder, ds); err != nil {
		return nil, fmt.Errorf("convert update format: delete set encode failed: %w", err)
	}
	out := updateEncoder.toBytes()
	// Surface a deferred column-encode error (e.g. a >2^61 clock diff hitting the
	// IntDiffOptRle bound when converting a hostile V1 update to V2).
	if err := updateEncoder.encodeError(); err != nil {
		return nil, fmt.Errorf("convert update format: encode failed: %w", err)
	}
	return out, nil
}

// ConvertUpdateFormatV1ToV2 converts a V1-encoded update to V2.
func ConvertUpdateFormatV1ToV2(update []uint8) ([]uint8, error) {
	return convertUpdateFormatWith(update, newDecoderV1, newEncoderV2)
}

// ConvertUpdateFormatV2ToV1 converts a V2-encoded update to V1.
func ConvertUpdateFormatV2ToV1(update []uint8) ([]uint8, error) {
	return convertUpdateFormatWith(update, newDecoderV2, newEncoderV1)
}

func flushLazyStructWriter(lazyWriter *lazyStructWriter) {
	if lazyWriter.written > 0 {
		// Capture only this fragment's rest bytes and start a fresh rest buffer.
		// For V2 this preserves the column sub-encoders, which keep accumulating
		// across all fragments (matching Yjs flushLazyStructWriter).
		lazyWriter.clientStructs = append(lazyWriter.clientStructs, clientStruct{
			written:     lazyWriter.written,
			restEncoder: lazyWriter.encoder.restartRestEncoder(),
		})

		if lazyWriter.needRecordPosition {
			lazyWriter.clientStructs[len(lazyWriter.clientStructs)-1].positionList = lazyWriter.positionList
			lazyWriter.clientStructs[len(lazyWriter.clientStructs)-1].client = lazyWriter.currClient
			lazyWriter.positionList = make([]positionInfo, 0)
		}
		lazyWriter.written = 0
	}
}

func writeStructToLazyStructWriter(lazyWriter *lazyStructWriter, s abstractStruct, offset Number) error {
	// flush curr if we start another client
	if lazyWriter.written > 0 && lazyWriter.currClient != s.getID().Client {
		flushLazyStructWriter(lazyWriter)
	}

	if lazyWriter.written == 0 {
		lazyWriter.currClient = s.getID().Client
		// write next client
		lazyWriter.encoder.writeClient(s.getID().Client)

		// write startClock
		writeVarUint(lazyWriter.encoder.restEncoder(), uint64(s.getID().Clock+offset))

		// record position of first struct
		if lazyWriter.needRecordPosition {
			pos := positionInfo{clock: s.getID().Clock + offset, startByte: lazyWriter.encoder.restEncoder().Len(), structNo: lazyWriter.written}
			lazyWriter.positionList = append(lazyWriter.positionList, pos)
		}
	}

	var startByte int
	if lazyWriter.needRecordPosition && lazyWriter.written > 0 {
		startByte = lazyWriter.encoder.restEncoder().Len()
	}

	// Surface a content-encode failure (e.g. an unserializable any) instead of
	// dropping it and letting the lazy writer emit a truncated struct.
	if err := s.writeStruct(lazyWriter.encoder, offset); err != nil {
		return err
	}

	if lazyWriter.needRecordPosition && lazyWriter.written > 0 {
		lastStartByte := lazyWriter.positionList[len(lazyWriter.positionList)-1].startByte
		if lazyWriter.encoder.restEncoder().Len() >= lastStartByte+recordPositionUnit {
			pos := positionInfo{clock: s.getID().Clock + offset, startByte: startByte, structNo: lazyWriter.written}
			lazyWriter.positionList = append(lazyWriter.positionList, pos)
		}
	}

	lazyWriter.written++
	return nil
}

// Call this function when we collected all parts and want to
// put all the parts together. After calling this method,
// you can continue using the UpdateEncoder.
func finishLazyStructWriting(lazyWriter *lazyStructWriter) {
	flushLazyStructWriter(lazyWriter)

	// this is a fresh encoder because we called flushCurr
	restEncoder := lazyWriter.encoder.restEncoder()

	// Now we put all the fragments together.
	// This works similarly to `writeClientsStructs`

	// write # states that were updated - i.e. the clients
	writeVarUint(restEncoder, uint64(len(lazyWriter.clientStructs)))

	for i := 0; i < len(lazyWriter.clientStructs); i++ {
		partStructs := lazyWriter.clientStructs[i]

		// Works similarly to `writeStructs`

		// write # encoded structs
		writeVarUint(restEncoder, uint64(partStructs.written))

		// write the rest of the fragment
		writeUint8Array(restEncoder, partStructs.restEncoder)
	}
}

type lazyStructBlockArena[T any] struct {
	block []T
	used  int
	max   int
}

type lazyContentBlockArenas struct {
	// The three readers share one closure environment, so adding a sparse content
	// kind does not add an allocation to substantial blocks that never contain it.
	// String and Any use 128-entry blocks. ContentFormat is pointerful and 32 bytes;
	// 128 entries plus the scan-object header cross Go's 4 KiB malloc class, so 127
	// keeps each block in-class and avoids about 768 bytes of allocator slack.
	stringArena lazyStructBlockArena[contentString]
	anyArena    lazyStructBlockArena[contentAny]
	formatArena lazyStructBlockArena[contentFormat]
}

const (
	lazyStructBlockMax        = 64
	lazyContentBlockMax       = 128
	lazyFormatContentBlockMax = 127
)

func (a *lazyStructBlockArena[T]) alloc() *T {
	if a.used == len(a.block) {
		limit := a.max
		if limit == 0 {
			limit = lazyStructBlockMax
		}
		next := 1
		if a.used != 0 {
			next = min(a.used*2, limit)
		}
		// Values escape through the lazy reader, including decodeUpdate's retained
		// result. Allocate independent fixed blocks and never rewind one: replacing
		// this slice header leaves every previously published pointer stable.
		a.block = make([]T, next)
		a.used = 0
	}
	item := &a.block[a.used]
	a.used++
	return item
}

// decodedUpdate is what an update decodes to: its structs in wire order, and its delete set.
// The reference returns `{structs, ds}` from decodeUpdate; this is the Go shape of that.
type decodedUpdate struct {
	structs []abstractStruct
	ds      *deleteSet
}

// decodeUpdateWith reads an update into its structs and delete set WITHOUT applying it — the
// introspection primitive tooling needs (what does this update actually contain?).
//
// Faithful to yjs decodeUpdateV2 (src/utils/updates.js): a lazyStructReader with skip-filtering
// OFF, so Skip blocks are reported rather than swallowed, followed by the delete set. Errors are
// surfaced rather than returning a partial result a caller would read as success, matching how
// ConvertUpdateFormatWith handles the same reader.
func decodeUpdateWith(update []uint8, mkDecoder func([]byte) updateDecoder) (*decodedUpdate, error) {
	updateDecoder := mkDecoder(update)
	lazyDecoder := newLazyStructReader(updateDecoder, false)

	var structs []abstractStruct
	for curr := lazyDecoder.curr; curr != nil; curr = lazyDecoder.nextStruct() {
		structs = append(structs, curr)
	}
	if err := lazyDecoder.decodeError(); err != nil {
		return nil, fmt.Errorf("decode update: %w", err)
	}
	ds, err := readDeleteSet(updateDecoder)
	if err != nil {
		return nil, fmt.Errorf("decode update: read delete set: %w", err)
	}
	return &decodedUpdate{structs: structs, ds: ds}, nil
}

// decodeUpdate reads a V1-encoded update into its structs and delete set.
func decodeUpdate(update []uint8) (*decodedUpdate, error) {
	return decodeUpdateWith(update, func(b []byte) updateDecoder { return newUpdateDecoderV1(b) })
}

// decodeUpdateV2 reads a V2-encoded update into its structs and delete set.
func decodeUpdateV2(update []uint8) (*decodedUpdate, error) {
	return decodeUpdateWith(update, func(b []byte) updateDecoder { return newUpdateDecoderV2(b) })
}

// obfuscatorOptions selects which classes of content an obfuscator scrubs. A nil *obfuscatorOptions
// means all three, matching the reference's `{ formatting = true, subdocs = true, yxml = true }`
// default destructuring — a Go zero value would otherwise invert every default to false.
type obfuscatorOptions struct {
	formatting bool
	subdocs    bool
	yxml       bool
}

// defaultObfuscatorOptions returns the reference's defaults: scrub everything.
func defaultObfuscatorOptions() *obfuscatorOptions {
	return &obfuscatorOptions{formatting: true, subdocs: true, yxml: true}
}

// newObfuscator returns the reference's block transformer: it replaces CONTENT while preserving
// every piece of CRDT structure — ids, lengths, origins, parents and the delete set — so an
// obfuscated update still applies and still reproduces the original document's shape.
//
// Faithful to yjs createObfuscator (src/utils/updates.js), including its caches: the same input maps
// to the same output every time, so a document that repeats a key or a format value still repeats it
// after obfuscation. Losing that would change the document's structure, which is the one thing this
// function must not do.
func newObfuscator(opts *obfuscatorOptions) func(abstractStruct) (abstractStruct, error) {
	if opts == nil {
		opts = defaultObfuscatorOptions()
	}
	i := 0
	mapKeyCache := map[string]string{}
	nodeNameCache := map[string]string{}
	formattingKeyCache := map[string]string{}
	formattingValueCache := map[string]interface{}{}

	// valueKey renders a format value as a cache key. The reference keys a JS Map by the value
	// itself; Go map keys must be comparable and a format value may not be, so it is rendered
	// instead — same "equal values share an obfuscation" semantics without risking a panic.
	valueKey := func(v interface{}) string { return fmt.Sprintf("%T:%v", v, v) }
	// The reference presets null -> null: the end of a formatting range must stay the end of one.
	formattingValueCache[valueKey(Null)] = Null
	formattingValueCache[valueKey(nil)] = Null

	cached := func(c map[string]string, k string, mk func() string) string {
		if v, ok := c[k]; ok {
			return v
		}
		v := mk()
		c[k] = v
		return v
	}

	return func(block abstractStruct) (abstractStruct, error) {
		item, isItem := block.(*itemStruct)
		if !isItem {
			// GC and Skip carry no content, so they pass through untouched.
			return block, nil
		}
		switch c := item.content.(type) {
		case *contentDeleted:
			// Nothing to scrub: the content is already gone.
		case *contentType:
			if opts.yxml {
				switch tp := c.value.(type) {
				case *YXmlElement:
					tp.NodeName = cached(nodeNameCache, tp.NodeName,
						func() string { return fmt.Sprintf("node-%d", i) })
				case *yXmlHook:
					tp.hookName = cached(nodeNameCache, tp.hookName,
						func() string { return fmt.Sprintf("hook-%d", i) })
				}
			}
		case *contentAny:
			for k := range c.arr {
				c.arr[k] = i
			}
		case *contentBinary:
			c.value = []uint8{uint8(i)}
		case *contentDoc:
			if opts.subdocs {
				c.opts = newObject()
				if c.doc != nil {
					c.doc.Guid = fmt.Sprint(i)
				}
			}
		case *contentEmbed:
			c.embed = newObject()
		case *contentFormat:
			if opts.formatting {
				c.key = cached(formattingKeyCache, c.key, func() string { return fmt.Sprint(i) })
				vk := valueKey(c.value)
				if v, ok := formattingValueCache[vk]; ok {
					c.value = v
				} else {
					replacement := newObject()
					replacement.Set("i", i)
					formattingValueCache[vk] = replacement
					c.value = replacement
				}
			}
		case *contentJSON:
			for k := range c.arr {
				c.arr[k] = i
			}
		case *contentString:
			// Repeat one digit to the SAME length, measured in UTF-16 units as the reference
			// measures it — a byte-length replacement would change the item's length and with it
			// the document's structure, which is exactly what must survive.
			c.value = strings.Repeat(fmt.Sprint(i%10), stringLength(c.value))
		default:
			return nil, fmt.Errorf("obfuscate update: unknown content type %T", item.content)
		}
		if item.parentSub != "" {
			item.parentSub = cached(mapKeyCache, item.parentSub, func() string { return fmt.Sprint(i) })
		}
		i++
		return item, nil
	}
}

// convertUpdateFormatTransform re-encodes an update, passing every block through transform first.
// ConvertUpdateFormatWith is this with an identity transform; obfuscation is this with a scrubber.
func convertUpdateFormatTransform(update []uint8, transform func(abstractStruct) (abstractStruct, error),
	mkDecoder func([]byte) updateDecoder, mkEncoder func() updateEncoder) ([]uint8, error) {
	updateDecoder := mkDecoder(update)
	lazyDecoder := newLazyStructReader(updateDecoder, false)
	updateEncoder := mkEncoder()
	lazyWriter := newLazyStructWriter(updateEncoder)

	for curr := lazyDecoder.curr; curr != nil; curr = lazyDecoder.nextStruct() {
		out, err := transform(curr)
		if err != nil {
			return nil, err
		}
		if err := writeStructToLazyStructWriter(lazyWriter, out, 0); err != nil {
			return nil, fmt.Errorf("convert update format: %w", err)
		}
	}
	if err := lazyDecoder.decodeError(); err != nil {
		return nil, fmt.Errorf("convert update format: %w", err)
	}
	finishLazyStructWriting(lazyWriter)

	ds, err := readDeleteSet(updateDecoder)
	if err != nil {
		return nil, fmt.Errorf("convert update format: read delete set: %w", err)
	}
	if err := writeDeleteSet(updateEncoder, ds); err != nil {
		return nil, fmt.Errorf("convert update format: delete set encode failed: %w", err)
	}
	out := updateEncoder.toBytes()
	if err := updateEncoder.encodeError(); err != nil {
		return nil, fmt.Errorf("convert update format: encode failed: %w", err)
	}
	return out, nil
}

// obfuscateUpdate strips the content out of a V1 update while preserving its CRDT structure, so a
// document can be attached to a bug report. Structure-derived information (typing rhythm, document
// shape) still survives, exactly as the reference warns.
func obfuscateUpdate(update []uint8) ([]uint8, error) {
	return obfuscateUpdateWith(update, nil)
}

// ObfuscateUpdate replaces user content in a V1 update while preserving its
// CRDT structure, allowing the update to be shared as a reproducible fixture.
func ObfuscateUpdate(update []uint8) ([]uint8, error) {
	return obfuscateUpdate(update)
}

// obfuscateUpdateWith is obfuscateUpdate with explicit options; nil means all defaults.
func obfuscateUpdateWith(update []uint8, opts *obfuscatorOptions) ([]uint8, error) {
	return convertUpdateFormatTransform(update, newObfuscator(opts),
		func(b []byte) updateDecoder { return newUpdateDecoderV1(b) },
		func() updateEncoder { return newUpdateEncoderV1() })
}

// obfuscateUpdateV2 is obfuscateUpdate for a V2-encoded update.
func obfuscateUpdateV2(update []uint8) ([]uint8, error) {
	return obfuscateUpdateV2With(update, nil)
}

// obfuscateUpdateV2With is obfuscateUpdateV2 with explicit options; nil means all defaults.
func obfuscateUpdateV2With(update []uint8, opts *obfuscatorOptions) ([]uint8, error) {
	return convertUpdateFormatTransform(update, newObfuscator(opts),
		func(b []byte) updateDecoder { return newUpdateDecoderV2(b) },
		func() updateEncoder { return newDefaultUpdateEncoderV2() })
}
