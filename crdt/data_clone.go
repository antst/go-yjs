package crdt

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ErrUnsupportedDataValue marks a value outside the JSON-shaped data domain
// understood by cloneDataValue. Callers must reject such a value rather than
// silently retain a mutable reference that defeats an ownership boundary.
var ErrUnsupportedDataValue = errors.New("unsupported data value")

// cloneDataObject makes a deep, insertion-order-preserving copy of o without
// reflection. It is deliberately reusable outside awareness: Object.Clone can
// adopt the same walker separately once that change has been measured and
// reviewed on its own merits.
func cloneDataObject(o Object) (Object, error) {
	return cloneDataObjectDepth(o, 0)
}

func cloneDataObjectDepth(o Object, depth int) (Object, error) {
	if depth > maxAnyDepth {
		return Object{}, fmt.Errorf("%w: nesting depth exceeds %d", ErrUnsupportedDataValue, maxAnyDepth)
	}
	if o.d == nil {
		return Object{}, nil
	}

	if o.d.large == nil {
		out := Object{d: &objectData{
			smallKeys: o.d.smallKeys,
			smallLen:  o.d.smallLen,
		}}
		for i := 0; i < int(o.d.smallLen); i++ {
			value, err := cloneDataValue(o.d.smallValues[i], depth+1)
			if err != nil {
				return Object{}, fmt.Errorf("object key %q: %w", o.d.smallKeys[i], err)
			}
			out.d.smallValues[i] = value
		}
		return out, nil
	}

	out := newObjectWithCapacity(len(o.d.large.keys))
	for _, key := range o.d.large.keys {
		value, err := cloneDataValue(o.d.large.m[key], depth+1)
		if err != nil {
			return Object{}, fmt.Errorf("object key %q: %w", key, err)
		}
		out.appendNew(key, value)
	}
	return out, nil
}

// cloneDataValue deeply copies the mutable containers admitted by the library's
// JSON/any data model. Immutable scalars are returned directly. Unknown values
// are errors: sharing an unrecognized pointer, map, or slice would make the
// caller believe it owns a copy while retaining the original mutable storage.
func cloneDataValue(value any, depth int) (any, error) {
	if depth > maxAnyDepth {
		return nil, fmt.Errorf("%w: nesting depth exceeds %d", ErrUnsupportedDataValue, maxAnyDepth)
	}

	switch value := value.(type) {
	case nil,
		UndefinedType, NullType,
		bool, string,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64,
		json.Number:
		return value, nil
	case Object:
		return cloneDataObjectDepth(value, depth)
	case *Object:
		if value == nil {
			return (*Object)(nil), nil
		}
		out, err := cloneDataObjectDepth(*value, depth)
		if err != nil {
			return nil, err
		}
		return &out, nil
	case []byte:
		if value == nil {
			return []byte(nil), nil
		}
		out := make([]byte, len(value))
		copy(out, value)
		return out, nil
	case []any:
		if value == nil {
			return []any(nil), nil
		}
		out := make([]any, len(value))
		for i, element := range value {
			cloned, err := cloneDataValue(element, depth+1)
			if err != nil {
				return nil, fmt.Errorf("array index %d: %w", i, err)
			}
			out[i] = cloned
		}
		return out, nil
	case map[string]any:
		if value == nil {
			return map[string]any(nil), nil
		}
		out := make(map[string]any, len(value))
		for key, element := range value {
			cloned, err := cloneDataValue(element, depth+1)
			if err != nil {
				return nil, fmt.Errorf("map key %q: %w", key, err)
			}
			out[key] = cloned
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%w: %T", ErrUnsupportedDataValue, value)
	}
}

// mustCloneDataObject is for boundaries whose source is already protected by
// the package invariant established on insertion. Panicking is intentional: an
// unsupported value here means an internal path bypassed that invariant, and
// returning a shared reference would silently reintroduce the race this copier
// exists to prevent.
func mustCloneDataObject(o Object) Object {
	out, err := cloneDataObject(o)
	if err != nil {
		panic(fmt.Sprintf("y_crdt: internal Object ownership invariant violated: %v", err))
	}
	return out
}
