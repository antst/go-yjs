package crdt

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/antst/go-yjs/internal/oracle"
)

type relposEntry struct {
	Target        string `json:"target"`
	Idx           int    `json:"idx"`
	Assoc         int    `json:"assoc"`
	Encoded       string `json:"encoded"`
	ResolvedIndex *int   `json:"resolvedIndex"`
	ResolvedAssoc *int   `json:"resolvedAssoc"`
}

type relposAfter struct {
	Encoded       string `json:"encoded"`
	ResolvedIndex *int   `json:"resolvedIndex"`
}

type relposCase struct {
	Seed        int                      `json:"seed"`
	Ops         []map[string]interface{} `json:"ops"`
	State       string                   `json:"state"`
	Positions   []relposEntry            `json:"positions"`
	AfterDelete []relposAfter            `json:"afterDelete"`
}

// TestRelPosDiff is the relative-position differential (US3, FR-004, C-S2).
//
// Relative positions are a WIRE FORMAT with zero differential coverage before this feature. Unlike
// an internal defect, a divergence here breaks interoperability with third-party clients: a
// position stored by a JS peer would resolve to the wrong place here, or vice versa. It surfaces
// as silent data misplacement rather than a failing test.
//
// Three things are compared, and they are genuinely different claims:
//  1. DECODE  — the reference's encoded position resolves to the same index here.
//  2. ENCODE  — re-encoding it here produces byte-identical output (canonicality).
//  3. SURVIVAL — after the anchor's content is deleted, both sides still agree. This is the point
//     of the format; a position that only agrees while its anchor is alive is not useful.
func TestRelPosDiff(t *testing.T) {
	path := oracleCorpus(t, "FUZZ_RELPOS_FILE", "fuzz/relpos_gen.mjs", "1", "400")
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

	var total, decodeDiv, encodeDiv, survivalDiv int
	var firstDecode, firstEncode, firstSurvival []int

	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var c relposCase
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			t.Fatalf("bad relpos record: %v", err)
		}
		total++

		doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
		txt := doc.GetText("t")
		arr := doc.GetArray("a")

		// Replay only the ops that built the document BEFORE the positions were taken. The
		// generator appends one trailing delete for the survival check; apply it separately.
		trailing := len(c.Ops)
		if len(c.AfterDelete) > 0 {
			trailing--
		}
		for i := 0; i < trailing; i++ {
			op := c.Ops[i]
			switch op["op"].(string) {
			case "tinsert":
				txt.Insert(int(op["idx"].(float64)), op["ch"].(string), Object{})
			case "tdelete":
				txt.Delete(int(op["idx"].(float64)), 1)
			case "ainsert":
				arr.Insert(int(op["idx"].(float64)), ArrayAny{int(op["v"].(float64))})
			}
		}

		for _, p := range c.Positions {
			raw, err := hex.DecodeString(p.Encoded)
			if err != nil {
				t.Fatalf("seed %d: bad encoded position: %v", c.Seed, err)
			}
			rp, err := DecodeRelativePosition(raw)
			if err != nil {
				t.Fatalf("decode relative position: %v", err)
			}
			if rp == nil {
				decodeDiv++
				if len(firstDecode) < 8 {
					firstDecode = append(firstDecode, c.Seed)
				}
				continue
			}

			// (1) DECODE — same resolved index as the reference.
			abs := CreateAbsolutePositionFromRelativePosition(rp, doc)
			var gotIdx *int
			if abs != nil {
				i := abs.Index
				gotIdx = &i
			}
			if !samePtrInt(gotIdx, p.ResolvedIndex) {
				decodeDiv++
				if len(firstDecode) < 8 {
					firstDecode = append(firstDecode, c.Seed)
				}
			}

			// (2) ENCODE — canonical bytes. A position that resolves correctly but encodes
			// differently would corrupt any peer that stores our bytes.
			if hex.EncodeToString(EncodeRelativePosition(rp)) != p.Encoded {
				encodeDiv++
				if len(firstEncode) < 8 {
					firstEncode = append(firstEncode, c.Seed)
				}
			}
		}

		// (3) SURVIVAL — apply the trailing delete and re-resolve the same bytes.
		if len(c.AfterDelete) > 0 {
			op := c.Ops[len(c.Ops)-1]
			txt.Delete(int(op["idx"].(float64)), 1)
			for _, p := range c.AfterDelete {
				raw, err := hex.DecodeString(p.Encoded)
				if err != nil {
					continue
				}
				rp, err := DecodeRelativePosition(raw)
				if err != nil {
					t.Fatalf("decode relative position: %v", err)
				}
				var gotIdx *int
				if rp != nil {
					if abs := CreateAbsolutePositionFromRelativePosition(rp, doc); abs != nil {
						i := abs.Index
						gotIdx = &i
					}
				}
				if !samePtrInt(gotIdx, p.ResolvedIndex) {
					survivalDiv++
					if len(firstSurvival) < 8 {
						firstSurvival = append(firstSurvival, c.Seed)
					}
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if err := oracle.CheckCorpus("relpos", path, total); err != nil {
		t.Fatal(err)
	}

	t.Logf("RELPOS_DIFF total=%d decodeDiv=%d encodeDiv=%d survivalDiv=%d first=%v/%v/%v",
		total, decodeDiv, encodeDiv, survivalDiv, firstDecode, firstEncode, firstSurvival)
	if decodeDiv > 0 {
		t.Errorf("relpos DECODE diverged %d (first %v) — a reference position resolves elsewhere here",
			decodeDiv, firstDecode)
	}
	if encodeDiv > 0 {
		t.Errorf("relpos ENCODE diverged %d (first %v) — our bytes are not canonical",
			encodeDiv, firstEncode)
	}
	if survivalDiv > 0 {
		t.Errorf("relpos SURVIVAL diverged %d (first %v) — positions disagree after the anchor is deleted",
			survivalDiv, firstSurvival)
	}
}

func samePtrInt(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
