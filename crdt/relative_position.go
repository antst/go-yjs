package crdt

import (
	"errors"
	"fmt"
)

// A relative position is based on the Yjs model and is not affected by document changes.
// E.g. If you place a relative position before a certain character, it will always point to this character.
// If you place a relative position at the end of a type, it will always point to the end of the type.
//
// A numeric position is often unsuited for user selections, because it does not change when content is inserted
// before or after.
//
// ```Insert(0, 'x')('a|bc') = 'xa|bc'``` Where | is the relative position.
//
// One of the properties must be defined.
//
// @example
//   // Current cursor position is at position 10
//   const relativePosition = createRelativePositionFromIndex(yText, 10)
//   // modify yText
//   yText.insert(0, 'abc')
//   yText.delete(3, 10)
//   // Compute the cursor position
//   const absolutePosition = createAbsolutePositionFromRelativePosition(y, relativePosition)
//   absolutePosition.type === yText // => true
//   console.log('cursor location is ' + absolutePosition.index) // => cursor location is 3

type RelativePosition struct {
	Type  *ID
	Tname string
	Item  *ID

	// A relative position is associated to a specific character. By default
	// assoc >= 0, the relative position is associated to the character
	// after the meant position.
	// I.e. position 1 in 'ab' is associated to character 'b'.
	//
	// If assoc < 0, then the relative position is associated to the caharacter
	// before the meant position.
	Assoc Number
}

func RelativePositionToJSON(rpos *RelativePosition) Object {
	json := newObject()
	if rpos.Type != nil {
		json.Set("type", rpos.Type)
	}

	if rpos.Tname != "" {
		json.Set("tname", rpos.Tname)
	}

	if rpos.Item != nil {
		json.Set("item", rpos.Item)
	}

	json.Set("assoc", rpos.Assoc)
	return json
}

// relPosJSONID accepts either form an ID can arrive in. RelativePositionToJSON
// emits *ID, but an Object rebuilt from actual JSON, or assembled by hand, may
// carry a value ID; both are unambiguous, so both are read.
func relPosJSONID(field string, v any) (*ID, error) {
	switch id := v.(type) {
	case *ID:
		if id == nil {
			return nil, fmt.Errorf("relative position json: %q is a nil *ID", field)
		}
		copied := *id
		return &copied, nil
	case ID:
		return &id, nil
	default:
		return nil, fmt.Errorf("relative position json: %q has type %T, want ID or *ID", field, v)
	}
}

// CreateRelativePositionFromJSON rebuilds a position from its JSON projection.
//
// It returns an error because it used to assert `v.(ID)` on a field that
// RelativePositionToJSON writes as *ID: our own round trip panicked with
// "interface conversion: interface {} is *y_crdt.ID, not y_crdt.ID". The two
// halves of one projection disagreed about their own format, and nothing caught
// it because nothing round-tripped them. The assertions on tname and assoc were
// equally bare, so a hand-assembled or externally-sourced Object could crash the
// process on any field.
func CreateRelativePositionFromJSON(json Object) (*RelativePosition, error) {
	r := &RelativePosition{}
	if v, exist := json.Get("type"); exist {
		id, err := relPosJSONID("type", v)
		if err != nil {
			return nil, err
		}
		r.Type = id
	}

	if v, exist := json.Get("tname"); exist {
		name, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("relative position json: \"tname\" has type %T, want string", v)
		}
		r.Tname = name
	}

	if v, exist := json.Get("item"); exist {
		id, err := relPosJSONID("item", v)
		if err != nil {
			return nil, err
		}
		r.Item = id
	}

	if v, exist := json.Get("assoc"); exist {
		assoc, ok := v.(Number)
		if !ok {
			return nil, fmt.Errorf("relative position json: \"assoc\" has type %T, want Number", v)
		}
		r.Assoc = assoc
	}
	return r, nil
}

type AbsolutePosition struct {
	Type  SharedType
	Index Number
	Assoc Number
}

func newAbsolutePosition(t abstractType, index, assoc Number) *AbsolutePosition {
	return &AbsolutePosition{
		Type:  asSharedType(t),
		Index: index,
		Assoc: assoc,
	}
}

// NewAbsolutePosition constructs an absolute position against a shared type.
func NewAbsolutePosition(t SharedType, index, assoc Number) *AbsolutePosition {
	return newAbsolutePosition(asAbstractType(t), index, assoc)
}

