package protocol

import (
	"bytes"
	"runtime"
	"testing"
	"time"

	"github.com/antst/go-yjs/crdt"
)

const (
	protocolFuzzMaxInputBytes = 64 * 1024
	// A real hang is UNBOUNDED, so a generous deadline loses no detection power
	// while a tight one manufactures findings. At 150ms this fired on a 40-byte
	// input that actually decodes in 248 MICROseconds — a 600x margin — because
	// Go runs fuzz workers in parallel and the host was also running the suite.
	// The cost of that false positive is not just noise: the harness writes the
	// input into testdata/fuzz, where it becomes a seed that fails `go test` for
	// everyone until someone re-derives that it was never a hang.
	protocolFuzzRunTimeout = 5 * time.Second
)

const maxInt64 = int64(^uint64(0) >> 1)

// Protocol message-reader hardening budget is kept intentionally conservative and
// input-scaled so tiny payloads cannot trigger huge allocations unnoticed.
var (
	protocolReadMessageBudget    = allocBudget{base: 256 << 10, perByte: 256}
	protocolInspectMessageBudget = allocBudget{base: 256 << 10, perByte: 256}
	protocolAwarenessBudget      = allocBudget{base: 256 << 10, perByte: 512}
	protocolHandleBudget         = allocBudget{base: 768 << 10, perByte: 512}
)

type allocBudget struct {
	base    int64
	perByte int64
}

var protocolFuzzSeedCorpus = protocolHardeningSeeds()

func protocolHardeningSeeds() [][]byte {
	seedDoc := crdt.NewDoc("seed")
	t := seedDoc.GetText("t")
	t.Insert(0, "seed", crdt.Object{})
	t.Delete(0, 1)
	update, _ := crdt.EncodeStateAsUpdate(seedDoc, nil)
	updateV2, _ := crdt.EncodeStateAsUpdateV2(seedDoc, nil)

	h := crdt.NewAwareness(seedDoc)
	_ = h.SetLocalState(crdt.MakeObject("name", "seed"))
	aw := crdt.EncodeAwarenessUpdate(h, []crdt.Number{seedDoc.ClientID}, nil)
	awMsg := EncodeAwarenessUpdateMessage(aw)
	syncStep1 := EncodeSyncStep1(seedDoc)
	step2, _ := EncodeSyncStep2(seedDoc, []byte{})
	updateMsg := EncodeUpdate(update)

	return [][]byte{update, updateV2, syncStep1, step2, updateMsg, awMsg, aw}
}

func addProtocolFuzzSeeds(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add([]byte{0x80})
	f.Add([]byte{0x80, 0x80})
	f.Add([]byte{0x80, 0x7f})
	f.Add([]byte("hello world"))
	for _, seed := range protocolFuzzSeedCorpus {
		if len(seed) > 0 {
			f.Add(seed)
		}
	}
}

type protocolFuzzResult struct {
	panicValue any
	allocDelta int64
	duration   time.Duration
}

func protocolClampForFuzz(raw []byte) []byte {
	if len(raw) > protocolFuzzMaxInputBytes {
		raw = raw[:protocolFuzzMaxInputBytes]
	}
	copy := append([]byte(nil), raw...)
	if copy == nil {
		return []byte{}
	}
	return copy
}

func protocolAllocBudgetFor(b allocBudget, n int) int64 {
	if n < 0 {
		n = 0
	}
	if n > protocolFuzzMaxInputBytes {
		n = protocolFuzzMaxInputBytes
	}
	if b.perByte <= 0 {
		return b.base
	}
	if n == 0 {
		return b.base
	}
	need := int64(n) * b.perByte
	if need < 0 {
		return maxInt64
	}
	if maxInt64-b.base < need {
		return maxInt64
	}
	return b.base + need
}

