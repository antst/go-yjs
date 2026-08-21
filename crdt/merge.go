package crdt

import (
	"errors"
	"fmt"
	"math"
	"sort"
)

/*
 * We use the first five bits in the info flag for determining the type of the struct.
 *
 * 0: GC
 * 1: Item with Deleted content
 * 2: Item with JSON content
 * 3: Item with Binary content
 * 4: Item with String content
 * 5: Item with Embed content (for richtext content)
 * 6: Item with Format content (a formatting marker for richtext content)
 * 7: Item with Type
 */

type clientStructRef struct {
	i    Number
	refs []abstractStruct
}

const maxDecodedStringItemBlockSize = 64

type decodedStringItem struct {
	item    itemStruct
	content contentString
}

type decodedStringItemArena struct {
	block         []decodedStringItem
	pos           int
	nextBlockSize int
}

func (a *decodedStringItemArena) alloc(str string) *decodedStringItem {
	if a.pos == len(a.block) {
		blockSize := a.nextBlockSize
		if blockSize == 0 {
			blockSize = 1
		}
		a.block = make([]decodedStringItem, blockSize)
		a.pos = 0
		if blockSize < maxDecodedStringItemBlockSize {
			a.nextBlockSize = minNumber(blockSize*2, maxDecodedStringItemBlockSize)
		}
	}
	storage := &a.block[a.pos]
	a.pos++
	storage.content.value = str
	return storage
}

// remainingLener is implemented by both update decoders to report how many
// undecoded bytes remain across all of their buffers. It lets
// readClientsStructRefs bound a corrupt struct count without spinning.
type remainingLener interface {
	RemainingLen() int
}

// decoderRemaining returns the decoder's undecoded byte count, or -1 if the
// decoder does not expose one (in which case the watchdog is skipped).
func decoderRemaining(decoder updateDecoder) int {
	if r, ok := decoder.(remainingLener); ok {
		return r.RemainingLen()
	}
	return -1
}

// maxStallIterations bounds how many consecutive zero-progress struct reads a
// decode loop tolerates before declaring the stream exhausted/corrupt. A corrupt
// struct count (e.g. 2^32) combined with an RleDecoder infinite-run sentinel can
// otherwise spin a decode loop without consuming input or advancing the clock.
const maxStallIterations = 64

// The cumulative decoded-struct cap is min(proportional, absoluteCeiling). The
// stall guard (above) only catches a loop that makes NO forward progress; it does
// NOT catch a struct count that advances the clock every iteration while
// consuming zero bytes — which is exactly what a V2 GC struct does once the info
// RLE column hits its -1 "repeat forever" sentinel and the len Opt-RLE column is
// mid-run (the info byte and length come back without consuming, while clock +=
// length). That lets an attacker-supplied numberOfStructs (up to 2^64) grow Refs
// without bound. Two complementary bounds close it:
//
//   - maxStructsPerInputByte (K) is the PROPORTIONAL term's slope: the
//     amplification guard for the common/medium-input case, where the real
//     structs/byte ratio is well under 1. Empirically the densest LEGITIMATE
//     updates with bounded byte size are a regular-spaced-GC text (every other
//     char deleted under GC) at ~0.29 structs/byte and a 50k-key full-state map at
//     ~0.086. K=128 keeps >400x headroom over the spaced-GC density yet is far
//     below the attack, and (combined with the tiny additive constant) keeps a
//     small input's heap minimal — a 23-byte OOM is capped at a few thousand
//     structs.
//
//   - structCountAbsoluteCeiling (A) is the HARD memory bound — the actual close.
//     It MUST exist because structs/byte is UNBOUNDED for legitimate
//     fully-RLE-compressed GC updates: an empirically measured front-insert +
//     delete-all of N elements encodes to ~50 bytes yet decodes to N GC structs
//     (10,000+ structs/byte, rising with N), so NO purely-proportional cap can
//     separate a large-INPUT amplification attack from legit input by ratio. Only
//     an absolute count bounds memory regardless of input size: a 1MB attack that
//     the old purely-proportional K=512 cap would have amplified to ~512M structs
//     (~170 GB) is clamped here to A — the heap is governed by A, not by the input
//     length.
//
//     A = 1<<23 (8,388,608) structs. At a measured ~340 B per decoded struct
//     (interface slot + *GC + slice growth) that bounds the decode heap to ~2.85
//     GB — survivable on a server, and ~16.8x above the largest measured
//     legitimate single update (a 500k-struct full state), so a real full-state
//     sync is never false-rejected. (The 60M-struct OOM attack is rejected; a
//     genuinely larger-than-A single document update is also rejected, but such an
//     update is itself a multi-GB allocation, and numberOfStructs is the only
//     signal the format exposes to tell it from an attack.)
//
// cap = min(totalInput*K + structCountCapConst, A).
const maxStructsPerInputByte = 128

// structCountAbsoluteCeiling is the hard upper bound on cumulative decoded
// structs, independent of input size — the memory bound that actually closes the
// OOM (see maxStructsPerInputByte). ~8.39M structs ≈ ~2.85 GB decode heap.
const structCountAbsoluteCeiling uint64 = 1 << 23

// structCountCapConst is the proportional cap's additive constant (slack for the
// first struct of a minimal update); it is NOT a memory floor — small inputs stay
// bounded by K*bytes, not by this constant.
const structCountCapConst = maxStructsPerInputByte

// structCountCap returns the ceiling on cumulative decoded structs for an input
// whose total undecoded byte length at decode start is totalInput:
//
//	cap = min(totalInput*K + structCountCapConst, A)
//
// The absolute ceiling A bounds heap regardless of input; the proportional term
// tightens it for small/medium inputs. When the decoder cannot report a length
// (totalInput < 0 — no RemainingLen), the absolute ceiling alone is used so the
// heap is still bounded; both V1 and V2 decoders do report one, so that fallback
// is defensive only.
func structCountCap(totalInput int) uint64 {
	if totalInput < 0 {
		return structCountAbsoluteCeiling
	}
	proportional := uint64(totalInput)*maxStructsPerInputByte + structCountCapConst
	if proportional < structCountAbsoluteCeiling {
		return proportional
	}
	return structCountAbsoluteCeiling
}

