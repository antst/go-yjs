package crdt

// jsonString serializes an awareness state with JSON.stringify semantics:
// insertion-ordered object keys for the ordered Object type, so a multi-key
// awareness state re-encodes byte-identically to what JS produced (plain
// json.Marshal would sort the keys). Scalars/arrays are unchanged.
func jsonString(object interface{}) string {
	data, err := marshalJSONOrdered(object)
	if err != nil {
		return ""
	}

	return string(data)
}

// jsonObject parses an awareness state into order-preserving values (JSON objects
// become the ordered Object type), so on-wire key order survives a decode/encode
// round-trip. A cleared/null state (JSON null, which unmarshalJSONOrdered decodes
// to lib0 Null) is returned as a bare Go nil so awareness callers can detect it
// via `raw != nil` — matching the previous json.Unmarshal-into-any behavior.
func jsonObject(data string) interface{} {
	object, err := unmarshalJSONOrdered([]byte(data))
	if err != nil {
		// todo trace error
		return nil
	}

	if _, isNull := object.(NullType); isNull {
		return nil
	}

	return object
}

func awarenessStatesKeys(states map[Number]Object) []Number {
	v := make([]Number, 0, len(states))
	for k := range states {
		v = append(v, k)
	}
	return v
}
