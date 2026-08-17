package backend_test

import (
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/antst/go-yjs"

type boundaryRule struct {
	dir          string
	allowedLocal map[string]bool
	forbidNet    bool
}

// This guard makes the port diagram compiler-visible. In particular, Hub is a
// transport-neutral contract: adding a connection or protocol dependency is a
// design change, not an innocent import.
func TestBackendImportBoundaries(t *testing.T) {
	tests := []boundaryRule{
		{dir: ".", allowedLocal: nil},
		{dir: "persistence", allowedLocal: set(modulePath + "/backend")},
		{dir: "cluster", allowedLocal: set(modulePath + "/backend")},
		{dir: "hub", allowedLocal: set(modulePath + "/backend"), forbidNet: true},
		{dir: "memory", allowedLocal: set(modulePath+"/crdt", modulePath+"/backend")},
	}
	for _, test := range tests {
		t.Run(test.dir, func(t *testing.T) {
			if err := checkBoundary(test); err != nil {
				t.Fatal(err)
			}
		})
	}

	for _, dir := range []string{"..", "../crdt", "../protocol"} {
		if err := checkForbiddenPrefix(dir, modulePath+"/backend"); err != nil {
			t.Fatal(err)
		}
	}
}

// The boundary assertions have negative fixtures because a source guard that
// has only observed a clean tree can be hollow while appearing authoritative.
func TestBackendImportBoundaryGuardRejectsViolations(t *testing.T) {
	t.Run("hub transport", func(t *testing.T) {
		dir := writePackage(t, "package fixture\nimport _ \"net/http\"\n")
		if err := checkBoundary(boundaryRule{dir: dir, forbidNet: true}); err == nil || !strings.Contains(err.Error(), "transport package") {
			t.Fatalf("transport violation = %v", err)
		}
	})
	t.Run("sibling dependency", func(t *testing.T) {
		dir := writePackage(t, "package fixture\nimport _ \"github.com/antst/go-yjs/protocol\"\n")
		if err := checkBoundary(boundaryRule{dir: dir, allowedLocal: set(modulePath + "/backend")}); err == nil || !strings.Contains(err.Error(), "forbidden module package") {
			t.Fatalf("sibling violation = %v", err)
		}
	})
	t.Run("core reverse dependency", func(t *testing.T) {
		dir := writePackage(t, "package fixture\nimport _ \"github.com/antst/go-yjs/backend/hub\"\n")
		if err := checkForbiddenPrefix(dir, modulePath+"/backend"); err == nil || !strings.Contains(err.Error(), "forbidden dependency") {
			t.Fatalf("reverse dependency violation = %v", err)
		}
	})
}

func checkBoundary(rule boundaryRule) error {
	imports, err := productionImports(rule.dir)
	if err != nil {
		return err
	}
	for _, imported := range imports {
		if rule.forbidNet && (imported == "net" || strings.HasPrefix(imported, "net/")) {
			return fmt.Errorf("%s imports transport package %q", rule.dir, imported)
		}
		if !strings.HasPrefix(imported, modulePath) {
			continue
		}
		if !rule.allowedLocal[imported] {
			return fmt.Errorf("%s imports forbidden module package %q", rule.dir, imported)
		}
	}
	return nil
}

func checkForbiddenPrefix(dir, prefix string) error {
	imports, err := productionImports(dir)
	if err != nil {
		return err
	}
	for _, imported := range imports {
		if imported == prefix || strings.HasPrefix(imported, prefix+"/") {
			return fmt.Errorf("%s has forbidden dependency %q", dir, imported)
		}
	}
	return nil
}

func productionImports(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var result []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		for _, specification := range file.Imports {
			imported, err := strconv.Unquote(specification.Path.Value)
			if err != nil {
				return nil, fmt.Errorf("unquote import in %s: %w", path, err)
			}
			result = append(result, imported)
		}
	}
	return result, nil
}

func set(values ...string) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		result[value] = true
	}
	return result
}

func writePackage(t *testing.T, source string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fixture.go"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}
