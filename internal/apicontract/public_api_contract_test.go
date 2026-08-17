package apicontract

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const publicAPIContractPath = "testdata/public_api.txt"

// TestPublicAPIContract freezes every exported production identifier in every
// package a consumer can import, WITH ITS SHAPE.
//
// It lives in internal/ because it is a MODULE-WIDE invariant. Nothing here is
// a crdt concern: the contract covers crdt, protocol and each backend package
// at once, and a test that asserts the public API of backend/persistence has no
// business being compiled into the CRDT. A new export is an API decision,
// not an incidental capital letter: update testdata/public_api.txt in the same
// reviewed change. Removing or privatizing an identifier likewise produces an
// explicit contract diff rather than hiding inside a large mechanical rename.
//
// SHAPES ARE RECORDED BECAUSE NAMES ALONE MISSED REAL CHANGES. An earlier
// name-only contract passed unchanged across a refactor in which Doc.GetMap
// narrowed its result from IAbstractType to *YMap and NewDoc dropped its
// gcFilter parameter entirely. Both are breaking changes for a caller and both
// were invisible: the entries `root method Doc.GetMap` and `root func NewDoc`
// were identical before and after. Interfaces are signature commitments, so a
// contract that cannot see a signature is not a contract.
//
// Parameter NAMES are deliberately excluded and only types recorded. A renamed
// parameter breaks nobody, and including names would fill the diff with changes
// that carry no compatibility meaning — which is how a guard trains people to
// skim it.
//
// Types are recorded AS SPELLED, not resolved. Number is an alias for int, so
// swapping one for the other compiles and changes nothing, yet trips this
// contract. That is deliberate: resolving aliases needs full type information,
// and the collector parses without type-checking precisely so it can see exports
// behind mutually-exclusive build tags — which is what
// TestPublicAPIContractIncludesMembersAndTaggedFiles pins. Spelling-level
// strictness costs an occasional regeneration; type-checking would cost the
// tagged-file guarantee, and a tag-hidden export is the more dangerous miss.
func TestPublicAPIContract(t *testing.T) {
	actual, err := collectRepositoryPublicAPI()
	if err != nil {
		t.Fatal(err)
	}
	want, err := readPublicAPIContract(publicAPIContractPath)
	if err != nil {
		t.Fatal(err)
	}

	added, removed := publicAPIContractDiff(want, actual)
	if len(added) == 0 && len(removed) == 0 {
		return
	}
	var message strings.Builder
	fmt.Fprintf(&message, "public API differs from %s (%d contracted, %d actual)", publicAPIContractPath, len(want), len(actual))
	if len(added) > 0 {
		message.WriteString("\n\nexported but not contracted:")
		for _, entry := range added {
			fmt.Fprintf(&message, "\n+ %s", entry)
		}
	}
	if len(removed) > 0 {
		message.WriteString("\n\ncontracted but no longer exported:")
		for _, entry := range removed {
			fmt.Fprintf(&message, "\n- %s", entry)
		}
	}
	message.WriteString("\n\nEvery export change must update the checked-in contract deliberately.")
	t.Fatal(message.String())
}