// stallGuard detects a struct-decode loop that is spinning without making
// forward progress. Every legitimate struct read either consumes decoder bytes
// or advances the clock (length > 0); a run of iterations that do neither means
// the input is exhausted or corrupt. Both the eager (readClientsStructRefs) and
// the lazy (lazyStructReader) struct readers share it so they agree on when to
// bail. A negative "remaining" (decoder without a RemainingLen) disables the
// byte-progress half of the check, falling back to clock progress only.
type stallGuard struct {
	decoder updateDecoder
	max     int
	stalled int
}

// snapshot captures the decoder's remaining bytes and the current clock before a
// struct read; feed both back into progressed() afterwards.
func (g *stallGuard) snapshot(clock Number) (beforeRemaining int, beforeClock Number) {
	return decoderRemaining(g.decoder), clock
}

// progressed reports whether the just-completed struct read made forward
// progress. When it returns false for more than max consecutive reads the caller
// must stop: the stream is exhausted/corrupt. The internal stall counter resets
// on any progress.
func (g *stallGuard) progressed(beforeRemaining int, beforeClock, clock Number) bool {
	if (beforeRemaining < 0 || decoderRemaining(g.decoder) >= beforeRemaining) && clock == beforeClock {
		g.stalled++
		return g.stalled <= g.max
	}
	g.stalled = 0
	return true
}

type pendingStructsResult struct {
	missing map[Number]Number
	update  []uint8
}

func writeClientsStructs(encoder updateEncoder, store *structStore, _sm map[Number]Number) error {
	// we filter all valid _sm entries into sm
	sm := make(map[Number]Number)

	for client, clock := range _sm {
		// Only entries that are BOTH present in the store AND have newer structs go
		// into sm. The presence check is load-bearing for the framing invariant
		// (E#1): the header below writes len(sm), so an absent client must never
		// enter sm — otherwise the per-client write loop would skip it and emit
		// len(sm)-1 blocks against a header claiming len(sm), misframing the update.
		// (GetState returns 0 for an absent client; with the readStateVector toNumber
		// clamp clock is non-negative so `0 > clock` is already false, but pin it
		// explicitly so no future caller can smuggle one in.)
		if _, ok := store.clientStructs(client); ok && getState(store, client) > clock {
			sm[client] = clock // only write if new structs are available
		}
	}

	sv := getStateVector(store)
	for client := range sv {
		// sv is derived from GetStateVector(store), so every client here is present
		// in the store by construction — these additions cannot break the invariant.
		if _, exist := _sm[client]; !exist {
			sm[client] = 0
		}
	}

	// write # states that were updated. INVARIANT: every client in sm is present in
	// the store (the filter loop's presence check, and sv being store-derived), so
	// len(sm) == the number of blocks the loop below writes — the header count and
	// the body can never disagree.
	writeEncoderRestVarUint(encoder, uint64(len(sm)))

	// Write items with higher client ids first
	// This heavily improves the conflict algorithm.
	var writeErr error
	mapSortedRange(sm, false, func(client, clock Number) {
		if writeErr != nil {
			return // first error wins; skip remaining clients
		}
		// By the invariant above every client in sm is present, so this lookup
		// always succeeds; the !ok branch is dead but kept as a typed-nil deref
		// guard (WriteStructs derefs (*structs)[0]) so a future change to sm's
		// construction cannot silently reintroduce the K3/misframe crash. If it ever
		// IS reached, fail LOUDLY (record writeErr) rather than bare-return: a bare
		// return would emit len(sm)-1 blocks against a header claiming len(sm),
		// misframing the whole update silently (E#3).
		structs, ok := store.clientStructs(client)
		if !ok {
			writeErr = fmt.Errorf("write clients structs: client %d absent from store", client)
			return
		}
		writeErr = structs.writeStructs(encoder, client, clock)
	})
	return writeErr
}

func readClientsStructRefs(decoder updateDecoder, doc *Doc) (map[Number]*clientStructRef, error) {
	clientRefs := make(map[Number]*clientStructRef)
	var stringItems decodedStringItemArena
	stream := newStructDecodeStream(decoder, structDecodeStreamOptions{
		mode:                 structDecodeEager,
		doc:                  doc,
		lenientMissingHeader: false,
	})

	blocksRead := 0
	gcCnt, skipCnt, itemCnt := 0, 0, 0
	for {
		block, ok, err := stream.NextBlock()
		if err != nil {
			return clientRefs, fmt.Errorf("read clients struct refs: block %d: %w", blocksRead, err)
		}
		if !ok {
			break
		}
		clientStructRef := &clientStructRef{
			refs: make([]abstractStruct, 0, int(block.ReserveCount)),
		}
		clientRefs[block.Client] = clientStructRef
		if block.DeclaredCount != 0 {
			gc, skip, item, err := stream.collectEagerBlock(&stringItems, clientStructRef)
			if err != nil {
				return clientRefs, fmt.Errorf("read clients struct refs: block %d: %w", blocksRead, err)
			}
			gcCnt += gc
			skipCnt += skip
			itemCnt += item
		}
		blocksRead++
	}

	// intentionally track struct mix and keep no special-case logging path here
	_ = gcCnt + skipCnt + itemCnt

	return clientRefs, nil
}

