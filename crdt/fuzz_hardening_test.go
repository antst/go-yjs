package crdt

import (
	"runtime"
	"testing"
	"time"
)

type allocBudget struct {
	base    int64
	perByte int64
}

const (
	crdtFuzzMaxInputBytes = 64 * 1024
	// A real hang is UNBOUNDED, so a generous deadline loses no detection power
	// while a tight one manufactures findings. At 150ms this fired on a 40-byte
	// input that actually decodes in 248 MICROseconds — a 600x margin — because
	// Go runs fuzz workers in parallel and the host was also running the suite.
	// The cost of that false positive is not just noise: the harness writes the
	// input into testdata/fuzz, where it becomes a seed that fails `go test` for
	// everyone until someone re-derives that it was never a hang.
	crdtFuzzRunTimeout      = 5 * time.Second
	crdtPairControlMaxInput = 2048
)

const maxInt64 = int64(^uint64(0) >> 1)

// Allocation guard policy for untrusted-byte entry points.
//
// Runtime context used while calibrating these constants:
//
//	runtime.Version(): go1.26.5
//	goos/goarch:       darwin/arm64
//	seed corpus:       6 valid payloads from this package's real update/snapshot/path
//	                   seed builder + malformed framing edge cases.
//
// The values are input-scaled (base + perByte*len) and bounded by
// crdtFuzzMaxInputBytes so a 20-byte payload cannot hide a superlinear
// allocation explosion behind tiny input.
var (
	crdtDecodeAllocBudget = allocBudget{base: 3 << 20, perByte: 512}
	crdtApplyAllocBudget  = allocBudget{base: 4 << 20, perByte: 1_024}
	crdtDiffAllocBudget   = allocBudget{base: 4 << 20, perByte: 1_024}
	crdtEncodeAllocBudget = allocBudget{base: 2 << 20, perByte: 512}
	crdtParseAllocBudget  = allocBudget{base: 1 << 20, perByte: 256}
)

var crdtFuzzSeedCorpus = crdtHardeningSeeds()

// Seeds from this package's own valid-corpus-like constructor.
func crdtHardeningSeeds() [][]byte {
	doc := newDoc("seed", false, defaultGCFilter, nil, false, WithClientID(1))
	txt := doc.GetText("t")
	txt.Insert(0, "ab", Object{})
	arr := doc.GetArray("a")
	arr.Push(ArrayAny{1})
	m := doc.GetMap("m")
	m.Set("k", 1)
	txt.Delete(0, 1)

	v1, err := EncodeStateAsUpdate(doc, nil)
	if err != nil {
		return nil
	}
	v2, err := EncodeStateAsUpdateV2(doc, nil)
	if err != nil {
		return nil
	}
	sv := encodeStateVectorWith(doc, getStateVector(doc.store), newUpdateEncoderV1())
	genID := GenID(1, 0)
	rp := EncodeRelativePosition(&RelativePosition{Item: &genID, Assoc: 0})
	snap, err := EncodeSnapshot(newSnapshot(newDeleteSetFromStructStore(doc.store), getStateVector(doc.store)))
	if err != nil {
		return nil
	}

	aw := NewAwareness(doc)
	_ = aw.SetLocalState(MakeObject("name", "a"))
	awUpdate := EncodeAwarenessUpdate(aw, []Number{doc.ClientID}, nil)

	return [][]byte{v1, v2, sv, rp, snap, awUpdate}
}

func addCrdtFuzzSeeds(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add([]byte{0x01})
	f.Add([]byte{0x80})
	f.Add([]byte{0xff})
	f.Add([]byte{0x80, 0x80})
	f.Add([]byte{0x80, 0x7f})
	f.Add([]byte("hello world"))
	for _, seed := range crdtFuzzSeedCorpus {
		if len(seed) > 0 {
			f.Add(seed)
		}
	}
}

type crdtFuzzResult struct {
	panicValue any
	allocDelta int64
	duration   time.Duration
}

func crdtClampForFuzz(raw []byte) []byte {
	if len(raw) > crdtFuzzMaxInputBytes {
		raw = raw[:crdtFuzzMaxInputBytes]
	}
	dup := append([]byte(nil), raw...)
	if dup == nil {
		return []byte{}
	}
	return dup
}

