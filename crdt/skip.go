package crdt

import "errors"

const structSkipRefNumber = 10

type skipStruct struct {
	abstractStructBase
}

func (s *skipStruct) isDeleted() bool {
	return true
}

func (s *skipStruct) mergeStructWith(right abstractStruct) bool {
	r, ok := right.(*skipStruct)
	if !ok {
		return false
	}

	s.length += r.length
	return true
}

func (s *skipStruct) integrateStruct(trans *Transaction, offset Number) error {
	return nil
}

func (s *skipStruct) writeStruct(encoder updateEncoder, offset Number) error {
	encoder.writeInfo(structSkipRefNumber)
	// write as VarUint because Skips can't make use of predictable length-encoding
	writeEncoderRestVarUint(encoder, uint64(s.length-offset))
	return nil
}

func (s *skipStruct) missingClient(trans *Transaction, store *structStore) (Number, error) {
	return 0, errors.New("gc not support this function")
}

func newSkip(id ID, length Number) *skipStruct {
	return &skipStruct{abstractStructBase{id: id, length: length}}
}