// Resume computing structs generated by struct readers.
//
// While there is something to do, we integrate structs in this order
// 1. top element on stack, if stack is not empty
// 2. next element from current struct reader (if empty, use next struct reader)
//
// If struct causally depends on another struct (ref.missing), we put next reader of
// `ref.id.client` on top of stack.
//
// At some point we find a struct that has no causal dependencies,
// then we start emptying the stack.
//
// It is not possible to have circles: i.e. struct1 (from client1) depends on struct2 (from client2)
// depends on struct3 (from client1). Therefore the max stack size is eqaul to `structReaders.length`.
//
// This method is implemented in a way so that we can resume computation if this update
// causally depends on another update.
//
// It returns the unintegrated rest-structs as a resumable pending-apply buffer,
// or an error if their re-encode fails. On that error the caller MUST NOT store
// the returned restStructs: its Update buffer would be truncated and silently
// corrupt every later resume. The integration side-effects already applied to
// the store are intentionally left in place (best-effort apply, matching the
// rest of the path); only the corrupt pending buffer is refused.
func integrateStructs(trans *Transaction, store *structStore, clientsStructRefs map[Number]*clientStructRef) (*pendingStructsResult, error) {
	if len(clientsStructRefs) == 0 {
		return nil, nil
	}

	var stack []abstractStruct

	// sort them so that we take the higher id first, in case of conflicts the lower id will probably not conflict with the id from the higher user.
	clientsStructRefsIds := make(numberSlice, 0, len(clientsStructRefs))
	for k := range clientsStructRefs {
		clientsStructRefsIds = append(clientsStructRefsIds, k)
	}
	sort.Sort(clientsStructRefsIds)

	getNextStructTarget := func() *clientStructRef {
		if len(clientsStructRefsIds) == 0 {
			return nil
		}

		nextStructsTarget := clientsStructRefs[clientsStructRefsIds[len(clientsStructRefsIds)-1]]
		for len(nextStructsTarget.refs) == nextStructsTarget.i {
			clientsStructRefsIds = clientsStructRefsIds[:len(clientsStructRefsIds)-1]
			if len(clientsStructRefsIds) > 0 {
				nextStructsTarget = clientsStructRefs[clientsStructRefsIds[len(clientsStructRefsIds)-1]]
			} else {
				return nil
			}
		}

		return nextStructsTarget
	}

	curStructsTarget := getNextStructTarget()
	if curStructsTarget == nil && len(stack) == 0 {
		return nil, nil
	}

	restStructs := newStructStore()
	missingSV := make(map[Number]Number)

	updateMissingSv := func(client Number, clock Number) {
		mclock, exist := missingSV[client]
		if !exist || mclock > clock {
			missingSV[client] = clock
		}
	}

	stackHead := curStructsTarget.refs[curStructsTarget.i]
	curStructsTarget.i++

	// caching the state because it is used very often
	state := make(map[Number]Number)

	addStackToRestSS := func() {
		for _, item := range stack {
			client := item.getID().Client
			unapplicableItems := clientsStructRefs[client]

			if unapplicableItems != nil {
				// decrement because we weren't able to apply previous operation
				unapplicableItems.i--

				var cpRefs []abstractStruct
				for i := unapplicableItems.i; i < len(unapplicableItems.refs); i++ {
					cpRefs = append(cpRefs, unapplicableItems.refs[i])
				}

				for _, value := range cpRefs {
					restStructs.appendClientStruct(client, value)
				}
				delete(clientsStructRefs, client)

				unapplicableItems.i = 0
				unapplicableItems.refs = nil
			} else {
				// item was the last item on clientsStructRefs and the field was already cleared. Add item to restStructs and continue
				restStructs.appendClientStruct(client, item)
			}

			// remove client from clientsStructRefsIds to prevent users from applying the same update again
			clientsStructRefsIds = clientsStructRefsIds.Filter(func(number Number) bool {
				return number != client
			})
		}
		stack = nil
	}

	// Conflict sets are transient per Item but share the same high-water mark
	// throughout one remote update. Retaining their cleared overflow maps here
	// turns that repeated allocation into at most two growth sequences.
	var itemScratch integrationItemScratch

	// iterate over all struct readers until we are done
mergeLoop:
	for {
		if !isSameType(stackHead, &skipStruct{}) {
			state[stackHead.getID().Client] = getState(store, stackHead.getID().Client)
			localClock := state[stackHead.getID().Client]
			offset := localClock - stackHead.getID().Clock

			if offset < 0 {
				// update from the same client is missing
				stack = append(stack, stackHead)
				updateMissingSv(stackHead.getID().Client, stackHead.getID().Clock-1)

				// hid a dead wall, add all items from stack to restSS
				addStackToRestSS()
			} else {
				missing, err := stackHead.missingClient(trans, store)
				if err == nil {
					stack = append(stack, stackHead)

					// get the struct reader that has the missing struct
					structRefs := clientsStructRefs[missing]
					if structRefs == nil {
						structRefs = &clientStructRef{}
					}

					if len(structRefs.refs) == structRefs.i {
						// This update message causally depends on another update message that doesn't exist yet
						updateMissingSv(missing, getState(store, missing))
						addStackToRestSS()
					} else {
						stackHead = structRefs.refs[structRefs.i]
						structRefs.i++
						continue
					}
				} else if offset == 0 || offset < stackHead.structLength() {
					// all fine, apply the stackhead
					if item, ok := stackHead.(*itemStruct); ok {
						if err := item.integrateWithScratch(trans, offset, &itemScratch); err != nil {
							return nil, err
						}
					} else {
						if err := stackHead.integrateStruct(trans, offset); err != nil {
							return nil, err
						}
					}
					state[stackHead.getID().Client] = stackHead.getID().Clock + stackHead.structLength()
				}
			}
		}

		// iterate to next stackHead.
		//
		// The termination break is LABELLED. Inside a switch an unlabelled break
		// binds to the switch, not the loop, which would turn "we are done" into
		// an infinite loop — a real bug this rewrite introduced once before the
		// label was added.
		switch {
		case len(stack) > 0:
			stackHead = stack[len(stack)-1]
			stack = stack[:len(stack)-1]
		case curStructsTarget != nil && curStructsTarget.i < len(curStructsTarget.refs):
			stackHead = curStructsTarget.refs[curStructsTarget.i]
			curStructsTarget.i++
		default:
			curStructsTarget = getNextStructTarget()
			if curStructsTarget == nil {
				// we are done
				break mergeLoop
			}
			stackHead = curStructsTarget.refs[curStructsTarget.i]
			curStructsTarget.i++
		}
	}

	if restStructs.clientCountValue() > 0 {
		encoder := newUpdateEncoderV1()
		// These are already-decoded V1 structs being re-encoded as V1 (no column
		// codecs), so a content-encode error cannot arise in practice. Propagate it
		// rather than drop it: a truncated re-encode here would otherwise be stored
		// verbatim in restStructs.Update — the resumable pending-apply buffer — and
		// silently corrupt every later resume.
		if err := writeClientsStructs(encoder, restStructs, make(map[Number]Number)); err != nil {
			return nil, fmt.Errorf("integrate structs: rest-struct re-encode failed: %w", err)
		}
		// write empty deleteset
		// writeDeleteSet(encoder, new DeleteSet())
		writeVarUint(encoder.rest, 0) // => no need for an extra function call, just write 0 deletes
		return &pendingStructsResult{
			missing: missingSV,
			update:  encoder.toBytes(),
		}, nil
	}

	return nil, nil
}

