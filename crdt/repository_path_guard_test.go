package crdt

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Tests in this package must reach repository assets through repoPath, never
// through a bare relative literal.
//
// WHY THIS EXISTS. The crdt/ move broke this twice, and neither break was
// caught by the thing that should have caught it. A bare "fuzz/node_modules/yjs"
// used to resolve because the package sat at the repository root; afterwards it
// resolved to crdt/fuzz/node_modules/yjs and did not exist. One of the two
// SKIPPED on the missing directory instead of failing, so it reported success
// while silently testing nothing — the failure mode that makes this worth
// enforcing rather than remembering. A path that moves must break loudly.
//
// The rule is deliberately narrow so it needs no exemption list: only literals
// that a path-consuming call actually opens are flagged. Error messages and
// prefix comparisons mention the same directory names and are none of this
// guard's business.
func TestTestsResolveRepositoryAssetsThroughRepoPath(t *testing.T) {
	roots := repositoryTopLevelNames(t)
	for _, file := range packageTestFiles(t) {
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, violation := range bareRepositoryPaths(parsed, roots) {
			t.Errorf("%s: %s reaches %q relative to this package, which no longer is the "+
				"repository root; wrap it in repoPath so it resolves from go.mod",
				fset.Position(violation.pos), violation.call, violation.literal)
		}
		for _, function := range nodeScriptsWithoutWorkingDirectory(parsed) {
			t.Errorf("%s: %s runs node on a script that requires a repository-relative "+
				"path but never sets cmd.Dir, so it depends on the caller's working directory",
				fset.Position(parsed.Pos()), function)
		}
	}
}

// The negative fixtures matter more than usual here: a source guard that has
// only ever seen a clean tree proves nothing about what it would reject.
func TestRepositoryPathGuardRejectsViolations(t *testing.T) {
	roots := map[string]bool{"fuzz": true}

	for _, source := range []string{
		"package p\nimport \"os\"\nfunc f() { os.Stat(\"fuzz/node_modules/yjs\") }\n",
		"package p\nimport (\"os\"; \"path/filepath\")\nfunc f() { os.Stat(filepath.Join(\"fuzz\", \"node_modules\")) }\n",
		"package p\nimport \"os\"\nfunc f() { os.ReadFile(\"fuzz/generate.js\") }\n",
	} {
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, "fixture.go", source, 0)
		if err != nil {
			t.Fatal(err)
		}
		if got := bareRepositoryPaths(parsed, roots); len(got) == 0 {
			t.Errorf("guard accepted a bare repository path in:\n%s", source)
		}
	}

	// ...and must not fire on the wrapped form, or it would be unusable.
	for _, source := range []string{
		"package p\nimport \"os\"\nfunc f(t *testing.T) { os.Stat(repoPath(t, \"fuzz\", \"node_modules\")) }\n",
		"package p\nimport \"os\"\nfunc f() { os.ReadDir(\".\") }\n",
		"package p\nfunc f() { _ = \"fuzz/node_modules/yjs is missing\" }\n",
		"package p\nimport \"strings\"\nfunc f(p string) bool { return strings.HasPrefix(p, \"fuzz/\") }\n",
	} {
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, "fixture.go", source, 0)
		if err != nil {
			t.Fatal(err)
		}
		if got := bareRepositoryPaths(parsed, roots); len(got) != 0 {
			t.Errorf("guard rejected a legitimate form %q in:\n%s", got[0].literal, source)
		}
	}

	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, "fixture.go", "package p\nimport \"os/exec\"\n"+
		"func f() { cmd := exec.Command(\"node\", \"-e\", \"require('./fuzz/node_modules/yjs')\"); cmd.Output() }\n", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := nodeScriptsWithoutWorkingDirectory(parsed); len(got) == 0 {
		t.Error("guard accepted a node script requiring ./fuzz with no cmd.Dir")
	}
}