// newRelativePosition builds a position against t, identifying t by root name
// when it is a root type and by its item ID otherwise.
//
// Type stays nil for a root type. It used to be set unconditionally to the
// address of a zero ID, so a root position carried both Tname and Type{0,0}.
// writeRelativePosition prefers Tname, so the wire form encoded only the name and
// decoding produced Type == nil — meaning a root position never compared equal to
// its own round trip. Anything holding a position across an encode (a saved
// cursor, a position sent to a peer and echoed back) saw them as different
// positions even though both resolve to the same place.
func newRelativePosition(t abstractType, item *ID, assoc Number) *RelativePosition {
	var typeID *ID
	var tname string

	if t.getItem() == nil {
		tname = findRootTypeKey(t)
	} else {
		id := GenID(t.getItem().id.Client, t.getItem().id.Clock)
		typeID = &id
	}

	return &RelativePosition{
		Type:  typeID,
		Tname: tname,
		Item:  item,
		Assoc: assoc,
	}
}

// NewRelativePosition constructs a relative position against a shared type.
func NewRelativePosition(t SharedType, item *ID, assoc Number) *RelativePosition {
	return newRelativePosition(asAbstractType(t), item, assoc)
}

// Create a relativePosition based on a absolute position.
func newRelativePositionFromTypeIndex(tp abstractType, index, assoc Number) *RelativePosition {
	t := tp.startItem()
	if assoc < 0 {
		// associated to the left character or the beginning of a type, increment index if possible.
		if index == 0 {
			return newRelativePosition(tp, nil, assoc)
		}

		index--
	}
	if t != nil && t.right != nil {
		if indexedItem, indexedStart, ok := indexedReadPosition(tp, index); ok {
			t = indexedItem
			index -= indexedStart
		}
	}

	for t != nil {
		if !t.isDeleted() && t.countable() {
			if t.length > index {
				// case 1: found position somewhere in the linked list
				item := GenID(t.id.Client, t.id.Clock+index)
				return newRelativePosition(tp, &item, assoc)
			}
			index -= t.length
		}

		if t.right == nil && assoc < 0 {
			// left-associated position, return last available id
			return newRelativePosition(tp, t.lastID(), assoc)
		}

		t = t.right
	}

	return newRelativePosition(tp, nil, assoc)
}

// NewRelativePositionFromTypeIndex creates a relative position at an index in
// a package-owned shared type.
func NewRelativePositionFromTypeIndex(tp SharedType, index, assoc Number) *RelativePosition {
	return newRelativePositionFromTypeIndex(asAbstractType(tp), index, assoc)
}

func writeRelativePosition(encoder *updateEncoderV1, rpos *RelativePosition) error {
	t, tname, item, assoc := rpos.Type, rpos.Tname, rpos.Item, rpos.Assoc
	switch {
	case item != nil:
		writeVarUint(encoder.rest, 0)
		encoder.writeID(item)
	case tname != "":
		// case 2: found position at the end of the list and type is stored in y.share
		writeByte(encoder.rest, 1)
		_ = encoder.writeStringValue(tname)
	case t != nil:
		// case 3: found position at the end of the list and type is attached to an item
		writeByte(encoder.rest, 2)
		encoder.writeID(t)
	default:
		return errors.New("unexpected case")
	}

	writeVarInt(encoder.rest, assoc)
	return nil
}

func EncodeRelativePosition(rpos *RelativePosition) []uint8 {
	encoder := newUpdateEncoderV1()
	_ = writeRelativePosition(encoder, rpos)
	return encoder.toBytes()
}

