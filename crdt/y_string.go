package crdt

type yString struct {
	abstractTypeBase
	str string
}

func (str *yString) GetLength() Number {
	return len(str.str)
}

func (str *yString) getItem() *itemStruct {
	return nil
}

func (str *yString) getMap() map[string]*itemStruct {
	return nil
}

func (str *yString) startItem() *itemStruct {
	return nil
}

func (str *yString) setStartItem(item *itemStruct) {

}

func (str *yString) getDoc() *Doc {
	return nil
}

func (str *yString) updateLength(n Number) {

}

func (str *yString) setSearchMarker(mark []*arraySearchMarker) {

}

func (str *yString) parentType() abstractType {
	return nil
}

func (str *yString) integrate(doc *Doc, item *itemStruct) {

}

func (str *yString) copyType() abstractType {
	return nil
}

func (str *yString) cloneType() abstractType {
	return nil
}

func (str *yString) writeType(encoder updateEncoder) {

}

func (str *yString) firstItem() *itemStruct {
	return nil
}

func (str *yString) callObserver(trans *Transaction, parentSubs ChangedSubs) {

}

func (str *yString) Observe(f func(interface{}, interface{})) {

}

func (str *yString) ObserveDeep(f func(interface{}, interface{})) {

}

func (str *yString) Unobserve(f func(interface{}, interface{})) {

}

func (str *yString) UnobserveDeep(f func(interface{}, interface{})) {

}

func (str *yString) toJSONValue() interface{} { return str.ToJSON() }

func (str *yString) ToJSON() interface{} {
	return ""
}

func newYString(str string) *yString {
	ystr := &yString{
		str: str,
	}

	return ystr
}
