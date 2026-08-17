package crdt

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// T068 / version drift. Eleven source comments claimed parity with an OLDER yjs and lib0 than the
// harness was actually pinned to, having been left behind by a re-pin. A stale claim is worse than
// no claim: it states a verification that was never run at the version actually used.
//
// (Deliberately no literal version numbers in this comment — they would be indistinguishable from
// the claims the test scans for, and the test would flag itself.)
//
// This test derives the truth from the committed lockfiles — the same ones `npm ci` installs from —
// and fails if any tracked source file names a DIFFERENT version of a pinned dependency. It is the
// coverage that would have caught the drift (FR-016 bar (a)).
func TestNoStaleReferenceVersionClaims(t *testing.T) {
	pinned := map[string]string{}
	for _, lock := range []string{"fuzz/package-lock.json", "v2_test_fixtures/package-lock.json"} {
		raw, err := os.ReadFile(repoPath(t, lock))
		if err != nil {
			t.Fatalf("read %s: %v", lock, err)
		}
		var parsed struct {
			Packages map[string]struct {
				Version string `json:"version"`
			} `json:"packages"`
		}
		if err := json.Unmarshal(raw, &parsed); err != nil {
			t.Fatalf("parse %s: %v", lock, err)
		}
		for path, pkg := range parsed.Packages {
			// The lockfile's root entry has an empty key and no "node_modules/" segment.
			at := strings.LastIndex(path, "node_modules/")
			if at < 0 {
				continue
			}
			name := path[at+len("node_modules/"):]
			switch name {
			case "yjs", "lib0", "y-protocols":
				if prev, ok := pinned[name]; ok && prev != pkg.Version {
					t.Fatalf("%s is pinned to two different versions (%s and %s) across lockfiles",
						name, prev, pkg.Version)
				}
				pinned[name] = pkg.Version
			}
		}
	}
	for _, want := range []string{"yjs", "lib0", "y-protocols"} {
		if pinned[want] == "" {
			t.Fatalf("%s not found in any lockfile; the drift guard would silently pass", want)
		}
	}
	t.Logf("pinned: yjs@%s lib0@%s y-protocols@%s", pinned["yjs"], pinned["lib0"], pinned["y-protocols"])

	// Any semver-shaped mention next to a dependency name must match the pin. Prior features'
	// specs are historical records of what was true then, so they are out of scope.
	pat := regexp.MustCompile(`(?i)\b(yjs|lib0|y-protocols)\b[ @v]*([0-9]+\.[0-9]+\.[0-9]+)`)
	var bad []string
	// Walk the REPOSITORY, not the package directory. With "." this scans only
	// wherever the package happens to sit, so once the package moves it would
	// silently check a fraction of the tree and still pass — a coverage loss that
	// reports success, which is worse than a failure.
	root := repoRoot(t)
	err := filepath.WalkDir(root, func(abs string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		path, relErr := filepath.Rel(root, abs)
		if relErr != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", ".git":
				return filepath.SkipDir
			}
			return nil
		}
		switch filepath.Ext(path) {
		case ".go", ".md", ".mjs", ".cjs", ".sh":
		default:
			return nil
		}
		if strings.HasPrefix(path, "specs/001-") || strings.HasPrefix(path, "specs/002-") ||
			strings.HasPrefix(path, "specs/003-") {
			return nil
		}
		raw, rerr := os.ReadFile(abs)
		if rerr != nil {
			return nil
		}
		for i, line := range strings.Split(string(raw), "\n") {
			for _, m := range pat.FindAllStringSubmatch(line, -1) {
				dep, ver := strings.ToLower(m[1]), m[2]
				if want, ok := pinned[dep]; ok && ver != want {
					bad = append(bad, fmt.Sprintf("%s:%d claims %s@%s, pinned is %s", path, i+1, dep, ver, want))
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range bad {
		t.Error("stale reference-version claim: " + b)
	}
}