// readRelativePosition decodes a relative position, reporting malformed input as
// an error rather than a partly-populated value.
//
// It returns an error because this decoder consumes UNTRUSTED bytes: a relative
// position travels between peers exactly like an update does. Every read here
// used to discard its error and the assoc read used a bare type assertion, so a
// truncated frame did not produce a bad position — it panicked. ReadVarInt
// returns an any that holds a Number on success and a uint64 partial on failure,
// so `v.(Number)` on the failure path crashed the process. An exhaustive sweep of
// every one- and two-byte input panicked on 16,000 of them, the shortest being
// 03 80. A library that decodes network data must never let a remote peer choose
// between "valid" and "crash", which is what an unchecked assertion offers.
//
// The remaining reads propagate for the quieter half of the same defect: with the
// errors dropped, a truncated tag or ID yielded a zero-valued RelativePosition
// that resolves somewhere plausible instead of failing, so corrupt input became a
// silently wrong cursor rather than a rejected frame.
func readRelativePosition(decoder *updateDecoderV1) (*RelativePosition, error) {
	var t *ID
	var tname string
	var itemID *ID
	var assoc Number

	tag, err := readVarUint(decoder.rest)
	if err != nil {
		return nil, fmt.Errorf("read relative position: tag: %w", err)
	}
	switch tag {
	case 0:
		// found position somewhere in the linked list
		id, err := decoder.readID()
		if err != nil {
			return nil, fmt.Errorf("read relative position: item id: %w", err)
		}
		itemID = id

	case 1:
		// found position at the end of the list and type is stored in y.share
		name, err := decoder.readStringValue()
		if err != nil {
			return nil, fmt.Errorf("read relative position: type name: %w", err)
		}
		tname = name

	case 2:
		// found position at the end of the list and type is attached to an item
		id, err := decoder.readID()
		if err != nil {
			return nil, fmt.Errorf("read relative position: type id: %w", err)
		}
		t = id

	default:
		// Yjs writes only 0, 1 or 2. Falling through on anything else produced an
		// empty position that reads as "start of an unnamed root".
		return nil, fmt.Errorf("read relative position: unknown tag %d", tag)
	}

	if hasContent(decoder.rest) {
		v, err := readVarInt(decoder.rest)
		if err != nil {
			return nil, fmt.Errorf("read relative position: assoc: %w", err)
		}
		n, ok := v.(Number)
		if !ok {
			return nil, fmt.Errorf("read relative position: assoc decoded as %T, want Number", v)
		}
		assoc = n
	}

	return &RelativePosition{
		Type:  t,
		Tname: tname,
		Item:  itemID,
		Assoc: assoc,
	}, nil
}

// DecodeRelativePosition decodes a relative position from its wire bytes.
func DecodeRelativePosition(uint8Array []uint8) (*RelativePosition, error) {
	return readRelativePosition(newUpdateDecoderV1(uint8Array))
}

func CreateAbsolutePositionFromRelativePosition(rpos *RelativePosition, doc *Doc) *AbsolutePosition {
	store := doc.store
	rightID := rpos.Item
	typeID := rpos.Type
	tname := rpos.Tname
	assoc := rpos.Assoc

	var t abstractType
	var index Number

	if rightID != nil {
		if getState(store, rightID.Client) <= rightID.Clock {
			return nil
		}

		item, diff := followRedone(store, *rightID)
		right := item
		if right == nil {
			return nil
		}

		t = right.parent.(abstractType)
		if t.getItem() == nil || !t.getItem().isDeleted() {
			indexedStart, indexed := Number(0), false
			if right.left != nil || right.right != nil {
				indexedStart, indexed = indexedVisibleStart(t, right)
			}
			if indexed {
				index = indexedStart
			} else {
				index = 0
				n := right.left
				for n != nil {
					if !n.isDeleted() && n.countable() {
						index += n.length
					}
					n = n.left
				}
			}
			// Adjust from the start of the anchor Item according to association. Deleted and
			// uncountable anchors contribute no visible offset, matching the linked walk.
			if !right.isDeleted() && right.countable() {
				if assoc >= 0 {
					index += diff
				} else {
					index += diff + 1
				}
			}
		}
	} else {
		switch {
		case tname != "":
			// Generic root lookup, matching yjs's `doc.get(rpos.tname)` whose TypeConstructor
			// defaults to AbstractType — it returns whatever type is registered under the name.
			//
			// This used doc.GetMap(tname), which FORCES the YMap constructor: for a root Y.Text,
			// Doc.Get then errors ("already defined with a different constructor"), GetMap turns
			// that into nil, and the deref below segfaulted. Reachable from ordinary use — a
			// position at the end of a root Y.Text has no anchor item, so it is encoded by NAME
			// and lands exactly here.
			existing := doc.getGeneric(tname)
			if existing == nil {
				return nil
			}
			t = existing
		case typeID != nil:
			if getState(store, typeID.Client) <= typeID.Clock {
				// type does not exist yet
				return nil
			}

			item, _ := followRedone(store, *typeID)
			if item != nil && isSameType(item.content, &contentType{}) {
				t = item.content.(*contentType).value
			} else {
				// struct is garbage collected
				return nil
			}
		default:
			return nil
		}

		if assoc >= 0 {
			index = t.GetLength()
		} else {
			index = 0
		}
	}

	return newAbsolutePosition(t, index, rpos.Assoc)
}

func CompareRelativePositions(a, b *RelativePosition) bool {
	return a == b || a != nil && b != nil && a.Tname == b.Tname && CompareIDs(a.Item, b.Item) && CompareIDs(a.Type, b.Type) && a.Assoc == b.Assoc
}
