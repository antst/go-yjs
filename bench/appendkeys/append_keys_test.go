package appendkeys_test

import (
	"runtime"
	"testing"

	"github.com/antst/go-yjs/crdt"
)

const mapSize = 2000

var keysSink []string

func benchmarkMap() *crdt.YMap {
	doc := crdt.NewDoc("append-keys-bench", crdt.WithGC(false), crdt.WithClientID(1))
	m := doc.GetMap("m")
	for i := 0; i < mapSize; i++ {
		m.Set(mapKey(i), i)
	}
	_, _ = m.Keys(), m.Keys()
	return m
}

func mapKey(i int) string {
	return "k" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + string(rune('a'+(i/676)%26))
}

func releaseSink(b *testing.B) {
	b.Cleanup(func() {
		keysSink = nil
		runtime.GC()
	})
}

func BenchmarkKeys(b *testing.B) {
	m := benchmarkMap()
	releaseSink(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		keysSink = m.Keys()
	}
	runtime.KeepAlive(m)
}

func BenchmarkAppendKeysFresh(b *testing.B) {
	m := benchmarkMap()
	releaseSink(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst := make([]string, 0, mapSize)
		keysSink = m.AppendKeys(dst)
	}
	runtime.KeepAlive(m)
}

func BenchmarkAppendKeysReused(b *testing.B) {
	m := benchmarkMap()
	dst := make([]string, 0, mapSize)
	releaseSink(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst = m.AppendKeys(dst[:0])
		keysSink = dst
	}
	runtime.KeepAlive(m)
}
