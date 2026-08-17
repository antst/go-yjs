package crdt

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestSharedTypeBoundaryRemainsSealed(t *testing.T) {
	_, file := parseProductionFileDeclaring(t, "SharedType")
	for _, declaration := range file.Decls {
		gen, ok := declaration.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, specification := range gen.Specs {
			typeSpec, ok := specification.(*ast.TypeSpec)
			if !ok || typeSpec.Name.Name != "SharedType" {
				continue
			}
			iface, ok := typeSpec.Type.(*ast.InterfaceType)
			if !ok || len(iface.Methods.List) != 1 {
				t.Fatalf("SharedType must contain exactly one sealed marker method; got %T with %d entries", typeSpec.Type, len(iface.Methods.List))
			}
			marker := iface.Methods.List[0]
			if len(marker.Names) != 1 || marker.Names[0].Name != "isSharedType" || marker.Names[0].IsExported() {
				t.Fatalf("SharedType marker = %v, want the sole unexported isSharedType method", marker.Names)
			}
			signature, ok := marker.Type.(*ast.FuncType)
			if !ok || fieldCount(signature.Params) != 0 || fieldCount(signature.Results) != 0 {
				t.Fatal("SharedType.isSharedType must be a parameterless, resultless marker")
			}
			return
		}
	}
	t.Fatal("SharedType declaration not found")
}

// Exporting a name is not useful when its signature contains a type callers
// cannot name. The stage-1 contract inventories identifiers; this complementary
// guard pins that every externally reachable function, method, and struct field
// is usable without reaching into the private object graph.
func TestPublicSignaturesDoNotExposePrivateTypes(t *testing.T) {
	// protocol is scanned alongside this package because the two together are
	// what a consumer imports: a private type leaking through protocol's
	// signatures is the same defect as one leaking through this package's. It is
	// resolved from the repository root because it is a SIBLING directory, not a
	// child of this one.
	for _, dir := range []string{".", repoPath(t, "protocol")} {
		files, privateTypes, err := parseProductionPackage(dir)
		if err != nil {
			t.Fatal(err)
		}
		var leaks []string
		for path, file := range files {
			for _, declaration := range file.Decls {
				switch declaration := declaration.(type) {
				case *ast.FuncDecl:
					if !declaration.Name.IsExported() || !publicReceiver(declaration.Recv) {
						continue
					}
					if names := privateTypeReferences(declaration.Type, privateTypes); len(names) != 0 {
						leaks = append(leaks, fmt.Sprintf("%s: %s mentions %s", path, declaration.Name.Name, strings.Join(names, ", ")))
					}

				case *ast.GenDecl:
					for _, specification := range declaration.Specs {
						typeSpec, ok := specification.(*ast.TypeSpec)
						if !ok || !typeSpec.Name.IsExported() {
							continue
						}
						switch definition := typeSpec.Type.(type) {
						case *ast.StructType:
							for _, field := range definition.Fields.List {
								if !publicField(field) {
									continue
								}
								if names := privateTypeReferences(field.Type, privateTypes); len(names) != 0 {
									leaks = append(leaks, fmt.Sprintf("%s: %s field mentions %s", path, typeSpec.Name.Name, strings.Join(names, ", ")))
								}
							}
						case *ast.InterfaceType:
							for _, method := range definition.Methods.List {
								if len(method.Names) != 0 && !method.Names[0].IsExported() {
									continue // a sealed marker is deliberately unreachable
								}
								if names := privateTypeReferences(method.Type, privateTypes); len(names) != 0 {
									leaks = append(leaks, fmt.Sprintf("%s: %s interface method mentions %s", path, typeSpec.Name.Name, strings.Join(names, ", ")))
								}
							}
						}
					}
				}
			}
		}
		if len(leaks) != 0 {
			sort.Strings(leaks)
			t.Fatalf("public signatures expose package-private types:\n%s", strings.Join(leaks, "\n"))
		}
	}
}

func parseProductionPackage(dir string) (map[string]*ast.File, map[string]struct{}, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	files := make(map[string]*ast.File)
	privateTypes := make(map[string]struct{})
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return nil, nil, err
		}
		files[path] = file
		for _, declaration := range file.Decls {
			gen, ok := declaration.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, specification := range gen.Specs {
				if typeSpec, ok := specification.(*ast.TypeSpec); ok && !typeSpec.Name.IsExported() {
					privateTypes[typeSpec.Name.Name] = struct{}{}
				}
			}
		}
	}
	return files, privateTypes, nil
}

func publicReceiver(receiver *ast.FieldList) bool {
	if receiver == nil {
		return true
	}
	return ast.IsExported(receiverTypeName(receiver.List[0].Type))
}

func publicField(field *ast.Field) bool {
	if len(field.Names) != 0 {
		for _, name := range field.Names {
			if name.IsExported() {
				return true
			}
		}
		return false
	}
	return ast.IsExported(receiverTypeName(field.Type))
}

func privateTypeReferences(expression ast.Expr, privateTypes map[string]struct{}) []string {
	selectors := make(map[*ast.Ident]struct{})
	ast.Inspect(expression, func(node ast.Node) bool {
		if selector, ok := node.(*ast.SelectorExpr); ok {
			selectors[selector.Sel] = struct{}{}
		}
		return true
	})
	seen := make(map[string]struct{})
	ast.Inspect(expression, func(node ast.Node) bool {
		identifier, ok := node.(*ast.Ident)
		if !ok {
			return true
		}
		if _, selector := selectors[identifier]; selector {
			return true
		}
		if _, private := privateTypes[identifier.Name]; private {
			seen[identifier.Name] = struct{}{}
		}
		return true
	})
	result := make([]string, 0, len(seen))
	for name := range seen {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

func fieldCount(list *ast.FieldList) int {
	if list == nil {
		return 0
	}
	count := 0
	for _, field := range list.List {
		if len(field.Names) == 0 {
			count++
		} else {
			count += len(field.Names)
		}
	}
	return count
}