func writeStructsFromTransaction(encoder updateEncoder, trans *Transaction) error {
	return writeClientsStructs(encoder, trans.doc.store, trans.beforeState)
}

// Read and apply a document update.
// This function has the same effect as `applyUpdate` but accepts a decoder.
// It is format-generic: structDecoder may be a V1 or V2 decoder.
//
// An error is returned only after transaction cleanup and observer dispatch.
// As in Yjs, the transaction is not a rollback boundary: structs integrated
// before a malformed trailing field remain in the document. Callers receiving
// an error must treat the replica as unsynchronised and recover by resyncing.
func readUpdateV2(_ updateDecoder, ydoc *Doc, transactionOrigin interface{}, structDecoder updateDecoder) error {
	var applyErr error
	fail := func(stage string, err error) {
		if applyErr != nil {
			return
		}
		applyErr = fmt.Errorf("read update: %s: %w", stage, err)
	}

	Transact(ydoc, func(trans *Transaction) {
		// force that transaction.local is set to non-local
		trans.local = false
		retry := false
		doc := trans.doc
		store := doc.store
		ss, err := readClientsStructRefs(structDecoder, doc)
		if err != nil {
			fail("read struct refs", err)
			return
		}

		/*
			totalCnt := 0
			totalLength := 0
			totalConCnt := 0
			for k,v := range ss{
				length := 0
				clock := v.Refs[0].GetID().Clock
				gcCnt := 0
				skipCnt := 0
				itemCnt := 0
				continueCnt := 0
				for _, v1 := range v.Refs{
					length += v1.GetLength()
					continueCnt++
					if isSameType(v1, &GC{}) {
						gcCnt++
					}else if isSameType(v1, &Skip{}) {
						skipCnt++
						break
					}else{
						itemCnt++
					}
				}
				totalLength += length
				totalConCnt += continueCnt
				fmt.Printf("k:%d, len:%d itemLength:%d continueCnt:%d clock:%d gcCnt:%d skipCnt:%d itemCnt:%d\n", k, len(v.Refs), length, continueCnt,  clock, gcCnt, skipCnt, itemCnt)
				totalCnt += len(v.Refs)
			}
			fmt.Printf("clientCnt:%d itemCnt:%d totalConCnt:%d totalLength:%d\n", len(ss), totalCnt, totalConCnt, totalLength)
		*/

		restStructs, err := integrateStructs(trans, store, ss)
		if err != nil {
			// A failed re-encode of the unintegrated rest-structs means restStructs.Update
			// is truncated. Bail before it is stored as (or merged into) the pending
			// buffer — storing it would silently corrupt every later resume. Surfaced
			// through the same channel as the sibling pending-merge failures below.
			fail("integrate structs", err)
			return
		}
		pending := store.pendingStructs

		if pending != nil {
			// check if we can apply something
			for client, clock := range pending.missing {
				if clock < getState(store, client) {
					retry = true
					break
				}
			}

			if restStructs != nil {
				// merge restStructs into store.pending
				for client, clock := range restStructs.missing {
					mclock, exist := pending.missing[client]
					if !exist || mclock > clock {
						pending.missing[client] = clock
					}
				}
				// Both inputs are internally-produced V1 updates; a merge error here
				// means a corrupt pending buffer. Surface it (keep the old pending on
				// failure) rather than overwrite the pending update with a silent nil.
				merged, err := mergeUpdatesCore([][]uint8{pending.update, restStructs.update}, newDecoderV1, newEncoderV1)
				if err != nil {
					fail("merge pending structs", err)
					return
				}
				pending.update = merged
			}
		} else {
			store.pendingStructs = restStructs
		}

		// ReadAndApplyDeleteSet now decodes the WHOLE delete set before applying
		// any of it and returns an error on a truncated/malformed frame. A bare
		// nil used to be ambiguous between "no pending DS" and "truncated", so a
		// truncated incoming delete-only update silently dropped a graded subset
		// of its deletes (the receiver then permanently kept deletions the sender
		// removed). Bail on the error — the same way the struct-ref / integrate
		// failures above abort the apply — rather than treat it as no-pending-DS.
		dsRest, err := readAndApplyDeleteSet(structDecoder, trans, store)
		if err != nil {
			fail("read and apply delete set", err)
			return
		}
		if store.pendingDeleteSet != nil {
			// store.PendingDs is always V2-DS-encoded (ReadAndApplyDeleteSet emits
			// it through a V2 encoder), so it must be read and re-merged as V2.
			pendingDSUpdate := newUpdateDecoderV2(store.pendingDeleteSet)
			// Leading "0 structs" header (PendingDs encodes only deletes). It is
			// internally produced, so a read error or a non-zero count means a corrupt
			// pending buffer — surface it instead of replaying from a mispositioned
			// decoder (which would apply the wrong deletes).
			nStructs, err := readVarUintAny(pendingDSUpdate.restDecoder())
			if err != nil {
				fail("read pending delete-set header", err)
				return
			}
			if nStructs.(uint64) != 0 {
				fail("read pending delete-set header", fmt.Errorf("want 0 structs, got %d", nStructs.(uint64)))
				return
			}
			dsRest2, err := readAndApplyDeleteSet(pendingDSUpdate, trans, store)
			if err != nil {
				fail("re-apply pending delete set", err)
				return
			}

			if dsRest != nil && dsRest2 != nil {
				// case 1: ds1 != null && ds2 != null
				// Both are internally-produced V2 delete-set updates; on a merge error
				// surface it rather than store a silent nil pending DS.
				mergedDs, err := mergeUpdatesCore([][]uint8{dsRest, dsRest2}, newDecoderV2, newEncoderV2)
				if err != nil {
					fail("merge pending delete set", err)
					return
				}
				store.pendingDeleteSet = mergedDs
			} else {
				// case 2: ds1 != null
				// case 3: ds2 != null
				// case 4: ds1 == null && ds2 == null
				if dsRest != nil {
					store.pendingDeleteSet = dsRest
				} else {
					store.pendingDeleteSet = dsRest2
				}
			}
		} else {
			// Either dsRest == null && pendingDs == null OR dsRest != null
			store.pendingDeleteSet = dsRest
		}

		if retry {
			update := store.pendingStructs.update
			store.pendingStructs = nil
			// Pending structs are always re-encoded as V1 (see integrateStructs).
			if err := applyUpdateWith(trans.doc, update, nil, newUpdateDecoderV1(update)); err != nil {
				fail("retry pending structs", err)
			}
		}
	}, transactionOrigin, false)

	return applyErr
}