func protocolNoPanicNoHangAndBound(t *testing.T, name string, raw []byte, budget allocBudget, run func([]byte)) {
	t.Helper()
	seed := protocolClampForFuzz(raw)

	resCh := make(chan protocolFuzzResult, 1)
	go func(in []byte) {
		res := protocolFuzzResult{}
		start := time.Now()
		defer func() {
			res.duration = time.Since(start)
			if r := recover(); r != nil {
				res.panicValue = r
			}
			resCh <- res
		}()

		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		run(in)
		runtime.ReadMemStats(&after)
		res.allocDelta = int64(after.TotalAlloc - before.TotalAlloc)
	}(seed)

	select {
	case got := <-resCh:
		if got.panicValue != nil {
			t.Fatalf("%s panicked on %d-byte input: %v", name, len(seed), got.panicValue)
		}
		if got.duration > protocolFuzzRunTimeout {
			t.Fatalf("%s exceeded %s on %d-byte input (took %s)", name, protocolFuzzRunTimeout, len(seed), got.duration)
		}
		limit := protocolAllocBudgetFor(budget, len(seed))
		if got.allocDelta > limit {
			t.Fatalf("%s allocated %d bytes, want <= %d on %d-byte input", name, got.allocDelta, limit, len(seed))
		}
	case <-time.After(protocolFuzzRunTimeout):
		t.Fatalf("%s hung on %d-byte input", name, len(seed))
	}
}

func TestProtocolFuzzAllocationScaleControl(t *testing.T) {
	seed := []byte{0x00, 0x00}
	if len(protocolFuzzSeedCorpus) > 0 && len(protocolFuzzSeedCorpus[0]) > 0 {
		seed = protocolFuzzSeedCorpus[0]
	}
	if len(seed) > protocolFuzzMaxInputBytes {
		seed = seed[:protocolFuzzMaxInputBytes]
	}
	base := append([]byte(nil), seed...)
	double := append(append([]byte(nil), base...), base...)

	protocolNoPanicNoHangAndBound(t, "InspectMessage:scale-1x", base, protocolInspectMessageBudget, func(in []byte) {
		_, _ = InspectMessage(in)
	})
	protocolNoPanicNoHangAndBound(t, "InspectMessage:scale-2x", double, protocolInspectMessageBudget, func(in []byte) {
		_, _ = InspectMessage(in)
	})
}

func FuzzReadMessage(f *testing.F) {
	addProtocolFuzzSeeds(f)
	f.Fuzz(func(t *testing.T, raw []byte) {
		protocolNoPanicNoHangAndBound(t, "ReadMessage", raw, protocolReadMessageBudget, func(in []byte) {
			buf := bytes.NewBuffer(append([]byte(nil), in...))
			_, _, _ = ReadMessage(buf)
		})
	})
}

func FuzzInspectMessage(f *testing.F) {
	addProtocolFuzzSeeds(f)
	f.Fuzz(func(t *testing.T, raw []byte) {
		protocolNoPanicNoHangAndBound(t, "InspectMessage", raw, protocolInspectMessageBudget, func(in []byte) {
			_, _ = InspectMessage(in)
		})
	})
}

func FuzzDecodeAwarenessMessage(f *testing.F) {
	addProtocolFuzzSeeds(f)
	f.Add([]byte("{\"n\":\"a\"}"))
	f.Add([]byte(""))
	f.Add([]byte("null"))
	f.Add([]byte("{bad}"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		protocolNoPanicNoHangAndBound(t, "DecodeAwarenessMessage", raw, protocolAwarenessBudget, func(in []byte) {
			_, _, _ = DecodeAwarenessMessage(in)
		})
	})
}

func FuzzHandleMessageWithOrigin(f *testing.F) {
	addProtocolFuzzSeeds(f)
	f.Fuzz(func(t *testing.T, raw []byte) {
		doc := crdt.NewDoc("seed")
		h := NewSyncHandler(doc)
		h.SetAwareness(crdt.NewAwareness(doc))
		var out bytes.Buffer
		protocolNoPanicNoHangAndBound(t, "HandleMessageWithOrigin", raw, protocolHandleBudget, func(in []byte) {
			_, _ = h.HandleMessageWithOrigin(in, &out, nil)
		})
	})
}
