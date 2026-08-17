package crdt

const (
	ActionAdd    = "add"
	ActionDelete = "delete"
	ActionUpdate = "update"
)

type EventAction struct {
	Action   string
	OldValue interface{}
	NewValue interface{}
}

type EventOperator struct {
	// InsertText and Insert are the two arms of an insert. Keeping text typed
	// avoids boxing every rendered string into an interface; Insert carries only
	// embeds and nested shared types.
	InsertText string
	Insert     interface{}
	Length     Number
	Attributes Object
	Kind       EventOperatorKind
}

// EventOperatorKind identifies the single operation carried by an EventOperator.
// None is deliberately the zero value so an uninitialized operator remains a no-op.
type EventOperatorKind uint8

const (
	EventOperatorNone EventOperatorKind = iota
	EventOperatorInsertText
	EventOperatorInsertValue
	EventOperatorRetain
	EventOperatorDelete
)

// GetKind returns the operation represented by op. Delta operators are a tagged
// union in both the Yjs model and this Go representation; the accessor keeps
// callers independent of its physical layout.
func (op EventOperator) GetKind() EventOperatorKind {
	return op.Kind
}

func (op EventOperator) IsInsert() bool {
	return op.Kind == EventOperatorInsertText || op.Kind == EventOperatorInsertValue
}

// InsertValue returns the inserted text, embed, or nested type. Call it only for
// an insert operator.
func (op EventOperator) InsertValue() any {
	if op.Kind == EventOperatorInsertText {
		return op.InsertText
	}
	if op.Kind == EventOperatorInsertValue {
		return op.Insert
	}
	return nil
}

// OpLength returns the retain or delete length. It returns zero for other kinds.
func (op EventOperator) OpLength() Number {
	if op.Kind == EventOperatorRetain || op.Kind == EventOperatorDelete {
		return op.Length
	}
	return 0
}

func (op EventOperator) HasAttributes() bool { return !op.Attributes.IsNil() }

// NewTextDeltaOp constructs a text insert, with Object{} meaning omitted attributes.
func NewTextDeltaOp(text string, attributes Object) EventOperator {
	return EventOperator{InsertText: text, Attributes: attributes, Kind: EventOperatorInsertText}
}

// NewValueDeltaOp constructs an embed or nested-type insert.
func NewValueDeltaOp(value any, attributes Object) EventOperator {
	return EventOperator{Insert: value, Attributes: attributes, Kind: EventOperatorInsertValue}
}

// NewRetainDeltaOp constructs a retain, optionally carrying formatting attributes.
func NewRetainDeltaOp(length Number, attributes Object) EventOperator {
	return EventOperator{Length: length, Attributes: attributes, Kind: EventOperatorRetain}
}

// NewDeleteDeltaOp constructs a delete.
func NewDeleteDeltaOp(length Number) EventOperator {
	return EventOperator{Length: length, Kind: EventOperatorDelete}
}

type IEventType interface {
	GetTarget() SharedType
	GetCurrentTarget() SharedType
	Path() []interface{}
	targetType() abstractType
	setCurrentTarget(abstractType)
}

// YEvent describes the changes on a YType.
type YEvent struct {
	target        abstractType // The type on which this event was created on.
	currentTarget abstractType // The current target on which the observe callback is called.
	transaction   *Transaction // The transaction that triggered this event.
	Changes       Object
	Keys          map[string]EventAction // Map<string, { action: 'add' | 'update' | 'delete', oldValue: any, newValue: any }>}
	delta         []EventOperator
}

func (y *YEvent) GetTarget() SharedType {
	return asSharedType(y.target)
}

func (y *YEvent) GetCurrentTarget() SharedType {
	return asSharedType(y.currentTarget)
}

func (y *YEvent) targetType() abstractType { return y.target }

func (y *YEvent) setCurrentTarget(t abstractType) { y.currentTarget = t }

// Computes the path from `y` to the changed type.
//
// @todo v14 should standardize on path: Array<{parent, index}> because that is easier to work with.
//
// The following property holds:
// @example
// ----------------------------------------------------------------------------
//
//	let type = y
//	event.path.forEach(dir => {
//	  type = type.get(dir)
//	})
//	type === event.target // => true
//
// ----------------------------------------------------------------------------
func (y *YEvent) Path() []interface{} {
	return getPathTo(y.currentTarget, y.target)
}

// Check if a struct is deleted by this event.
// In contrast to change.deleted, this method also returns true if the struct was added and then deleted.
func (y *YEvent) deletesStruct(s abstractStruct) bool {
	return isDeleted(y.transaction.deleteSet, s.getID())
}