// Read and apply a document update.
// This function has the same effect as `applyUpdate` but accepts a decoder.
// See readUpdateV2 for the partial-apply error contract.
func readUpdate(decoder updateDecoder, ydoc *Doc, transactionOrigin interface{}) error {
	return readUpdateV2(decoder, ydoc, transactionOrigin, newUpdateDecoderV1(decoder.restDecoder().Bytes()))
}

// applyUpdateWith applies an update using the provided struct decoder (V1 or V2),
// mirroring Yjs applyUpdateV2's YDecoder parameter. The public ApplyUpdate /
// ApplyUpdateV2 wrappers pick the V1 / V2 decoder respectively. See readUpdateV2
// for the partial-apply error contract.
func applyUpdateWith(ydoc *Doc, update []uint8, transactionOrigin interface{}, structDecoder updateDecoder) error {
	decoder := newUpdateDecoderV1(update)
	return readUpdateV2(decoder, ydoc, transactionOrigin, structDecoder)
}

// ApplyUpdate applies a document update created by, for example,
// `y.on('update', update => ..)` or `update = encodeStateAsUpdate()`.
//
// This function has the same effect as `readUpdate` but accepts an Uint8Array instead of a Decoder.
// A non-nil error means the document may contain structs decoded before the bad
// field and must be resynchronised; it does not mean the mutation was rolled back.
func ApplyUpdate(ydoc *Doc, update []uint8, transactionOrigin interface{}) error {
	return applyUpdateWith(ydoc, update, transactionOrigin, newUpdateDecoderV1(update))
}

// ApplyUpdateV2 applies a V2-encoded document update (as produced by JS
// Y.encodeStateAsUpdateV2 or Go EncodeStateAsUpdateV2). Its error contract is
// identical to ApplyUpdate.
func ApplyUpdateV2(ydoc *Doc, update []uint8, transactionOrigin interface{}) error {
	return applyUpdateWith(ydoc, update, transactionOrigin, newUpdateDecoderV2(update))
}

// Write all the document as a single update message. If you specify the state of the remote client (`targetStateVector`) it will
// only write the operations that are missing.
func writeStateAsUpdate(encoder updateEncoder, doc *Doc, targetStateVector map[Number]Number) error {
	// Structs are written from live document content; a content-encode failure
	// here must surface rather than emit a truncated update.
	if err := writeClientsStructs(encoder, doc.store, targetStateVector); err != nil {
		return fmt.Errorf("write state as update: %w", err)
	}
	// The delete set is streamed from live store state, where every delete range
	// has length >= 1. Avoid materializing a temporary DeleteSet merely to encode
	// it; snapshots and undo still use the durable representation.
	if err := writeDeleteSetFromStructStore(encoder, doc.store, nil); err != nil {
		return fmt.Errorf("write state as update: delete set encode failed: %w", err)
	}
	return nil
}

