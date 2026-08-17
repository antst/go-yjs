//go:build !structstoreoracle

package crdt

const clientStructListOracleEnabled = false

type clientStructListOracle struct{}

func (*clientStructListOracle) insert(abstractStruct)                      {}
func (*clientStructListOracle) remove([]abstractStruct)                    {}
func (*clientStructListOracle) removeRange(abstractStruct, abstractStruct) {}
func (*clientStructListOracle) replace(abstractStruct, abstractStruct)     {}
func (*clientStructListOracle) checkList(*clientStructList)                {}
func (*clientStructListOracle) checkFind(Number, abstractStruct, bool)     {}
