package crdt

import "sync/atomic"

// Event that describes the changes on a YArray
type YArrayEvent struct {
	YEvent
	YTrans *Transaction
}

func newYArrayEvent(yarray *YArray, trans *Transaction) *YArrayEvent {
	y := &YArrayEvent{
		YEvent: *newYEvent(yarray, trans),
		YTrans: trans,
	}

	return y
}

// A shared Array implementation.
type YArray struct {
	abstractTypeBase
	prelimContent ArrayAny
	readIndex     atomic.Pointer[listReadIndex]
}

// Construct a new YArray containing the specified items.
func (y *YArray) From(items ArrayAny) *YArray {
	a := NewYArray()
	a.Push(items)
	return a
}

// integrate this type into the Yjs instance.
//
//	Save this struct in the os
//	This type is sent to other client
//	Observer functions are fired
func (y *YArray) integrate(doc *Doc, item *itemStruct) {
	y.abstractTypeBase.integrate(doc, item)
	y.Insert(0, y.prelimContent)
	y.prelimContent = nil
}

func (y *YArray) copyType() abstractType {
	return NewYArray()
}

func (y *YArray) cloneType() abstractType {
	arr := NewYArray()

	var content []interface{}
	for _, el := range y.ToArray() {
		a, ok := el.(abstractType)
		if ok {
			content = append(content, a.cloneType())
		} else {
			content = append(content, el)
		}
	}

	arr.Insert(0, content)
	return arr
}

// Clone returns an independent YArray with cloned nested shared values.
func (y *YArray) Clone() *YArray { return y.cloneType().(*YArray) }

func (y *YArray) GetLength() Number {
	if y.prelimContent == nil {
		return y.length
	}

	return len(y.prelimContent)
}

// Creates YArrayEvent and calls observers.
func (y *YArray) callObserver(trans *Transaction, parentSubs ChangedSubs) {
	y.abstractTypeBase.callObserver(trans, parentSubs)
	if hasTypeObservers(y) {
		callTypeObservers(y, trans, newYArrayEvent(y, trans))
	}
}

// Inserts new content at an index.
//
// Important: This function expects an array of content. Not just a content
// object. The reason for this "weirdness" is that inserting several elements
// is very efficient when it is done as a single operation.
//
//	@example
//	 // Insert character 'a' at position 0
//	 yarray.insert(0, ['a'])
//	 // Insert numbers 1, 2 at position 1
//	 yarray.insert(1, [1, 2])
func (y *YArray) Insert(index Number, content ArrayAny) {
	if y.doc != nil {
		transactMutation(y.doc, func(trans *Transaction) {
			_ = typeListInsertGenerics(trans, y, index, content)
		}, nil, true)
	} else {
		spliceArray(&y.prelimContent, index, 0, content)
	}
}

// Push appends content to this YArray.
//
// Routed through typeListPushGenerics rather than Insert(length): appending must land after
// trailing TOMBSTONES, which an index-based insert does not do. See typeListPushGenerics.
func (y *YArray) Push(content ArrayAny) {
	if y.doc != nil {
		transactMutation(y.doc, func(trans *Transaction) {
			_ = typeListPushGenerics(trans, y, content)
		}, nil, true)
	} else {
		y.prelimContent = append(y.prelimContent, content...)
	}
}

// Preppends content to this YArray.
func (y *YArray) Unshift(content ArrayAny) {
	y.Insert(0, content)
}

// Deletes elements starting from an index.
func (y *YArray) Delete(index, length Number) {
	if y.doc != nil {
		transactMutation(y.doc, func(trans *Transaction) {
			_ = typeListDelete(trans, y, index, length)
		}, nil, true)
	} else {
		spliceArray(&y.prelimContent, index, length, nil)
	}
}

// Returns the i-th element from a YArray.
func (y *YArray) Get(index Number) interface{} {
	return typeListGet(y, index)
}

// Transforms this YArray to a JavaScript Array.
func (y *YArray) ToArray() ArrayAny {
	return typeListToArray(y)
}

// Transforms this YArray to a JavaScript Array.
func (y *YArray) Splice(start, end Number) ArrayAny {
	return typeListSlice(y, start, end)
}

// Transforms this Shared Type to a JSON object.
func (y *YArray) ToJson() interface{} {
	result := make(ArrayAny, 0, y.GetLength())
	for item := y.start; item != nil; item = item.right {
		if item.countable() && !item.isDeleted() {
			content := item.content.contentValues()
			for _, value := range content {
				if shared, ok := value.(abstractType); ok {
					result = append(result, shared.ToJson())
				} else {
					result = append(result, value)
				}
			}
		}
	}
	return result
}

// Returns an Array with the result of calling a provided function on every
// element of this YArray.
func (y *YArray) Map(f func(interface{}, Number, *YArray) interface{}) ArrayAny {
	if item := y.start; item != nil && item.right == nil && !item.isDeleted() && item.countable() {
		if content, ok := item.content.(*contentAny); ok {
			result := make(ArrayAny, len(content.arr))
			for i, value := range content.arr {
				result[i] = f(value, i, y)
			}
			return result
		}
	}
	return typeListMap(y, func(value interface{}, index Number, _ abstractType) interface{} {
		return f(value, index, y)
	})
}

// Executes a provided function on once on overy element of this YArray.
func (y *YArray) ForEach(f func(interface{}, Number, *YArray)) {
	if item := y.start; item != nil && item.right == nil && !item.isDeleted() && item.countable() {
		if content, ok := item.content.(*contentAny); ok {
			for i, value := range content.arr {
				f(value, i, y)
			}
			return
		}
	}
	typeListForEach(y, func(value interface{}, index Number, _ abstractType) {
		f(value, index, y)
	})
}

func (y *YArray) rangeItems(f func(item *itemStruct)) {
	n := y.start
	for ; n != nil; n = n.right {
		if !n.isDeleted() {
			f(n)
		}
	}
}

func (y *YArray) writeType(encoder updateEncoder) {
	encoder.writeTypeRef(yArrayRefID)
}

func NewYArray() *YArray {
	a := &YArray{}
	a.typeMap = make(map[string]*itemStruct)
	// yjs YArray constructor: `this._searchMarker = []` (markers ENABLED).
	a.searchMarker = []*arraySearchMarker{}
	return a
}

func newYArrayType() SharedType {
	return NewYArray()
}