// encodeStateAsUpdateWith writes the whole document as a single update message
// through the supplied encoder (V1 or V2). targetStateVector (V1-encoded) limits
// output to operations the remote is missing.
//
// Pending updates (present only after an out-of-order apply) are normalized to
// the target wire format before the merge: store.PendingDs is V2-encoded, and
// the PendingStructs diff is V1-encoded, so each is converted to the encoder's
// format via the V1->target / V2->target converters. This mirrors Yjs's
// encodeStateAsUpdateV2, which likewise converts pending updates to the target
// format before merging. Use `writeStateAsUpdate` directly if you already hold
// a lib0 encoder.
func encodeStateAsUpdateWith(doc *Doc, encodedTargetStateVector []uint8, encoder updateEncoder, mergeDecoder func([]byte) updateDecoder, mergeEncoder func() updateEncoder, fromV1, fromV2 func([]byte) ([]byte, error)) ([]uint8, error) {
	if len(encodedTargetStateVector) == 0 {
		encodedTargetStateVector = []byte{0}
	}

	targetStateVector, err := decodeStateVector(encodedTargetStateVector)
	if err != nil {
		return nil, fmt.Errorf("encode state as update: %w", err)
	}
	if err := writeStateAsUpdate(encoder, doc, targetStateVector); err != nil {
		return nil, err
	}

	// updates[0] (the primary output) is already in the target format. Surface a
	// deferred column-encode error from the primary write before going further.
	primary := encoder.toBytes()
	if err := encoder.encodeError(); err != nil {
		return nil, fmt.Errorf("encode state as update: encode failed: %w", err)
	}
	updates := [][]byte{primary}
	if len(doc.store.pendingDeleteSet) > 0 {
		conv, err := fromV2(doc.store.pendingDeleteSet) // PendingDs is V2-encoded
		if err != nil {
			return nil, fmt.Errorf("encode state as update: convert pending ds: %w", err)
		}
		updates = append(updates, conv)
	}

	if doc.store.pendingStructs != nil {
		diff, err := DiffUpdate(doc.store.pendingStructs.update, encodedTargetStateVector) // V1
		if err != nil {
			return nil, fmt.Errorf("encode state as update: diff pending structs: %w", err)
		}
		conv, err := fromV1(diff)
		if err != nil {
			return nil, fmt.Errorf("encode state as update: convert pending structs: %w", err)
		}
		updates = append(updates, conv)
	}

	if len(updates) > 1 {
		return mergeUpdatesWith(updates, mergeDecoder, mergeEncoder)
	}

	return updates[0], nil
}

// identityUpdate returns the update unchanged (used when a pending update is
// already in the target wire format).
func identityUpdate(u []byte) ([]byte, error) { return u, nil }

func EncodeStateAsUpdate(doc *Doc, encodedTargetStateVector []uint8) ([]uint8, error) {
	// V1 target: V1 pending stays V1, V2 pending (PendingDs) -> V1.
	if len(encodedTargetStateVector) == 0 && len(doc.store.pendingDeleteSet) == 0 && doc.store.pendingStructs == nil {
		return encodeFullStateAsUpdateV1(doc.store)
	}
	encoder := newFastUpdateEncoderV1(doc.store)
	return encodeStateAsUpdateWith(doc, encodedTargetStateVector, &encoder, newDecoderV1, newEncoderV1,
		identityUpdate, ConvertUpdateFormatV2ToV1)
}

// EncodeStateAsUpdateV2 writes the whole document (or the diff against
// encodedTargetStateVector, which is always V1-encoded) as a single V2 update
// message — byte-identical to JS Y.encodeStateAsUpdateV2. Pass nil for a full
// update.
func EncodeStateAsUpdateV2(doc *Doc, encodedTargetStateVector []uint8) ([]uint8, error) {
	// V2 target: V1 pending -> V2, V2 pending (PendingDs) stays V2.
	if len(encodedTargetStateVector) == 0 && len(doc.store.pendingDeleteSet) == 0 && doc.store.pendingStructs == nil {
		return encodeFullStateAsUpdateV2(doc.store)
	}
	return encodeStateAsUpdateWith(doc, encodedTargetStateVector, newDefaultUpdateEncoderV2(), newDecoderV2, newEncoderV2,
		ConvertUpdateFormatV1ToV2, identityUpdate)
}

// errNumberOverflow is the single sentinel both toNumber and nonNegNumber return
// when a decoded value does not fit a non-negative Number. Hoisting it out of the
// functions (instead of an inline fmt.Errorf) drops their inline cost below the
// budget so they inline at every call site (G#1) — the hot decode path then pays
// only a compare + branch per untrusted varuint.
var errNumberOverflow = errors.New("value overflows a non-negative Number")

// maxSafeInteger is lib0/Yjs Number.MAX_SAFE_INTEGER. lib0's readVarUint THROWS
// "Integer out of Range" the instant a single decoded varint exceeds this, so a
// single decoded clock/length/clientID in (2^53, 2^63) is rejected by Yjs even
// though it fits a Go int64. We clamp the per-read boundary here (toNumber /
// nonNegNumber) to maxSafeInteger so the Go port refuses exactly what Yjs throws
// on, UNIFORMLY across every single-value decode path — a peer divergence on
// adversarial input is closed. NOTE this is the SINGLE-VALUE read bound only: the
// SUM/accumulation guards (addBounded/addClock/addDsCurVal/addClockSaturating and
// ReadDsLen's +1 framing) stay at math.MaxInt, because lib0 does NOT throw on an
// accumulated sum — it does float64 arithmetic (imprecise above 2^53 but never
// aborting), so a CUMULATIVE struct clock (a sum of many lengths each ≤ 2^53) may
// legitimately exceed 2^53 and must not wrap int64 at 2^63.
//
// Typed int64 (not Number): 2^53-1 is a FIXED constant (it is independent of the
// platform int width), and a `Number`-typed declaration would overflow the
// constant on a 32-bit build (Number == int32 there). The read-clamp comparisons
// against it are width-independent (the decoded raw is uint64/int64), and on the
// 32-bit build the narrowing Number(v) is additionally floored at math.MaxInt
// (== MaxInt32 there, < maxSafeInteger) so an in-(MaxInt32, 2^53] value cannot
// truncate — a 32-bit width guard, NOT the 2^63 read-clamp (that is replaced).
const maxSafeInteger int64 = 9007199254740991 // 2^53 - 1, lib0/Yjs Number.MAX_SAFE_INTEGER; readVarUint throws "Integer out of Range" above this

