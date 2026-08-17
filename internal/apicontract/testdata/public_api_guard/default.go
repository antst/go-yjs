//go:build !structstoreoracle

package public_api_guard

type ExportedType struct {
	Field int
}

func DefaultExport() {}

func SharedExport() {}

func (ExportedType) Method() {}
