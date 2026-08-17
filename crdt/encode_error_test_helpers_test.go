package crdt

// encode_error_test_helpers_test.go provides thin "must" wrappers used by the
// existing tests after the encode/convert/diff/state-vector entry points gained
// error returns (the error-threading-completion change). On the success path
// they assert err == nil and return the value, so existing assertions over the
// returned bytes/maps stay unchanged; a regression that reintroduces a silent
// failure surfaces here as a panic (a failed test) instead of a wrong/empty
// result.
//
// They deliberately take no *testing.T so they can be written in the Go
// multi-value spread form mustBytes(F(...)) — F returning (value, error) — which
// reads at the call sites exactly like the old single-value calls. A panic here
// only fires on the success-path tests, where the encode never legitimately
// fails, so it is an unambiguous regression signal.

// mustBytes returns the bytes from a ([]uint8, error) call, panicking on error.
func mustBytes(b []uint8, err error) []uint8 {
	if err != nil {
		panic("unexpected encode error in test: " + err.Error())
	}
	return b
}

// mustSV returns the state-vector map from a (map, error) call, panicking on error.
func mustSV(m map[Number]Number, err error) map[Number]Number {
	if err != nil {
		panic("unexpected state-vector decode error in test: " + err.Error())
	}
	return m
}