// toNumber converts a decoded VarUint to a non-negative Number, rejecting any
// value that lib0's readVarUint would THROW "Integer out of Range" on — i.e. any
// SINGLE decoded value > maxSafeInteger (2^53-1). This both (a) prevents a value
// in [2^63, 2^64) from wrapping to a NEGATIVE Number (a negative clock smuggled
// into a state vector defeats the writeClientsStructs "GetState > clock" filter
// and drives WriteStructs to deref a nil struct slice — K3), and (b) makes the Go
// port refuse the SAME (2^53, 2^63) window Yjs aborts on, so the two peers
// converge on adversarial input. Legitimate clocks/clients/lengths are tiny
// (clocks < 2^53, client ids < 2^32), so the 2^53 bound cannot false-reject real
// data. This is the per-read SINGLE-VALUE clamp; the accumulation guards stay at
// the int width (addClock/addBounded) — see maxSafeInteger's note.
//
// The trailing round-trip `uint64(Number(v)) != v` is the 32-bit width floor: on
// the 64-bit target it is a dead no-op (2^53-1 < MaxInt64, so Number(v) is always
// exact after the first check), but on a 32-bit build (Number == int32) it
// rejects an in-(MaxInt32, 2^53-1] value that the narrowing conversion would
// otherwise truncate — preserving the prior reject-don't-truncate contract (D)
// without naming math.MaxInt in the read clamp.
func toNumber(v uint64) (Number, error) {
	if v > uint64(maxSafeInteger) || uint64(Number(v)) != v {
		return 0, errNumberOverflow
	}
	return Number(v), nil
}

// addBounded returns a+b, or the BARE errNumberOverflow sentinel if the sum would
// overflow a non-negative Number (a+b > math.MaxInt). It is the single home for
// the REJECT-on-overflow accumulation shared by the struct-store clock advance
// (addClock) and the V2 delete-set accumulator (DSDecoderV2.addDsCurVal) — F#1/G#1.
// Both operands are >= 0 at every call site (decoded values pass through the
// per-read toNumber clamp first), so the sum wraps iff b > MaxInt-a; this
// pre-check is exact. The body holds NO fmt.Errorf — returning the hoisted
// sentinel keeps the inline cost under budget so addBounded inlines at every
// call site (the same fix the errNumberOverflow sentinel gave toNumber, round 7);
// callers add their own context to the returned error (or wrap it via %w).
func addBounded(a, b Number) (Number, error) {
	if b > math.MaxInt-a {
		return 0, errNumberOverflow
	}
	return a + b, nil
}

// addClock advances a struct-store clock accumulator by a decoded length/clock
// delta, rejecting the running-sum overflow that the per-read clamp cannot catch
// (H#1, the arithmetic boundary). toNumber/readVarUintAsNumber bound EACH decoded
// length to [0, maxSafeInteger], but the struct readers run `clock += length`
// (eager readClientsStructRefs and the lazy struct reader): many lengths each
// <= 2^53 can SUM past MaxInt and wrap clock NEGATIVE, and that negative clock
// reaches the decode output (ParseUpdateMeta / EncodeStateVectorFromUpdate)
// silently, poisoning sync. Because both operands are >= 0 by the per-read clamp,
// the sum wraps iff it exceeds MaxInt, so this no-overflow pre-check is exact
// (delegated to addBounded) — it mirrors the V2 DsCurrVal accumulator guard
// (addDsCurVal). REJECT (not saturate) is correct here: a wrapped struct clock
// crashes the integrate path. Returns the new clock, or errNumberOverflow if the
// addition would overflow.
//
// The bound here is MaxInt (NOT maxSafeInteger): lib0 throws only on a single
// VARINT read > 2^53, never on an accumulated sum (it adds in float64, which is
// imprecise above 2^53 but does not abort), so a CUMULATIVE clock in (2^53, 2^63)
// — built from many in-range deltas — is legitimate and must not false-reject
// here. DOCUMENTED RESIDUAL: a doc whose cumulative struct clock exceeds 2^53
// (absurd in practice: > 2^53 operations) is the one place Go (precise int64) and
// Yjs (imprecise float64) can DIVERGE — Yjs's clock arithmetic silently loses
// precision above 2^53 while Go's stays exact. We accept the precise side and
// reject only at the int64-wrap boundary (2^63), where divergence is unavoidable
// regardless; clamping this sum guard down to 2^53 would instead false-reject a
// (2^53, 2^63) cumulative clock that Yjs would still apply.
func addClock(clock, delta Number) (Number, error) {
	return addBounded(clock, delta)
}

// addClockSaturating returns clock+length for a delete-RANGE END, SATURATING to
// math.MaxInt on overflow instead of rejecting (H8). It is a COMPARISON bound for
// the delete-set membership/merge/iterate sites (FindIndexDS, SortAndMergeDeleteSet,
// IterateStructs), distinct from the struct-store accumulator (addClock, which
// REJECTS a wrapped clock because a negative struct clock crashes the integrate
// path).
//
// CORRECTION (9th review): this saturate is now an UNREACHABLE DEFENSIVE FALLBACK,
// not a Yjs-matching behavior. After the per-read clamp dropped to maxSafeInteger
// (toNumber/nonNegNumber reject a single decoded DS clock/length > 2^53), every
// clock and length reaching these sites is <= 2^53-1, so the sum is <= 2^54-2,
// FAR below math.MaxInt — the overflow branch can no longer fire on decoded input.
// The earlier doc claimed saturating "matches Yjs (float64 delete-to-end)"; that
// is WRONG: lib0's readVarUint THROWS "Integer out of Range" at DECODE on every
// path (including snapshot/permanent-user-data), so Yjs never reaches a
// membership/merge/iterate site with an over-range value at all — there is nothing
// for the saturate to "match". It survives only as defense-in-depth against a
// future caller that constructs a DeleteItem WITHOUT going through the decode
// clamp (e.g. directly via AddToDeleteSet, as the H8 unit tests do). Both operands
// are >= 0, so the sum overflows iff length > MaxInt-clock — this pre-check is
// exact. Bound stays math.MaxInt (a SUM/wrap guard, not a single-value read clamp).
func addClockSaturating(clock, length Number) Number {
	if length > math.MaxInt-clock {
		return math.MaxInt
	}
	return clock + length
}

