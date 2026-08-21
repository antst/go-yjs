package crdt

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// inCurrentBuild reports whether the file participates in THIS build.
//
// The directory holds mutually-exclusive build-tagged variants — the
// structstoreoracle pair, for one — so a search that reads every .go file can
// return source that is not compiled into the binary under test. A guard
// inspecting the wrong variant is worse than one that fails: it reports on code
// that is not running.
func inCurrentBuild(dir, file string) bool {
	context := build.Default
	context.UseAllFiles = false
	ok, err := context.MatchFile(dir, file)
	return err == nil && ok
}

// parseProductionFileDeclaring returns the production file that declares name,
// found by SEARCHING rather than by hard-coding a filename.
//
// WHY. Several guards pin structural properties of the implementation by parsing
// a source file, and every one that named its file as a string broke the moment
// files were consolidated — three of them in a single change, each failing with
// an unhelpful "no such file or directory" that said nothing about what to do.
// A guard whose subject is "wherever typeListInsertGenericsAfter lives" should
// say that, not "abstract_type.go".
//
// name may be a plain function or type name, or a method spelled Receiver.Method.
func parseProductionFileDeclaring(t *testing.T, name string) *ast.File {
	t.Helper()
	receiver, method, isMethod := strings.Cut(name, ".")

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		file := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(file, ".go") || strings.HasSuffix(file, "_test.go") {
			continue
		}
		if !inCurrentBuild(".", file) {
			continue
		}
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, filepath.Clean(file), nil, 0)
		if err != nil {
			continue
		}
		for _, declaration := range parsed.Decls {
			switch declaration := declaration.(type) {
			case *ast.FuncDecl:
				if isMethod {
					if declaration.Recv == nil || declaration.Name.Name != method {
						continue
					}
					if receiverTypeName(declaration.Recv.List[0].Type) == receiver {
						return parsed
					}
					continue
				}
				// A bare name matches a plain function OR a method of that name.
				// The guards that use this pin one named routine each, and their
				// subject is "wherever that routine lives" regardless of whether
				// it happens to have a receiver.
				if declaration.Name.Name == name {
					return parsed
				}
			case *ast.GenDecl:
				for _, specification := range declaration.Specs {
					spec, ok := specification.(*ast.TypeSpec)
					if ok && spec.Name.Name == name {
						return parsed
					}
				}
			}
		}
	}
	t.Fatalf("no production file declares %q; if it was renamed or removed, "+
		"update the guard that looks for it", name)
	return nil
}

// parseTestFileDeclaring is parseProductionFileDeclaring for TEST sources.
//
// One guard audits the gate test itself — it checks that every declared
// direction-B response field is genuinely READ there rather than merely
// declared — so its subject is a _test.go file, which the production search
// deliberately skips.
func parseTestFileDeclaring(t *testing.T, name string) (*token.FileSet, *ast.File) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		file := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(file, "_test.go") {
			continue
		}
		if !inCurrentBuild(".", file) {
			continue
		}
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, filepath.Clean(file), nil, 0)
		if err != nil {
			continue
		}
		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if ok && function.Recv == nil && function.Name.Name == name {
				return fset, parsed
			}
		}
	}
	t.Fatalf("no test file declares %q; if it was renamed or removed, "+
		"update the guard that looks for it", name)
	return nil, nil
}

// receiverTypeName reduces a method receiver expression to the bare type name,
// looking through pointers and type parameters.
//
// internal/apicontract carries its own copy. The two are independent guards in
// independent packages, and a shared package for sixteen lines of AST switch
// would be an abstraction with no other reason to exist.
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
