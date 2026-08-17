package crdt

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

type representationBuildConfig struct {
	name string
	tags []string
}

var structStoreRepresentationBuilds = []representationBuildConfig{
	{name: "default"},
	{name: "structstoreoracle", tags: []string{"structstoreoracle"}},
}

func TestStructStoreRepresentationIsConfinedToItsBoundary(t *testing.T) {
	allowed := map[string]bool{
		"client_struct_hybrid.go":         true,
		"client_struct_hybrid_default.go": true,
		"client_struct_hybrid_oracle.go":  true,
		"client_struct_list.go":           true,
		"client_struct_tree.go":           true,
		"struct_store.go":                 true,
	}
	for _, config := range structStoreRepresentationBuilds {
		t.Run(config.name, func(t *testing.T) {
			violations, err := structStoreRepresentationViolations(".", config.tags, allowed)
			if err != nil {
				t.Fatal(err)
			}
			for _, violation := range violations {
				t.Error(violation)
			}
		})
	}

	// This source guard constrains where the representation fields are accessed.
	// It cannot prove that a method in one of the allowed boundary files does
	// not return a raw map or slice; ownership at that API boundary still requires
	// direct tests such as TestStructStoreGetStructsReturnsCallerOwnedFlattenedCopy.
}

func TestStructStoreRepresentationGuardIncludesTaggedFiles(t *testing.T) {
	allowed := map[string]bool{"model.go": true}
	var taggedViolations []string
	for _, config := range structStoreRepresentationBuilds {
		violations, err := structStoreRepresentationViolations(
			"testdata/struct_store_guard", config.tags, allowed,
		)
		if err != nil {
			t.Fatal(err)
		}
		if config.name == "default" && len(violations) != 0 {
			t.Fatalf("default build violations=%v, want none", violations)
		}
		for _, violation := range violations {
			taggedViolations = append(taggedViolations, config.name+": "+violation)
		}
	}
	if len(taggedViolations) != 1 ||
		!strings.Contains(taggedViolations[0], "tagged_violation.go") ||
		!strings.Contains(taggedViolations[0], "clientStructList.items") {
		t.Fatalf("tagged build violations=%v, want the tagged clientStructList.items access", taggedViolations)
	}
}

func structStoreRepresentationViolations(dir string, tags []string, allowed map[string]bool) ([]string, error) {
	context := build.Default
	context.BuildTags = append([]string(nil), tags...)
	buildPackage, err := context.ImportDir(dir, 0)
	if err != nil {
		return nil, fmt.Errorf("load %s with tags %v: %w", dir, tags, err)
	}
	fset := token.NewFileSet()
	files := make([]*ast.File, 0, len(buildPackage.GoFiles))
	for _, name := range buildPackage.GoFiles {
		path := filepath.Join(dir, name)
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return nil, fmt.Errorf("parse %s: %w", path, parseErr)
		}
		files = append(files, file)
	}

	info := &types.Info{Uses: make(map[*ast.Ident]types.Object)}
	checked, err := (&types.Config{Importer: newRepositoryImporter()}).Check(buildPackage.ImportPath, fset, files, info)
	if err != nil {
		return nil, fmt.Errorf("type-check %s with tags %v: %w", dir, tags, err)
	}
	storeTypeName := "structStore"
	if dir != "." {
		storeTypeName = "StructStore"
	}
	clientsField, err := namedStructField(checked, storeTypeName, "clients")
	if err != nil {
		return nil, err
	}
	itemsField, err := namedStructField(checked, "clientStructList", "items")
	if err != nil {
		return nil, err
	}
	treeRootField, err := namedStructField(checked, "clientStructTree", "root")
	if err != nil {
		return nil, err
	}
	leafItemsField, err := namedStructField(checked, "clientStructTreeLeaf", "items")
	if err != nil {
		return nil, err
	}
	branchChildrenField, err := namedStructField(checked, "clientStructTreeBranch", "children")
	if err != nil {
		return nil, err
	}
	targets := map[types.Object]string{
		clientsField:        "structStore.clients",
		itemsField:          "clientStructList.items",
		treeRootField:       "clientStructTree.root",
		leafItemsField:      "clientStructTreeLeaf.items",
		branchChildrenField: "clientStructTreeBranch.children",
	}
	var violations []string
	for ident, object := range info.Uses {
		field, targeted := targets[object]
		if !targeted {
			continue
		}
		position := fset.Position(ident.Pos())
		if !allowed[filepath.Base(position.Filename)] {
			violations = append(violations, fmt.Sprintf(
				"%s directly accesses %s; use the StructStore cursor/bulk boundary", position, field,
			))
		}
	}
	sort.Strings(violations)
	return violations, nil
}

type repositoryImporter struct {
	fallback types.Importer
	packages map[string]*types.Package
}

func newRepositoryImporter() *repositoryImporter {
	return &repositoryImporter{
		fallback: importer.Default(),
		packages: make(map[string]*types.Package),
	}
}

func (i *repositoryImporter) Import(path string) (*types.Package, error) {
	if cached := i.packages[path]; cached != nil {
		return cached, nil
	}
	if path != "github.com/antst/go-yjs/internal/lib0" {
		return i.fallback.Import(path)
	}

	root, err := locateRepoRoot()
	if err != nil {
		return nil, err
	}
	lib0Dir := filepath.Join(root, "internal", "lib0")
	buildPackage, err := build.Default.ImportDir(lib0Dir, 0)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	files := make([]*ast.File, 0, len(buildPackage.GoFiles))
	for _, name := range buildPackage.GoFiles {
		file, err := parser.ParseFile(fset, filepath.Join(lib0Dir, name), nil, 0)
		if err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	checked, err := (&types.Config{Importer: i}).Check(path, fset, files, nil)
	if err != nil {
		return nil, err
	}
	i.packages[path] = checked
	return checked, nil
}

func TestStructStoreGetStructsReturnsCallerOwnedFlattenedCopy(t *testing.T) {
	store := newStructStore()
	first := &abstractStructBase{id: GenID(11, 0), length: 1}
	second := &abstractStructBase{id: GenID(11, 1), length: 1}
	store.appendClientStruct(11, first)
	store.appendClientStruct(11, second)

	flattened := store.structsForClient(11)
	if len(flattened) != 2 || flattened[0] != first || flattened[1] != second {
		t.Fatalf("GetStructs=%v, want [%v %v]", flattened, first, second)
	}
	flattened[0] = second
	flattened = append(flattened, first)
	if len(flattened) != 3 {
		t.Fatalf("appended caller-owned view has len=%d, want 3", len(flattened))
	}
	stored, err := findStruct(store, GenID(11, 0))
	if err != nil || stored != first || store.clientLength(11) != 2 {
		t.Fatalf("caller mutation changed store: value=%v err=%v len=%d", stored, err, store.clientLength(11))
	}
}

func namedStructField(pkg *types.Package, typeName, fieldName string) (*types.Var, error) {
	object := pkg.Scope().Lookup(typeName)
	if object == nil {
		return nil, fmt.Errorf("type %s not found", typeName)
	}
	named, ok := object.Type().(*types.Named)
	if !ok {
		return nil, fmt.Errorf("%s is %T, want named type", typeName, object.Type())
	}
	structure, ok := named.Underlying().(*types.Struct)
	if !ok {
		return nil, fmt.Errorf("%s underlying type is %T, want struct", typeName, named.Underlying())
	}
	for i := 0; i < structure.NumFields(); i++ {
		if field := structure.Field(i); field.Name() == fieldName {
			return field, nil
		}
	}
	return nil, fmt.Errorf("field %s.%s not found", typeName, fieldName)
}