// nonNegNumber validates an already-int64-typed decoded value (e.g. a clock from
// the V2 IntDiffOptRle clock columns, which accumulate diffs into a signed int64
// and can run NEGATIVE on hostile input) as a non-negative Number. The
// IntDiffOptRle decoder already guards its own int64 overflow, so the remaining
// hostile cases are a negative running clock (reject < 0) and a value above
// maxSafeInteger (2^53-1). The upper bound is maxSafeInteger — not math.MaxInt —
// so this single decoded clock is rejected on exactly the (2^53, 2^63) window
// lib0's readVarUint throws "Integer out of Range" on, matching toNumber and
// keeping the V2 IntDiffOptRle clock path convergent with Yjs. The trailing
// round-trip `int64(Number(v)) != v` is the 32-bit width floor (dead on the
// 64-bit target where 2^53-1 < MaxInt64; on a 32-bit build it rejects an
// in-(MaxInt32, 2^53-1] value the narrowing conversion would truncate, preserving
// the prior reject-don't-truncate contract — D). Reject all so the value never
// reaches the delete-set / struct-store logic (N1/D). Real clocks are
// non-negative and < 2^53.
func nonNegNumber(v int64) (Number, error) {
	if v < 0 || v > maxSafeInteger || int64(Number(v)) != v {
		return 0, errNumberOverflow
	}
	return Number(v), nil
}

// Read state vector from Decoder and return as Map.
//
// It surfaces a truncation error (like ReadDeleteSet) instead of swallowing it:
// a truncated state vector previously yielded a wrong short/zero map silently
// (each readVarUint error was ignored), which a caller would treat as a valid —
// but wrong — state. Every per-entry read error is now checked and returned.
//
// Each decoded length/client/clock is run through toNumber so a varuint in
// [2^63, 2^64) is rejected rather than wrapped to a NEGATIVE Number (K3): a
// negative clock would otherwise pass the writeClientsStructs filter and crash
// WriteStructs on a nil struct slice.
// readStateVector reads a state vector through a DS-level decoder (it uses only
// the varint rest-framing, identical for V1 and V2). Narrowed from UpdateDecoder
// to DSDecoder so a bare DSDecoderV2 (snapshots) can be passed; UpdateDecoder
// still satisfies DSDecoder, so existing callers are unaffected.
func readStateVector(decoder dsDecoder) (map[Number]Number, error) {
	rest := decoder.restDecoder()
	ss := make(map[Number]Number)
	// Route length/client/clock through the single guarded reader (F#1): it is
	// readVarUint+toNumber in one step (behavior-identical to the three open-coded
	// pairs this replaced), so a varuint in [2^63, 2^64) is rejected here too and
	// the negative-wrap clamp lives in exactly one place.
	ssLength, err := readVarUintAsNumber(rest)
	if err != nil {
		return nil, fmt.Errorf("read state vector: length: %w", err)
	}

	for i := 0; i < ssLength; i++ {
		client, err := readVarUintAsNumber(rest)
		if err != nil {
			return nil, fmt.Errorf("read state vector: client[%d]: %w", i, err)
		}

		clock, err := readVarUintAsNumber(rest)
		if err != nil {
			return nil, fmt.Errorf("read state vector: clock[%d]: %w", i, err)
		}

		ss[client] = clock
	}

	return ss, nil
}

// Read decodedState and return State as Map.
func decodeStateVector(decodedState []uint8) (map[Number]Number, error) {
	return readStateVector(newUpdateDecoderV1(decodedState))
}

// DecodeStateVector decodes a state-vector payload into its client clocks.
func DecodeStateVector(decodedState []uint8) (map[Number]Number, error) {
	return decodeStateVector(decodedState)
}

// writeStateVector writes a state vector through a DS-level encoder (varint
// rest-framing, identical for V1 and V2). Narrowed from UpdateEncoder to DSEncoder
// so a bare DSEncoderV2 (snapshots) can be passed; UpdateEncoder still satisfies
// DSEncoder, so existing callers are unaffected.
func writeStateVector(encoder dsEncoder, sv map[Number]Number) {
	rest := encoder.restEncoder()
	writeVarUint(rest, uint64(len(sv)))
	// Iterate clients in descending order so the output is deterministic (Go map
	// iteration order is randomized) and matches the JS reference byte stream;
	// this keeps the encoded state vector stable across runs.
	mapSortedRange(sv, false, func(client, clock Number) {
		writeVarUint(rest, uint64(client))
		writeVarUint(rest, uint64(clock))
	})
}

func writeDocumentStateVector(encoder updateEncoder, doc *Doc) {
	writeStateVector(encoder, getStateVector(doc.store))
}

func encodeStateVectorUsing(doc *Doc, m map[Number]Number, encoder updateEncoder) []uint8 {
	if m != nil {
		writeStateVector(encoder, m)
	} else {
		writeDocumentStateVector(encoder, doc)
	}

	return encoder.toBytes()
}

func encodeStateVectorWith(doc *Doc, m map[Number]Number, encoder updateEncoder) []uint8 {
	return encodeStateVectorUsing(doc, m, encoder)
}

// EncodeStateVector encodes the document's current state vector in the
// canonical V1/lib0 representation used by sync step 1. Encoder selection is
// an implementation detail; callers should not need a codec object merely to
// ask which structs a document contains.
func EncodeStateVector(doc *Doc) []byte {
	return encodeStateVectorWith(doc, nil, newUpdateEncoderV1())
}
