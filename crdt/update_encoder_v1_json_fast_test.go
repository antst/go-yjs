package crdt

import (
	"bytes"
	"testing"
)

func TestFastV1JSONScalarsMatchOrderedMarshal(t *testing.T) {
	values := []any{
		nil,
		NullType{},
		UndefinedType{},
		true,
		false,
		"plain",
		"long-prefix-" + string(make([]byte, 140)),
		"quote\" slash\\ controls\b\t\n\f\r\x00\x1f",
		"<>&/\u2028\u2029 café",
	}
	for _, value := range values {
		want, err := marshalJSONOrdered(value)
		if err != nil {
			t.Fatal(err)
		}
		encoders := []struct {
			name  string
			write func(any) ([]byte, error)
		}{
			{name: "full-state", write: func(value any) ([]byte, error) {
				encoder := newFastUpdateEncoderV1(newStructStore())
				if err := encoder.writeJSONValue(value); err != nil {
					return nil, err
				}
				return encoder.toBytes(), nil
			}},
			{name: "exported", write: func(value any) ([]byte, error) {
				encoder := newUpdateEncoderV1()
				if err := encoder.writeJSONValue(value); err != nil {
					return nil, err
				}
				return encoder.toBytes(), nil
			}},
		}
		for _, encoder := range encoders {
			encoded, err := encoder.write(value)
			if err != nil {
				t.Fatalf("%s WriteJson(%#v): %v", encoder.name, value, err)
			}
			decoded, err := readVarString(newDecoder(encoded))
			if err != nil {
				t.Fatalf("decode %s WriteJson(%#v): %v", encoder.name, value, err)
			}
			got := decoded.(string)
			if !bytes.Equal([]byte(got), want) {
				t.Fatalf("%s WriteJson(%#v) = %q, want %q", encoder.name, value, got, want)
			}
		}
	}
}
