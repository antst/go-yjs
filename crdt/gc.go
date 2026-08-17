package crdt

import "errors"

const (
	structGCRefNumber = 0
)

type gcStruct struct {
	abstractStructBase
}

func (gc *gcStruct) isDeleted() bool {
	return true
}

func (gc *gcStruct) mergeStructWith(right abstractStruct) bool {
	r, ok := right.(*gcStruct)
	if !ok {
		return false
	}

	gc.length += r.length
	return true
}

func (gc *gcStruct) integrateStruct(trans *Transaction, offset Number) error {
	if offset > 0 {
		gc.id.Clock += offset
		gc.length -= offset
	}
	return addStruct(trans.doc.store, gc)
}

func (gc *gcStruct) writeStruct(encoder updateEncoder, offset Number) error {
	encoder.writeInfo(structGCRefNumber)
	encoder.writeLength(gc.length - offset)
	return nil
}

func (gc *gcStruct) missingClient(trans *Transaction, store *structStore) (Number, error) {
	return 0, errors.New("gc not support this function")
}

func newGC(id ID, length Number) *gcStruct {
	return &gcStruct{
		abstractStructBase{id: id, length: length},
	}
}