// This fixture makes the build-tag part of the guard non-vacuous. The collector
// parses every production Go file without type-checking a mutually-exclusive
// build, so an export hidden behind a current or future build tag cannot evade
// the contract. Both variants declare SharedExport to pin that one logical API
// identifier is deduplicated across mutually-exclusive implementations.
// Dropping tagged.go from the scan makes this test fail.
func TestPublicAPIContractIncludesMembersAndTaggedFiles(t *testing.T) {
	got, err := collectPackagePublicAPI("testdata/public_api_guard", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"fixture field ExportedType.Field int",
		"fixture func DefaultExport ()",
		"fixture func SharedExport ()",
		"fixture func TaggedExport ()",
		"fixture method ExportedType.Method ()",
		"fixture type ExportedType struct",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("fixture exports =\n%s\nwant =\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func collectRepositoryPublicAPI() ([]string, error) {
	all := make(map[string]struct{})
	packages := []struct {
		dir   string
		label string
	}{
		{dir: "crdt", label: "crdt"},
		{dir: "backend", label: "backend"},
		{dir: "backend/cluster", label: "backend/cluster"},
		{dir: "backend/conformance", label: "backend/conformance"},
		{dir: "backend/hub", label: "backend/hub"},
		{dir: "backend/memory", label: "backend/memory"},
		{dir: "backend/persistence", label: "backend/persistence"},
		{dir: "protocol", label: "protocol"},
	}
	root, err := locateRepoRoot()
	if err != nil {
		return nil, err
	}
	for _, pkg := range packages {
		entries, err := collectPackagePublicAPI(filepath.Join(root, filepath.FromSlash(pkg.dir)), pkg.label)
		if err != nil {
			return nil, fmt.Errorf("collect %s API: %w", pkg.label, err)
		}
		for _, entry := range entries {
			all[entry] = struct{}{}
		}
	}
	entries := make([]string, 0, len(all))
	for entry := range all {
		entries = append(entries, entry)
	}
	sort.Strings(entries)
	return entries, nil
}

func collectPackagePublicAPI(dir, label string) ([]string, error) {
	directory, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	entries := make(map[string]struct{})
	for _, entry := range directory {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		path := filepath.Join(dir, name)
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		collectFilePublicAPI(entries, label, file)
	}

	result := make([]string, 0, len(entries))
	for entry := range entries {
		result = append(result, entry)
	}
	sort.Strings(result)
	return result, nil
}

func collectFilePublicAPI(entries map[string]struct{}, label string, file *ast.File) {
	add := func(kind, name string) {
		entries[label+" "+kind+" "+name] = struct{}{}
	}
	addShaped := func(kind, name, shape string) {
		if shape == "" {
			entries[label+" "+kind+" "+name] = struct{}{}
			return
		}
		entries[label+" "+kind+" "+name+" "+shape] = struct{}{}
	}
	for _, declaration := range file.Decls {
		switch declaration := declaration.(type) {
		case *ast.FuncDecl:
			if !declaration.Name.IsExported() {
				continue
			}
			if declaration.Recv == nil {
				addShaped("func", declaration.Name.Name, renderFuncShape(declaration.Type))
				continue
			}
			if receiver := receiverTypeName(declaration.Recv.List[0].Type); receiver != "" {
				addShaped("method", receiver+"."+declaration.Name.Name, renderFuncShape(declaration.Type))
			}

		case *ast.GenDecl:
			for _, specification := range declaration.Specs {
				switch specification := specification.(type) {
				case *ast.TypeSpec:
					if specification.Name.IsExported() {
						addShaped("type", specification.Name.Name, renderTypeKind(specification))
					}
					collectTypeMembers(addShaped, specification.Name.Name, specification.Type)

				case *ast.ValueSpec:
					kind := declaration.Tok.String()
					for _, name := range specification.Names {
						if name.IsExported() {
							add(kind, name.Name)
						}
					}
				}
			}
		}
	}
}

func collectTypeMembers(add func(kind, name, shape string), owner string, expression ast.Expr) {
	switch expression := expression.(type) {
	case *ast.StructType:
		for _, field := range expression.Fields.List {
			if len(field.Names) == 0 {
				if name := receiverTypeName(field.Type); ast.IsExported(name) {
					add("field", owner+"."+name, types.ExprString(field.Type))
				}
				continue
			}
			for _, name := range field.Names {
				if name.IsExported() {
					add("field", owner+"."+name.Name, types.ExprString(field.Type))
				}
			}
		}

	case *ast.InterfaceType:
		for _, method := range expression.Methods.List {
			functionType, isFunction := method.Type.(*ast.FuncType)
			for _, name := range method.Names {
				if !name.IsExported() {
					continue
				}
				if isFunction {
					add("method", owner+"."+name.Name, renderFuncShape(functionType))
					continue
				}
				add("method", owner+"."+name.Name, "")
			}
		}
	}
}

// renderFuncShape renders the compatibility-relevant part of a signature: type
// parameters, parameter types and result types, with parameter names dropped.
func renderFuncShape(functionType *ast.FuncType) string {
	if functionType == nil {
		return ""
	}
	var shape strings.Builder
	if functionType.TypeParams != nil && len(functionType.TypeParams.List) > 0 {
		shape.WriteString("[")
		writeFieldTypes(&shape, functionType.TypeParams)
		shape.WriteString("]")
	}
	shape.WriteString("(")
	writeFieldTypes(&shape, functionType.Params)
	shape.WriteString(")")
	if functionType.Results != nil && len(functionType.Results.List) > 0 {
		results := &strings.Builder{}
		writeFieldTypes(results, functionType.Results)
		if len(functionType.Results.List) == 1 && len(functionType.Results.List[0].Names) == 0 {
			shape.WriteString(" " + results.String())
		} else {
			shape.WriteString(" (" + results.String() + ")")
		}
	}
	return shape.String()
}

// writeFieldTypes expands grouped declarations, so `a, b int` contributes two
// entries rather than one. A caller passing two ints is affected by the count.
func writeFieldTypes(out *strings.Builder, fields *ast.FieldList) {
	if fields == nil {
		return
	}
	first := true
	for _, field := range fields.List {
		repeats := len(field.Names)
		if repeats == 0 {
			repeats = 1
		}
		for i := 0; i < repeats; i++ {
			if !first {
				out.WriteString(", ")
			}
			first = false
			out.WriteString(types.ExprString(field.Type))
		}
	}
}

// renderTypeKind records what KIND of type this is, so a struct becoming an
// interface — or a named type becoming an alias — shows up even though the
// members are tracked separately.
func renderTypeKind(specification *ast.TypeSpec) string {
	if specification.Assign.IsValid() {
		return "= " + types.ExprString(specification.Type)
	}
	switch underlying := specification.Type.(type) {
	case *ast.StructType:
		return "struct"
	case *ast.InterfaceType:
		return "interface"
	case *ast.FuncType:
		return renderFuncShape(underlying)
	default:
		return types.ExprString(specification.Type)
	}
}

func receiverTypeName(expression ast.Expr) string {
	switch expression := expression.(type) {
	case *ast.Ident:
		return expression.Name
	case *ast.StarExpr:
		return receiverTypeName(expression.X)
	case *ast.SelectorExpr:
		return expression.Sel.Name
	case *ast.IndexExpr:
		return receiverTypeName(expression.X)
	case *ast.IndexListExpr:
		return receiverTypeName(expression.X)
	default:
		return ""
	}
}

func readPublicAPIContract(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = file.Close()
	}()

	var entries []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if len(entries) > 0 && line <= entries[len(entries)-1] {
			return nil, fmt.Errorf("%s must be sorted with no duplicates: %q follows %q", path, line, entries[len(entries)-1])
		}
		entries = append(entries, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

func publicAPIContractDiff(want, actual []string) (added, removed []string) {
	wantSet := make(map[string]struct{}, len(want))
	actualSet := make(map[string]struct{}, len(actual))
	for _, entry := range want {
		wantSet[entry] = struct{}{}
	}
	for _, entry := range actual {
		actualSet[entry] = struct{}{}
		if _, ok := wantSet[entry]; !ok {
			added = append(added, entry)
		}
	}
	for _, entry := range want {
		if _, ok := actualSet[entry]; !ok {
			removed = append(removed, entry)
		}
	}
	return added, removed
}