func (y *YEvent) GetKeys() map[string]EventAction {
	if y.Keys == nil {
		keys := make(map[string]EventAction)
		target := y.target
		changed := y.transaction.changedTypesInternal()[target]
		for strKey := range changed {
			if strKey != "" {
				item := target.getMap()[strKey]

				var action string
				var oldValue interface{}
				var err error

				if y.addsStruct(item) {
					prev := item.left
					for prev != nil && y.addsStruct(prev) {
						prev = prev.left
					}

					if y.deletesStruct(item) {
						if prev != nil && y.deletesStruct(prev) {
							action = ActionDelete
							oldValue, err = arrayLast(prev.content.contentValues())
							if err != nil {
								return nil
							}
						} else {
							// yjs returns from the changed.forEach callback here: skip
							// this key, but keep computing the rest of the projection.
							continue
						}
					} else {
						if prev != nil && y.deletesStruct(prev) {
							action = ActionUpdate
							oldValue, err = arrayLast(prev.content.contentValues())
							if err != nil {
								return nil
							}
						} else {
							action = ActionAdd
							oldValue = nil
						}
					}
				} else {
					if y.deletesStruct(item) {
						action = ActionDelete
						oldValue, err = arrayLast(item.content.contentValues())
						if err != nil {
							return nil
						}
					} else {
						// Same callback-local return as the reference: this key is a
						// no-op for the event, not a reason to discard every key.
						continue
					}
				}

				keys[strKey] = EventAction{
					Action:   action,
					OldValue: oldValue,
				}
			}
		}

		y.Keys = keys
	}

	return y.Keys
}

func (y *YEvent) GetDelta() []EventOperator {
	return y.GetChanges().GetOr("delta").([]EventOperator)
}

// Check if a struct is added by this event.
// In contrast to change.deleted, this method also returns true if the struct was added and then deleted.
func (y *YEvent) addsStruct(s abstractStruct) bool {
	return s.getID().Clock >= y.transaction.beforeState[s.getID().Client]
}

func (y *YEvent) GetChanges() Object {
	changes := y.Changes
	if changes.IsNil() || changes.Len() == 0 {
		target := y.target
		added := NewSet()
		deleted := NewSet()
		var delta []EventOperator

		changes = newObject()
		changes.Set("added", added)
		changes.Set("deleted", deleted)
		changes.Set("keys", y.GetKeys())

		changed := y.transaction.changedTypesInternal()[target]

		if changed.Has("") {
			var lastOp *EventOperator
			packOp := func() {
				if lastOp != nil {
					delta = append(delta, *lastOp)
				}
			}

			for item := target.startItem(); item != nil; item = item.right {
				if item.isDeleted() {
					if y.deletesStruct(item) && !y.addsStruct(item) {
						if lastOp == nil || lastOp.Kind != EventOperatorDelete {
							packOp()
							lastOp = &EventOperator{Kind: EventOperatorDelete}
						}
						lastOp.Length += item.length
						deleted.Add(item)
					} // else nop
				} else {
					if y.addsStruct(item) {
						if lastOp == nil || !lastOp.IsInsert() {
							packOp()

							lastOp = &EventOperator{
								Insert: ArrayAny{},
								Kind:   EventOperatorInsertValue,
							}
						}

						// yjs concatenates the item's content array into the insert op. Appending
						// the slice as one interface value produces a nested [[value]] delta.
						lastOp.Insert = append(lastOp.Insert.(ArrayAny), item.content.contentValues()...)
						added.Add(item)
					} else {
						if lastOp == nil || lastOp.Kind != EventOperatorRetain {
							packOp()

							lastOp = &EventOperator{Kind: EventOperatorRetain}
						}
						lastOp.Length += item.length
					}
				}
			}

			if lastOp != nil && lastOp.Kind != EventOperatorRetain {
				packOp()
			}
		}

		changes.Set("delta", delta)
		y.Changes = changes
	}

	return changes
}

func newYEvent(target abstractType, trans *Transaction) *YEvent {
	return &YEvent{
		target:        target,
		currentTarget: target,
		transaction:   trans,
		Changes:       newObject(),
	}
}

// Compute the path from this type to the specified target.
//
// @example
// ----------------------------------------------------------------------------
//
//	// `child` should be accessible via `type.get(path[0]).get(path[1])..`
//	const path = type.getPathTo(child)
//	// assuming `type instanceof YArray`
//	console.Log(path) // might look like => [2, 'key1']
//	child === type.get(path[0]).get(path[1])
//
// ----------------------------------------------------------------------------
func getPathTo(parent abstractType, child abstractType) []interface{} {
	// Walk from the target toward the requested parent, then reverse once. The
	// old unshift helper appended the previous slice as ONE interface value, so a
	// path deeper than one level became nested ([0 [0 [0]]]) rather than flat.
	// Even a correct prepend would repeatedly copy the growing prefix; append +
	// reverse is linear in the path depth.
	path := []interface{}{}

	for child.getItem() != nil && child != parent {
		childItem := child.getItem()
		if childItem.parentSub != "" {
			// parent is map-ish
			path = append(path, childItem.parentSub)
		} else {
			// parent is array-ish
			i := 0
			c := childItem.parent.(abstractType).startItem()
			for c != childItem && c != nil {
				// yjs counts visible values, not physical Items. ContentAny and
				// ContentString commonly coalesce several values into one Item.
				if !c.isDeleted() && c.countable() {
					i += c.length
				}
				c = c.right
			}
			path = append(path, i)
		}

		child = childItem.parent.(abstractType)
	}

	for left, right := 0, len(path)-1; left < right; left, right = left+1, right-1 {
		path[left], path[right] = path[right], path[left]
	}

	return path
}