func crdtAllocBudgetFor(b allocBudget, n int) int64 {
	if n < 0 {
		n = 0
	}
	if n > crdtFuzzMaxInputBytes {
		n = crdtFuzzMaxInputBytes
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

func crdtNoPanicNoHangAndBound(t *testing.T, name string, raw []byte, budget allocBudget, run func([]byte)) {
	t.Helper()
	seed := crdtClampForFuzz(raw)

	resCh := make(chan crdtFuzzResult, 1)
	go func(in []byte) {
		res := crdtFuzzResult{}
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
		if got.duration > crdtFuzzRunTimeout {
			t.Fatalf("%s exceeded %s on %d-byte input (took %s)", name, crdtFuzzRunTimeout, len(seed), got.duration)
		}
		limit := crdtAllocBudgetFor(budget, len(seed))
		if got.allocDelta > limit {
			t.Fatalf("%s allocated %d bytes, want <= %d on %d-byte input", name, got.allocDelta, limit, len(seed))
		}
	case <-time.After(crdtFuzzRunTimeout):
		t.Fatalf("%s hung on %d-byte input", name, len(seed))
	}
}

func TestCrdtFuzzAllocationScaleControl(t *testing.T) {
	for _, seed := range crdtFuzzSeedCorpus {
		if len(seed) == 0 || len(seed) > crdtPairControlMaxInput {
			continue
		}
		seedDelta := append([]byte(nil), seed...)
		doubled := append(append([]byte(nil), seedDelta...), seedDelta...)

		crdtNoPanicNoHangAndBound(t, "DecodeRelativePosition:scale-1x", seedDelta, crdtDecodeAllocBudget, func(in []byte) {
			_, _ = DecodeRelativePosition(in)
		})
		crdtNoPanicNoHangAndBound(t, "DecodeRelativePosition:scale-2x", doubled, crdtDecodeAllocBudget, func(in []byte) {
			_, _ = DecodeRelativePosition(in)
		})
		return
	}
}

func FuzzApplyUpdate(f *testing.F) {
	addCrdtFuzzSeeds(f)
	f.Fuzz(func(t *testing.T, raw []byte) {
		doc := newDoc("fuzz", false, defaultGCFilter, nil, false, WithClientID(1))
		crdtNoPanicNoHangAndBound(t, "ApplyUpdate", raw, crdtApplyAllocBudget, func(in []byte) {
			_ = ApplyUpdate(doc, in, nil)
		})
	})
}

func FuzzApplyUpdateV2(f *testing.F) {
	addCrdtFuzzSeeds(f)
	f.Fuzz(func(t *testing.T, raw []byte) {
		doc := newDoc("fuzz", false, defaultGCFilter, nil, false, WithClientID(1))
		crdtNoPanicNoHangAndBound(t, "ApplyUpdateV2", raw, crdtApplyAllocBudget, func(in []byte) {
			_ = ApplyUpdateV2(doc, in, nil)
		})
	})
}

func FuzzDecodeStateVector(f *testing.F) {
	addCrdtFuzzSeeds(f)
	f.Fuzz(func(t *testing.T, raw []byte) {
		crdtNoPanicNoHangAndBound(t, "DecodeStateVector", raw, crdtDecodeAllocBudget, func(in []byte) {
			_, _ = DecodeStateVector(in)
		})
	})
}

func FuzzDecodeSnapshot(f *testing.F) {
	addCrdtFuzzSeeds(f)
	f.Fuzz(func(t *testing.T, raw []byte) {
		crdtNoPanicNoHangAndBound(t, "DecodeSnapshot", raw, crdtDecodeAllocBudget, func(in []byte) {
			_, _ = DecodeSnapshot(in)
		})
	})
}

func FuzzDecodeSnapshotV1(f *testing.F) {
	addCrdtFuzzSeeds(f)
	f.Fuzz(func(t *testing.T, raw []byte) {
		crdtNoPanicNoHangAndBound(t, "DecodeSnapshotV1", raw, crdtDecodeAllocBudget, func(in []byte) {
			_, _ = DecodeSnapshotV1(in)
		})
	})
}

func FuzzDecodeSnapshotV2(f *testing.F) {
	addCrdtFuzzSeeds(f)
	f.Fuzz(func(t *testing.T, raw []byte) {
		crdtNoPanicNoHangAndBound(t, "DecodeSnapshotV2", raw, crdtDecodeAllocBudget, func(in []byte) {
			_, _ = DecodeSnapshotV2(in)
		})
	})
}

func FuzzDecodeRelativePosition(f *testing.F) {
	addCrdtFuzzSeeds(f)
	f.Fuzz(func(t *testing.T, raw []byte) {
		crdtNoPanicNoHangAndBound(t, "DecodeRelativePosition", raw, crdtDecodeAllocBudget, func(in []byte) {
			_, _ = DecodeRelativePosition(in)
		})
	})
}

func FuzzDiffUpdate(f *testing.F) {
	addCrdtFuzzSeeds(f)
	f.Fuzz(func(t *testing.T, raw []byte) {
		crdtNoPanicNoHangAndBound(t, "DiffUpdate", raw, crdtDiffAllocBudget, func(in []byte) {
			_, _ = DiffUpdate(in, in)
		})
	})
}

func FuzzDiffUpdateV2(f *testing.F) {
	addCrdtFuzzSeeds(f)
	f.Fuzz(func(t *testing.T, raw []byte) {
		crdtNoPanicNoHangAndBound(t, "DiffUpdateV2", raw, crdtDiffAllocBudget, func(in []byte) {
			_, _ = DiffUpdateV2(in, in)
		})
	})
}

func FuzzMergeUpdates(f *testing.F) {
	addCrdtFuzzSeeds(f)
	f.Fuzz(func(t *testing.T, raw []byte) {
		crdtNoPanicNoHangAndBound(t, "MergeUpdates", raw, crdtApplyAllocBudget, func(in []byte) {
			_, _ = MergeUpdates([][]byte{in})
		})
	})
}

func FuzzMergeUpdatesV2(f *testing.F) {
	addCrdtFuzzSeeds(f)
	f.Fuzz(func(t *testing.T, raw []byte) {
		crdtNoPanicNoHangAndBound(t, "MergeUpdatesV2", raw, crdtApplyAllocBudget, func(in []byte) {
			_, _ = MergeUpdatesV2([][]byte{in})
		})
	})
}

func FuzzEncodeStateVectorFromUpdate(f *testing.F) {
	addCrdtFuzzSeeds(f)
	f.Fuzz(func(t *testing.T, raw []byte) {
		crdtNoPanicNoHangAndBound(t, "EncodeStateVectorFromUpdate", raw, crdtEncodeAllocBudget, func(in []byte) {
			_, _ = EncodeStateVectorFromUpdate(in)
		})
	})
}

func FuzzEncodeStateVectorFromUpdateV2(f *testing.F) {
	addCrdtFuzzSeeds(f)
	f.Fuzz(func(t *testing.T, raw []byte) {
		crdtNoPanicNoHangAndBound(t, "EncodeStateVectorFromUpdateV2", raw, crdtEncodeAllocBudget, func(in []byte) {
			_, _ = EncodeStateVectorFromUpdateV2(in)
		})
	})
}

func FuzzObfuscateUpdate(f *testing.F) {
	addCrdtFuzzSeeds(f)
	f.Fuzz(func(t *testing.T, raw []byte) {
		crdtNoPanicNoHangAndBound(t, "ObfuscateUpdate", raw, crdtApplyAllocBudget, func(in []byte) {
			_, _ = ObfuscateUpdate(in)
		})
	})
}

func FuzzApplyAwarenessUpdate(f *testing.F) {
	addCrdtFuzzSeeds(f)
	f.Fuzz(func(t *testing.T, raw []byte) {
		doc := newDoc("fuzz", false, defaultGCFilter, nil, false, WithClientID(1))
		aw := NewAwareness(doc)
		crdtNoPanicNoHangAndBound(t, "ApplyAwarenessUpdate", raw, crdtApplyAllocBudget, func(in []byte) {
			_ = ApplyAwarenessUpdate(aw, in, nil)
		})
	})
}

func FuzzParseAwarenessStateJSON(f *testing.F) {
	addCrdtFuzzSeeds(f)
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))
	f.Add([]byte(``))
	f.Add([]byte("{"))
	f.Add([]byte("[]"))
	f.Add([]byte(`"`))
	f.Add([]byte("\"\""))
	f.Fuzz(func(t *testing.T, raw []byte) {
		crdtNoPanicNoHangAndBound(t, "ParseAwarenessStateJSON", raw, crdtParseAllocBudget, func(in []byte) {
			_, _ = ParseAwarenessStateJSON(string(in))
		})
	})
}