// Benchmark collectors must name the CRDT package explicitly. The repository
// root is documentation-only, so `go test ... .` succeeds while collecting zero
// benchmarks -- a particularly dangerous failure because it looks like a clean
// Go invocation. Shell continuations are joined before inspection: all three
// instances missed during the crdt/ move put the package on a continuation line,
// which made a same-line grep report a clean tree.
func TestBenchmarkScriptsDoNotTargetDocumentationRoot(t *testing.T) {
	benchRoot := repoPath(t, "bench")
	var scripts, benchmarkCommands int
	err := filepath.WalkDir(benchRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sh") {
			return nil
		}
		scripts++
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, line := range logicalShellLines(string(contents)) {
			if !isBenchmarkShellCommand(line) {
				continue
			}
			benchmarkCommands++
			if benchmarkCommandTargetsBareRoot(line) {
				t.Errorf("%s targets the documentation-only repository root with a bare '.'; use ./crdt", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scripts == 0 || benchmarkCommands == 0 {
		t.Fatalf("guard inspected %d shell scripts and %d benchmark commands; it would pass vacuously", scripts, benchmarkCommands)
	}
}

func TestBenchmarkScriptTargetGuardRejectsContinuedBareRoot(t *testing.T) {
	bad := "go test -run '^$' -bench '^Benchmark' \\\n\t-timeout 120m ."
	lines := logicalShellLines(bad)
	if len(lines) != 1 {
		t.Fatalf("continued command became %d logical lines, want 1", len(lines))
	}
	if !benchmarkCommandTargetsBareRoot(lines[0]) {
		t.Fatal("guard accepted a benchmark package target split onto a continuation line")
	}
	if benchmarkCommandTargetsBareRoot("go test -run '^$' -bench '^Benchmark' -timeout 120m ./crdt") {
		t.Fatal("guard rejected the CRDT package target")
	}
	if benchmarkCommandTargetsBareRoot("go test -run '^$' -bench . -timeout 120m ./crdt") {
		t.Fatal("guard mistook a bare benchmark-pattern argument for the package target")
	}
}

func logicalShellLines(source string) []string {
	source = strings.ReplaceAll(source, "\\\r\n", "\\\n")
	var logical []string
	var current strings.Builder
	for _, physical := range strings.Split(source, "\n") {
		physical = strings.TrimRight(physical, " \t")
		continued := strings.HasSuffix(physical, "\\")
		if continued {
			physical = strings.TrimSuffix(physical, "\\")
		}
		if current.Len() > 0 {
			current.WriteByte(' ')
		}
		current.WriteString(physical)
		if !continued {
			logical = append(logical, current.String())
			current.Reset()
		}
	}
	if current.Len() > 0 {
		logical = append(logical, current.String())
	}
	return logical
}

func benchmarkCommandTargetsBareRoot(line string) bool {
	if !isBenchmarkShellCommand(line) {
		return false
	}
	line = strings.TrimSpace(line)
	if comment := strings.IndexByte(line, '#'); comment >= 0 {
		line = line[:comment]
	}
	fields := strings.Fields(line)
	for index, field := range fields {
		candidate := strings.Trim(field, "\\\"'(){};|&")
		if candidate != "." {
			continue
		}
		// A command may legitimately spell `-bench .`; that dot is the
		// benchmark regexp's value, not a package target.
		if index > 0 && strings.Trim(fields[index-1], "\\\"'(){};|&") == "-bench" {
			continue
		}
		return true
	}
	return false
}

func isBenchmarkShellCommand(line string) bool {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return false
	}
	if comment := strings.IndexByte(line, '#'); comment >= 0 {
		line = line[:comment]
	}
	for _, field := range strings.Fields(line) {
		field = strings.Trim(field, "\\\"'(){};|&")
		if field == "-bench" || strings.HasPrefix(field, "-bench=") {
			return true
		}
	}
	return false
}

type bareRepositoryPath struct {
	pos     token.Pos
	call    string
	literal string
}

// pathConsumers are the calls that actually resolve a path against the working
// directory. A literal is only interesting where one of these opens it.
var pathConsumers = map[string]map[string]bool{
	"os":       {"Stat": true, "Open": true, "OpenFile": true, "ReadFile": true, "ReadDir": true, "Lstat": true},
	"filepath": {"Join": true, "Walk": true, "WalkDir": true, "Glob": true, "Abs": true},
}

// repoPath and its wrappers already resolve from go.mod, so a literal inside
// one is correct by construction and its subtree is not searched.
var pathResolvers = map[string]bool{"repoPath": true, "repoRoot": true, "oracleCorpus": true}

func bareRepositoryPaths(file *ast.File, roots map[string]bool) []bareRepositoryPath {
	var found []bareRepositoryPath
	var walk func(node ast.Node, insideConsumer string)
	walk = func(node ast.Node, insideConsumer string) {
		if node == nil {
			return
		}
		if call, isCall := node.(*ast.CallExpr); isCall {
			if name, isResolver := resolverName(call); isResolver {
				_ = name
				return // shielded: everything below resolves from the repository root
			}
			if consumer, isConsumer := consumerName(call); isConsumer {
				// Only the FIRST argument anchors the path. filepath.Join(root,
				// "internal", "lib0") is already anchored to a computed root and
				// its later segments are not paths in their own right, and
				// descending into a WalkDir callback would flag every literal in
				// the closure body. Every consumer below takes its path first.
				if len(call.Args) > 0 {
					walk(call.Args[0], consumer)
				}
				return
			}
		}
		if literal, isLiteral := node.(*ast.BasicLit); isLiteral && insideConsumer != "" && literal.Kind == token.STRING {
			if value, err := strconv.Unquote(literal.Value); err == nil && namesRepositoryRoot(value, roots) {
				found = append(found, bareRepositoryPath{pos: literal.Pos(), call: insideConsumer, literal: value})
			}
			return
		}
		ast.Inspect(node, func(child ast.Node) bool {
			if child == node || child == nil {
				return child == node
			}
			walk(child, insideConsumer)
			return false
		})
	}
	walk(file, "")
	return found
}

func namesRepositoryRoot(value string, roots map[string]bool) bool {
	head, _, _ := strings.Cut(filepath.ToSlash(value), "/")
	return roots[head]
}

func resolverName(call *ast.CallExpr) (string, bool) {
	identifier, isIdentifier := call.Fun.(*ast.Ident)
	if !isIdentifier {
		return "", false
	}
	return identifier.Name, pathResolvers[identifier.Name]
}

// consumerCallName renders pkg.Fn for a selector call, without the consumer filter.
func consumerCallName(call *ast.CallExpr) (string, bool) {
	selector, isSelector := call.Fun.(*ast.SelectorExpr)
	if !isSelector {
		return "", false
	}
	pkg, isIdentifier := selector.X.(*ast.Ident)
	if !isIdentifier {
		return "", false
	}
	return pkg.Name + "." + selector.Sel.Name, true
}

func consumerName(call *ast.CallExpr) (string, bool) {
	selector, isSelector := call.Fun.(*ast.SelectorExpr)
	if !isSelector {
		return "", false
	}
	pkg, isIdentifier := selector.X.(*ast.Ident)
	if !isIdentifier {
		return "", false
	}
	if !pathConsumers[pkg.Name][selector.Sel.Name] {
		return "", false
	}
	return pkg.Name + "." + selector.Sel.Name, true
}

// A node script that requires a repository-relative module resolves against the
// process working directory, which the crdt/ move changed. Setting cmd.Dir is
// the fix; this reports the functions that forgot it.
func nodeScriptsWithoutWorkingDirectory(file *ast.File) []string {
	var offenders []string
	for _, declaration := range file.Decls {
		function, isFunction := declaration.(*ast.FuncDecl)
		if !isFunction || function.Body == nil {
			continue
		}
		var needsDir, setsDir, runsCommand bool
		ast.Inspect(function.Body, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.CallExpr:
				// The function must actually SPAWN something. Without this the
				// guard flags its own negative fixtures, whose exec.Command sits
				// inside a source string rather than in the syntax tree.
				if name, isSelector := consumerCallName(node); isSelector && name == "exec.Command" {
					runsCommand = true
				}
			case *ast.BasicLit:
				if node.Kind == token.STRING && strings.Contains(node.Value, "./fuzz") {
					needsDir = true
				}
			case *ast.AssignStmt:
				for _, target := range node.Lhs {
					if selector, isSelector := target.(*ast.SelectorExpr); isSelector && selector.Sel.Name == "Dir" {
						setsDir = true
					}
				}
			}
			return true
		})
		if needsDir && runsCommand && !setsDir {
			offenders = append(offenders, function.Name.Name)
		}
	}
	return offenders
}

func repositoryTopLevelNames(t *testing.T) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, entry := range entries {
		// crdt is this package's own directory: paths under it are correctly
		// package-relative and must not be flagged.
		if !entry.IsDir() || entry.Name() == "crdt" || entry.Name() == ".git" {
			continue
		}
		names[entry.Name()] = true
	}
	if len(names) == 0 {
		t.Fatal("no repository directories found; the guard would pass vacuously")
	}
	return names
}

func packageTestFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), "_test.go") {
			files = append(files, entry.Name())
		}
	}
	if len(files) == 0 {
		t.Fatal("no test files found; the guard would pass vacuously")
	}
	return files
}
