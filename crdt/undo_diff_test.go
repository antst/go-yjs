package crdt

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/antst/go-yjs/internal/oracle"
)

type undoCase struct {
	Seed    int                      `json:"seed"`
	Ops     []map[string]interface{} `json:"ops"`
	State   string                   `json:"state"`
	CanUndo bool                     `json:"canUndo"`
	CanRedo bool                     `json:"canRedo"`
	UndoLen int                      `json:"undoLen"`
	RedoLen int                      `json:"redoLen"`
}

// TestUndoDiff is the undo/redo differential (US1, FR-001/FR-001a/FR-001b).
//
// Undo had ZERO differential coverage before this feature, yet four defects were found in it by
// hand. Two of those — a phantom undo entry and a lost redo — alter NO encoded bytes, so this
// compares OBSERVABLE STACK STATUS as well as state. A bytes-only comparison cannot reach them,
// which is why FR-001b exists.
func TestUndoDiff(t *testing.T) {
	path := oracleCorpus(t, "FUZZ_UNDO_FILE", "fuzz/undo_gen.mjs", "1", "400", "20")
	if path == "" {
		return
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)

	var total, stateDiv, stackDiv int
	var firstState, firstStack []int

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var c undoCase
		if err := json.Unmarshal(line, &c); err != nil {
			t.Fatalf("bad undo record: %v", err)
		}
		total++

		doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
		txt := doc.GetText("t")
		arr := doc.GetArray("a")
		remote := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(2))
		rtxt := remote.GetText("t")

		um := newUndoManager([]abstractType{txt, arr}, 100000, nil, nil)

		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("seed %d PANIC: %v", c.Seed, r)
				}
			}()
			defer um.Destroy()
			for _, op := range c.Ops {
				switch op["op"].(string) {
				case "tinsert":
					txt.Insert(int(op["idx"].(float64)), op["ch"].(string), Object{})
				case "tdelete":
					txt.Delete(int(op["idx"].(float64)), 1)
				case "ainsert":
					arr.Insert(int(op["idx"].(float64)), ArrayAny{int(op["v"].(float64))})
				case "adelete":
					arr.Delete(int(op["idx"].(float64)), 1)
				case "remote":
					rtxt.Insert(0, op["ch"].(string), Object{})
					if u, e := EncodeStateAsUpdate(remote, nil); e == nil {
						_ = ApplyUpdate(doc, u, nil)
					}
					if u, e := EncodeStateAsUpdate(doc, nil); e == nil {
						_ = ApplyUpdate(remote, u, nil)
					}
				case "stopCapturing":
					um.StopCapturing()
				case "undo":
					um.Undo()
				case "redo":
					um.Redo()
				}
			}
		}()

		got, err := EncodeStateAsUpdate(doc, nil)
		if err != nil {
			t.Errorf("seed %d encode: %v", c.Seed, err)
			continue
		}
		if hex.EncodeToString(got) != c.State {
			stateDiv++
			if len(firstState) < 8 {
				firstState = append(firstState, c.Seed)
			}
		}

		// FR-001b — stack status. A structurally-empty transaction pushing a phantom entry, or an
		// operation discarding an available redo, changes no bytes at all.
		if um.CanUndo() != c.CanUndo || um.CanRedo() != c.CanRedo {
			stackDiv++
			if len(firstStack) < 8 {
				firstStack = append(firstStack, c.Seed)
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if err := oracle.CheckCorpus("undo", path, total); err != nil {
		t.Fatal(err)
	}

	t.Logf("UNDO_DIFF total=%d stateDiv=%d stackDiv=%d firstState=%v firstStack=%v",
		total, stateDiv, stackDiv, firstState, firstStack)
	if stateDiv > 0 {
		t.Errorf("undo state diverged %d/%d (first seeds %v)", stateDiv, total, firstState)
	}
	if stackDiv > 0 {
		t.Errorf("undo STACK STATUS diverged %d/%d (first seeds %v) — invisible to a bytes-only "+
			"comparison, which is why FR-001b requires it", stackDiv, total, firstStack)
	}
}
