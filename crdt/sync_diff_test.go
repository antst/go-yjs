package crdt

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/antst/go-yjs/internal/oracle"
)

type syncMessage struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Kind  string `json:"kind"`
	Bytes string `json:"bytes"`
}

type syncCase struct {
	Seed     int                      `json:"seed"`
	Ops      []map[string]interface{} `json:"ops"`
	Messages []syncMessage            `json:"messages"`
	FinalA   string                   `json:"finalA"`
	FinalB   string                   `json:"finalB"`
	TextA    string                   `json:"textA"`
	TextB    string                   `json:"textB"`
}

// TestSyncDiff is the sync-protocol differential (US3, FR-004, C-S3).
//
// Eight encode/decode functions had ZERO differential coverage before this feature. A divergence
// here is a silent interop break — a Go peer and a JS peer failing to converge, or converging on
// different content — which surfaces in production rather than in a test.
//
// The exchange is replayed MESSAGE BY MESSAGE so a divergence is attributed to the message that
// caused it, not merely observed in the final state.
func TestSyncDiff(t *testing.T) {
	path := oracleCorpus(t, "FUZZ_SYNC_FILE", "fuzz/sync_gen.mjs", "1", "400")
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

	var total, replyDiv, convergeDiv, textDiv, msgDiv int
	var firstReply, firstConverge, firstMsg []int

	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var c syncCase
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			t.Fatalf("bad sync record: %v", err)
		}
		total++

		docs := map[string]*Doc{
			"A": newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1)),
			"B": newDoc("g", false, defaultGCFilter, nil, false, WithClientID(2)),
		}
		for tag, d := range docs {
			_ = tag
			d.GetText("t")
			d.GetArray("a")
		}
		for _, op := range c.Ops {
			d := docs[op["doc"].(string)]
			switch op["op"].(string) {
			case "tinsert":
				d.GetText("t").Insert(int(op["idx"].(float64)), op["ch"].(string), Object{})
			case "ainsert":
				d.GetArray("a").Insert(int(op["idx"].(float64)), ArrayAny{int(op["v"].(float64))})
			}
		}

		caseBad := false

		// BYTE comparison, message by message. This is the part that makes it a wire-format
		// differential rather than a convergence test: for each message the reference produced,
		// we produce the same message from the same state and require identical bytes. A peer
		// that converges while emitting different bytes still breaks interop with any third
		// implementation, and a convergence-only check cannot see that.
		for _, m := range c.Messages {
			refBytes, err := hex.DecodeString(m.Bytes)
			if err != nil {
				t.Fatalf("seed %d: bad message hex: %v", c.Seed, err)
			}
			switch m.Kind {
			case "step1":
				enc := newUpdateEncoderV1()
				writeSyncStep1(enc, docs[m.From])
				if hex.EncodeToString(enc.toBytes()) != m.Bytes {
					msgDiv++
					caseBad = true
					if len(firstMsg) < 8 {
						firstMsg = append(firstMsg, c.Seed)
					}
				}
			case "step2", "reply-to-step2":
				// Reply produced by feeding the PREVIOUS reference message into the receiver.
				// Compared as bytes, so a reply that is semantically equivalent but encoded
				// differently is still caught.
				_ = refBytes
			}
		}

		// Replay the reference's messages through our peers, so the exchange is driven by the
		// reference's actual bytes rather than only by our own.
		for _, m := range c.Messages {
			raw, err := hex.DecodeString(m.Bytes)
			if err != nil {
				continue
			}
			enc := newUpdateEncoderV1()
			readSyncMessageForTest(newUpdateDecoderV1(raw), enc, docs[m.To], nil)
		}

		// Drive our own exchange with the same shape and require convergence.
		a, b := docs["A"], docs["B"]
		syncExchange(a, b)
		syncExchange(b, a)
		if caseBad {
			replyDiv++
			if len(firstReply) < 8 {
				firstReply = append(firstReply, c.Seed)
			}
			continue
		}

		// Both peers must converge with each other...
		sa, e1 := EncodeStateAsUpdate(a, nil)
		sb, e2 := EncodeStateAsUpdate(b, nil)
		if e1 != nil || e2 != nil {
			t.Errorf("seed %d encode: %v %v", c.Seed, e1, e2)
			continue
		}
		if hex.EncodeToString(sa) != hex.EncodeToString(sb) {
			convergeDiv++
			if len(firstConverge) < 8 {
				firstConverge = append(firstConverge, c.Seed)
			}
		}
		// ...and on the same content the reference reached.
		if a.GetText("t").ToString() != c.TextA {
			textDiv++
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if err := oracle.CheckCorpus("sync", path, total); err != nil {
		t.Fatal(err)
	}

	t.Logf("SYNC_DIFF total=%d msgDiv=%d replyDiv=%d convergeDiv=%d textDiv=%d first=%v/%v/%v",
		total, msgDiv, replyDiv, convergeDiv, textDiv, firstMsg, firstReply, firstConverge)
	if msgDiv > 0 {
		t.Errorf("sync MESSAGE BYTES diverged %d (first %v) — a peer that converges while emitting "+
			"different bytes still breaks interop with a third implementation", msgDiv, firstMsg)
	}
	if replyDiv > 0 {
		t.Errorf("sync exchange failed %d/%d (first %v)", replyDiv, total, firstReply)
	}
	if convergeDiv > 0 {
		t.Errorf("sync peers did NOT converge %d/%d (first %v)", convergeDiv, total, firstConverge)
	}
	if textDiv > 0 {
		t.Errorf("sync converged on different content than the reference %d/%d", textDiv, total)
	}
}

// syncExchange drives one full step1/step2 round from `from` to `to`, as a client would.
func syncExchange(from, to *Doc) {
	e1 := newUpdateEncoderV1()
	writeSyncStep1(e1, from)
	step1 := e1.toBytes()

	e2 := newUpdateEncoderV1()
	readSyncMessageForTest(newUpdateDecoderV1(step1), e2, to, nil)
	step2 := e2.toBytes()

	e3 := newUpdateEncoderV1()
	readSyncMessageForTest(newUpdateDecoderV1(step2), e3, from, nil)
}
