// yrs benchmarks defined to match this project's Go suite operation-for-operation, so the three
// implementations (Go, JS reference, Rust) can be compared on identical workloads.
//
// Same 32-bit LCG in all three harnesses, so the random index sequences are identical rather than
// merely both-random. Setup is excluded from the timed region, matching Go's b.ResetTimer.
use std::collections::HashMap;
use std::sync::Arc;
use std::time::Instant;
use yrs::types::{Attrs, Delta, ToJson};
use yrs::updates::decoder::Decode;
use yrs::{
    Any, Array, ArrayRef, Doc, GetString, Map, MapRef, Options, ReadTxn, StateVector, Text,
    TextRef, Transact, Update, Xml, XmlElementPrelim, XmlFragment, XmlFragmentRef, XmlTextPrelim,
};

struct Lcg(u32);
impl Lcg {
    fn new() -> Self {
        Lcg(42)
    }
    fn next(&mut self) -> u32 {
        self.0 = self.0.wrapping_mul(1664525).wrapping_add(1013904223);
        self.0
    }
    fn intn(&mut self, n: usize) -> usize {
        if n == 0 { 0 } else { (self.next() as usize) % n }
    }
}

fn new_doc() -> Doc {
    let mut o = Options::default();
    o.client_id = 1;
    o.skip_gc = true; // mirrors gc:false on both other sides
    Doc::with_options(o)
}

fn ascii(n: usize) -> String {
    (0..n).map(|i| (b'a' + (i % 26) as u8) as char).collect()
}

// Three decimals, not zero. At {:>14.0} anything from 0.5 through 1.49 ns printed as "1", and
// status.py then consumed that rounded integer as an exact figure. XmlGetFirstChild is a ~1 ns
// operation, so the published page carried a 0.47x LOSS derived entirely from rounding — the same
// class of harness defect the page warns about for extreme WINS, in the direction nobody checks.
// A ~1 ns operation needs enough iterations that the loop is not dominated by timer resolution.
// At the block's 2,000 the whole measurement was ~2us, and combined with zero-decimal reporting the
// published page carried a LOSS derived entirely from rounding. Scoped to this probe so the
// expensive cases in the same block keep their own counts.
const FIRST_CHILD_ITERS: usize = 2_000_000;

fn report(name: &str, iters: usize, nanos: u128) {
    println!("{:<28}{:>14.3} ns/op   (iters={})", name, nanos as f64 / iters as f64, iters);
}

/// Time `f` over `iters` whole iterations; `f` builds and mutates its own document.
///
/// Building the document is INSIDE the timed region here on purpose: for the append, random-insert
/// and format cases the Go and JS harnesses time construction too, because construction is the
/// workload. Cases that instead measure operations against a PREBUILT document must use
/// `bench_setup`, or they charge fixture construction to the operation under test.
fn bench<F: FnMut()>(name: &str, iters: usize, mut f: F) {
    f(); // warmup
    let start = Instant::now();
    for _ in 0..iters { f(); }
    report(name, iters, start.elapsed().as_nanos());
}

/// Time `f` over `iters` iterations with per-iteration setup EXCLUDED from the measurement — the
/// direct counterpart of Go's `b.StopTimer()`/`b.StartTimer()` and the JS harness's
/// `perIterSetup: true`.
///
/// Required whenever the workload mutates its fixture and therefore cannot reuse one across
/// iterations. Timing the rebuild in that situation does not merely add noise, it adds a constant
/// far larger than the operation being measured, and it does so for only ONE of the four
/// implementations — which silently flatters the other three.
fn bench_setup<S, G: FnMut() -> S, F: FnMut(&mut S)>(name: &str, iters: usize, mut setup: G, mut f: F) {
    { let mut s = setup(); f(&mut s); } // warmup, discarded
    let mut nanos: u128 = 0;
    for _ in 0..iters {
        let mut s = setup();
        let start = Instant::now();
        f(&mut s);
        nanos += start.elapsed().as_nanos();
    }
    report(name, iters, nanos);
}

fn text_append(n: usize) {
    let doc = new_doc();
    let text: TextRef = doc.get_or_insert_text("t");
    for j in 0..n {
        let mut txn = doc.transact_mut();
        text.insert(&mut txn, j as u32, "x");
    }
}

fn text_insert_random(n: usize) {
    let mut rng = Lcg::new();
    let doc = new_doc();
    let text: TextRef = doc.get_or_insert_text("t");
    for j in 0..n {
        let idx = rng.intn(j + 1) as u32;
        let mut txn = doc.transact_mut();
        text.insert(&mut txn, idx, "y");
    }
}

fn built_doc(n: usize) -> Doc {
    let mut rng = Lcg::new();
    let doc = new_doc();
    let text: TextRef = doc.get_or_insert_text("t");
    for j in 0..n {
        let idx = rng.intn(j + 1) as u32;
        let mut txn = doc.transact_mut();
        text.insert(&mut txn, idx, "z");
    }
    doc
}

fn attrs_of(pairs: &[(&str, Any)]) -> Attrs {
    let mut m: HashMap<Arc<str>, Any> = HashMap::new();
    for (k, v) in pairs { m.insert(Arc::from(*k), v.clone()); }
    Attrs::from(m)
}

fn main() {
    println!("yrs — matched workloads, same LCG as the Go and JS suites\n");

    bench("TextAppendSmall", 20, || text_append(2_000));
    bench("TextAppendLarge", 5, || text_append(10_000));
    bench("TextInsertRandomSmall", 20, || text_insert_random(2_000));
    bench("TextInsertRandomLarge", 5, || text_insert_random(10_000));

    // Deletes mutate the fixture, so the 10k-char document is rebuilt per iteration OUTSIDE the
    // timed region — matching Go's b.StopTimer() around its setup and the JS harness's
    // perIterSetup. Length comes from text.len(), which is O(1); the earlier
    // get_string().chars().count() materialised the whole 10k string 2,000 times per iteration and
    // charged that to yrs alone, while Go, JS and ygo all use their O(1) length accessor.
    bench_setup("TextDeleteRandom", 10,
        || {
            let doc = new_doc();
            let text: TextRef = doc.get_or_insert_text("t");
            { let mut txn = doc.transact_mut(); text.insert(&mut txn, 0, &ascii(10_000)); }
            (doc, text)
        },
        |(doc, text)| {
            let mut rng = Lcg::new();
            for _ in 0..2_000 {
                let len = { let txn = doc.transact(); text.len(&txn) as usize };
                if len < 2 { break; }
                let mut txn = doc.transact_mut();
                text.remove_range(&mut txn, rng.intn(len - 1) as u32, 1);
            }
        });

    bench("TextFormatChurn", 20, || {
        let doc = new_doc();
        let text: TextRef = doc.get_or_insert_text("t");
        { let mut txn = doc.transact_mut(); text.insert(&mut txn, 0, &ascii(2_000)); }
        let mut rng = Lcg::new();
        for j in 0..1_000 {
            let a = match j % 3 {
                0 => attrs_of(&[("bold", Any::Bool(true))]),
                1 => attrs_of(&[("italic", Any::Bool(true))]),
                _ => attrs_of(&[("bold", Any::Null)]),
            };
            let start = rng.intn(2_000 - 20) as u32;
            let mut txn = doc.transact_mut();
            text.format(&mut txn, start, 20, a);
        }
    });

    // ToDelta on a formatted document — setup outside the timed region.
    {
        let doc = new_doc();
        let text: TextRef = doc.get_or_insert_text("t");
        { let mut txn = doc.transact_mut(); text.insert(&mut txn, 0, &ascii(2_000)); }
        let mut rng = Lcg::new();
        for j in 0..500 {
            let a = attrs_of(&[("bold", Any::Bool(j % 2 == 0))]);
            let s = rng.intn(2_000 - 20) as u32;
            let mut txn = doc.transact_mut();
            text.format(&mut txn, s, 20, a);
        }
        let iters = 2_000;
        let start = Instant::now();
        for _ in 0..iters {
            let txn = doc.transact();
            let _ = text.diff(&txn, yrs::types::text::YChange::identity);
        }
        report("TextToDelta", iters, start.elapsed().as_nanos());
    }

    bench("ArrayInsertSequential", 20, || {
        let doc = new_doc();
        let arr: ArrayRef = doc.get_or_insert_array("a");
        for j in 0..2_000i64 {
            let mut txn = doc.transact_mut();
            let len = arr.len(&txn);
            arr.insert(&mut txn, len, j);
        }
    });

    let keys = perf_keys();
    bench("MapSet", 20, || {
        let doc = new_doc();
        let m: MapRef = doc.get_or_insert_map("m");
        for j in 0..2_000i64 {
            let mut txn = doc.transact_mut();
            m.insert(&mut txn, keys[j as usize].as_str(), j);
        }
    });

    // Batched variants: identical work, ONE transaction instead of N.
    bench("TextAppendLargeBatched", 5, || {
        let doc = new_doc();
        let text: TextRef = doc.get_or_insert_text("t");
        let mut txn = doc.transact_mut();
        for j in 0..10_000u32 { text.insert(&mut txn, j, "x"); }
    });
    bench("ArrayInsertBatched", 20, || {
        let doc = new_doc();
        let arr: ArrayRef = doc.get_or_insert_array("a");
        let mut txn = doc.transact_mut();
        for j in 0..2_000i64 { let len = arr.len(&txn); arr.insert(&mut txn, len, j); }
    });
    bench("MapSetBatched", 20, || {
        let doc = new_doc();
        let m: MapRef = doc.get_or_insert_map("m");
        let mut txn = doc.transact_mut();
        for j in 0..2_000i64 { m.insert(&mut txn, keys[j as usize].as_str(), j); }
    });

    // ---- coverage cases ----------------------------------------------------------------
    // Mirrors perf_bench_ops_test.go so the read paths -- where this library's losses are
    // concentrated -- get a second reference point besides yjs.

    bench("ArrayPush", 20, || {
        let doc = new_doc();
        let arr: ArrayRef = doc.get_or_insert_array("a");
        for j in 0..2_000i64 {
            let mut txn = doc.transact_mut();
            arr.push_back(&mut txn, j);
        }
    });

    // Matched pair: identical work and delete pattern, differing only in push_back vs
    // insert-at-length. Any gap belongs to the append call alone.
    bench("ArrayPushWithTombstones", 20, || {
        let doc = new_doc();
        let arr: ArrayRef = doc.get_or_insert_array("a");
        for j in 0..2_000i64 {
            let mut txn = doc.transact_mut();
            arr.push_back(&mut txn, j);
            if j % 2 == 1 {
                let len = arr.len(&txn);
                if len > 0 { arr.remove_range(&mut txn, len - 1, 1); }
            }
        }
    });

    bench("ArrayInsertEndWithTombstones", 20, || {
        let doc = new_doc();
        let arr: ArrayRef = doc.get_or_insert_array("a");
        for j in 0..2_000i64 {
            let mut txn = doc.transact_mut();
            let len = arr.len(&txn);
            arr.insert(&mut txn, len, j);
            if j % 2 == 1 {
                let len = arr.len(&txn);
                if len > 0 { arr.remove_range(&mut txn, len - 1, 1); }
            }
        }
    });

    bench("ArrayUnshift", 20, || {
        let doc = new_doc();
        let arr: ArrayRef = doc.get_or_insert_array("a");
        for j in 0..2_000i64 {
            let mut txn = doc.transact_mut();
            arr.push_front(&mut txn, j);
        }
    });

    // Read paths: fixture built once, outside the timed region.
    {
        let doc = new_doc();
        let arr: ArrayRef = doc.get_or_insert_array("a");
        {
            let mut txn = doc.transact_mut();
            for j in 0..2_000i64 { arr.push_back(&mut txn, j); }
        }
        let iters = 2_000;

        let start = Instant::now();
        for _ in 0..iters {
            let txn = doc.transact();
            let v: Vec<_> = arr.iter(&txn).collect();
            std::hint::black_box(v);
        }
        report("ArrayToArray", iters, start.elapsed().as_nanos());

        let start = Instant::now();
        for _ in 0..iters {
            let txn = doc.transact();
            std::hint::black_box(arr.to_json(&txn));
        }
        report("ArrayToJson", iters, start.elapsed().as_nanos());

        // black_box on the accumulator, or the whole traversal is optimised away -- the Go side
        // hit exactly that and reported 0.98ns for walking 2000 elements.
        let start = Instant::now();
        for _ in 0..iters {
            let txn = doc.transact();
            let mut n = 0usize;
            for _ in arr.iter(&txn) { n += 1; }
            std::hint::black_box(n);
        }
        report("ArrayForEach", iters, start.elapsed().as_nanos());

        let start = Instant::now();
        for _ in 0..iters {
            let txn = doc.transact();
            let mut rng = Lcg::new();
            for _ in 0..2_000 {
                std::hint::black_box(arr.get(&txn, rng.intn(2_000) as u32));
            }
        }
        report("ArrayGetRandom", iters / 100, start.elapsed().as_nanos() / 100);
    }

    // Map read paths.
    {
        let doc = new_doc();
        let map: MapRef = doc.get_or_insert_map("m");
        {
            let mut txn = doc.transact_mut();
            for j in 0..2_000 { map.insert(&mut txn, map_key(j), j as i64); }
        }
        let iters = 2_000;

        let start = Instant::now();
        for _ in 0..iters {
            let txn = doc.transact();
            let v: Vec<_> = map.keys(&txn).collect();
            std::hint::black_box(v);
        }
        report("MapKeys", iters, start.elapsed().as_nanos());

        let start = Instant::now();
        for _ in 0..iters {
            let txn = doc.transact();
            let v: Vec<_> = map.values(&txn).collect();
            std::hint::black_box(v);
        }
        report("MapValues", iters, start.elapsed().as_nanos());

        let start = Instant::now();
        for _ in 0..iters {
            let txn = doc.transact();
            let v: Vec<_> = map.iter(&txn).collect();
            std::hint::black_box(v);
        }
        report("MapEntries", iters, start.elapsed().as_nanos());

        let start = Instant::now();
        for _ in 0..iters {
            let txn = doc.transact();
            std::hint::black_box(map.to_json(&txn));
        }
        report("MapToJson", iters, start.elapsed().as_nanos());

        // Full sweep, matching Go and JS: one lookup is below timer resolution.
        let start = Instant::now();
        for _ in 0..iters {
            let txn = doc.transact();
            for j in 0..2_000 { std::hint::black_box(map.contains_key(&txn, &map_key(j))); }
        }
        report("MapHas", iters, start.elapsed().as_nanos());

        let start = Instant::now();
        for _ in 0..iters {
            let txn = doc.transact();
            std::hint::black_box(map.len(&txn));
        }
        report("MapGetSize", iters, start.elapsed().as_nanos());
    }

    // MapClear mutates, so the fixture is rebuilt per iteration OUTSIDE the timed region.
    bench_setup("MapClear", 20,
        || {
            let doc = new_doc();
            let map: MapRef = doc.get_or_insert_map("m");
            {
                let mut txn = doc.transact_mut();
                for j in 0..2_000 { map.insert(&mut txn, map_key(j), j as i64); }
            }
            (doc, map)
        },
        |(doc, map)| {
            let mut txn = doc.transact_mut();
            map.clear(&mut txn);
        });

    // Text read paths.
    {
        let doc = new_doc();
        let text: TextRef = doc.get_or_insert_text("t");
        { let mut txn = doc.transact_mut(); text.insert(&mut txn, 0, &ascii(2_000)); }
        let iters = 2_000;

        let start = Instant::now();
        for _ in 0..iters {
            let txn = doc.transact();
            std::hint::black_box(text.get_string(&txn));
        }
        report("TextToString", iters, start.elapsed().as_nanos());
    }

    // Formatted text: the state a rich-text consumer is actually in, and the case where this
    // library is furthest behind yjs.
    {
        let doc = new_doc();
        let text: TextRef = doc.get_or_insert_text("t");
        { let mut txn = doc.transact_mut(); text.insert(&mut txn, 0, &ascii(2_000)); }
        {
            let mut rng = Lcg::new();
            for j in 0..500 {
                let mut txn = doc.transact_mut();
                let a = attrs_of(&[("bold", Any::Bool(j % 2 == 0))]);
                text.format(&mut txn, rng.intn(2_000 - 20) as u32, 20, a);
            }
        }
        let iters = 2_000;
        let start = Instant::now();
        for _ in 0..iters {
            let txn = doc.transact();
            std::hint::black_box(text.get_string(&txn));
        }
        report("TextToStringFormatted", iters, start.elapsed().as_nanos());
    }

    bench("TextInsertEmbed", 20, || {
        let doc = new_doc();
        let text: TextRef = doc.get_or_insert_text("t");
        { let mut txn = doc.transact_mut(); text.insert(&mut txn, 0, &ascii(2_000)); }
        for j in 0..200u32 {
            let mut txn = doc.transact_mut();
            let embed = attrs_of(&[("img", Any::String("x".into()))]);
            text.insert_embed(&mut txn, j, Any::Map(std::sync::Arc::new(
                embed.iter().map(|(k, v)| (k.to_string(), v.clone())).collect())));
        }
    });

    // ---- XML surface + remaining reads ---------------------------------------------------
    // yrs has no CSS-selector API, so XmlQuerySelector/QuerySelectorAll have no counterpart here
    // and are declared not-applicable in bench/status.py rather than left blank. Everything else
    // maps directly: successors() is the tree walk, get_string() the rendering.

    {
        let doc = new_doc();
        let frag: XmlFragmentRef = doc.get_or_insert_xml_fragment("x");
        {
            let mut txn = doc.transact_mut();
            for i in 0..500 {
                let el = frag.push_back(&mut txn, XmlElementPrelim::empty(
                    if i % 3 == 0 { "span" } else { "div" }));
                el.insert_attribute(&mut txn, "id", map_key(i));
                el.insert_attribute(&mut txn, "class", "row");
                el.push_back(&mut txn, XmlTextPrelim::new("cell"));
            }
        }
        let iters = 2_000;

        let start = Instant::now();
        for _ in 0..iters {
            let txn = doc.transact();
            std::hint::black_box(frag.get_string(&txn));
        }
        report("XmlToString", iters, start.elapsed().as_nanos());

        let start = Instant::now();
        for _ in 0..FIRST_CHILD_ITERS {
            // Materialise the child, do not merely test for its presence: the Go and JS
            // harnesses return the node itself, so `.is_some()` compared a existence check
            // against a materialisation.
            std::hint::black_box(frag.first_child());
        }
        report("XmlGetFirstChild", FIRST_CHILD_ITERS, start.elapsed().as_nanos());

        // successors() is yrs's tree walk, the counterpart of CreateTreeWalker.
        let start = Instant::now();
        for _ in 0..iters {
            let txn = doc.transact();
            let mut n = 0usize;
            for _ in frag.successors(&txn) { n += 1; }
            std::hint::black_box(n);
        }
        report("XmlCreateTreeWalker", iters, start.elapsed().as_nanos());

        // Slice: yrs has no slice(), so the equivalent is ONE traversal of the direct children
        // taking the same span. The first version used 250 indexed get() calls, each of which walks
        // from the start of the child list -- O(n^2) against our single O(n) copy, which is not a
        // substitution, it is a different algorithm. It reported a 150x that measured my harness.
        let start = Instant::now();
        for _ in 0..iters {
            let txn = doc.transact();
            let v: Vec<_> = frag.children(&txn).take(250).collect();
            std::hint::black_box(v);
        }
        report("XmlSlice", iters, start.elapsed().as_nanos());
    }

    // XML element attributes, on an element carrying 50 of them like the Go and JS fixtures.
    {
        let doc = new_doc();
        let frag: XmlFragmentRef = doc.get_or_insert_xml_fragment("x");
        let el = {
            let mut txn = doc.transact_mut();
            let el = frag.push_back(&mut txn, XmlElementPrelim::empty("div"));
            for i in 0..50 { el.insert_attribute(&mut txn, map_key(i), "v"); }
            el
        };
        let iters = 2_000;

        // XmlSetAttribute moved OUT of this shared-fixture block: see the bench_setup call below.
        // Replacing a key appends an item and tombstones the old one, so overwriting `id` here grew
        // this element's history by 2000 items before the two read benchmarks below ever ran —
        // while the Go and JS harnesses measure their reads on a freshly built element. The reads
        // now run first, on a fixture with no overwrite history, matching the other two.

        let start = Instant::now();
        for _ in 0..iters {
            let txn = doc.transact();
            for i in 0..50 { std::hint::black_box(el.get_attribute(&txn, &map_key(i))); }
        }
        report("XmlGetAttribute", iters, start.elapsed().as_nanos());

        let start = Instant::now();
        for _ in 0..iters {
            let txn = doc.transact();
            let v: Vec<_> = el.attributes(&txn).collect();
            std::hint::black_box(v);
        }
        report("XmlGetAttributes", iters, start.elapsed().as_nanos());
    }

    // Replacement mutates and accumulates history, so the element is rebuilt per iteration OUTSIDE
    // the timed region and the timed region performs a FIXED number of replacements. That count
    // must equal xmlSetAttributeOverwrites in perf_bench_xml_test.go and
    // XML_SET_ATTRIBUTE_OVERWRITES in fuzz/perf_bench.mjs; one reported op is that many
    // replacements. Previously this ran 2000 replacements on one shared element while Go autoscaled
    // into millions and JS ran to a time budget — three history depths compared as three speeds.
    bench_setup("XmlSetAttribute", 200,
        || {
            let doc = new_doc();
            let frag: XmlFragmentRef = doc.get_or_insert_xml_fragment("x");
            let el = {
                let mut txn = doc.transact_mut();
                let el = frag.push_back(&mut txn, XmlElementPrelim::empty("div"));
                for i in 0..50 { el.insert_attribute(&mut txn, map_key(i), "v"); }
                el
            };
            (doc, el)
        },
        |(doc, el)| {
            // One implicit transaction per public setAttribute call, matching Go and yjs; wrapping
            // the whole loop in one Rust transaction would measure the batched shape against their
            // non-batched shape.
            for _ in 0..100 {
                let mut txn = doc.transact_mut();
                el.insert_attribute(&mut txn, "id", "x");
            }
        });

    // Removal mutates, so the element is rebuilt per iteration OUTSIDE the timed region.
    bench_setup("XmlRemoveAttribute", 20,
        || {
            let doc = new_doc();
            let frag: XmlFragmentRef = doc.get_or_insert_xml_fragment("x");
            let el = {
                let mut txn = doc.transact_mut();
                let el = frag.push_back(&mut txn, XmlElementPrelim::empty("div"));
                for i in 0..50 { el.insert_attribute(&mut txn, map_key(i), "v"); }
                el
            };
            (doc, el)
        },
        |(doc, el)| {
            // Go and yjs open one implicit transaction per public removeAttribute call. Keeping
            // one Rust transaction around this whole loop silently measured the batched shape
            // against their non-batched shape.
            for i in 0..50 {
                let mut txn = doc.transact_mut();
                el.remove_attribute(&mut txn, &map_key(i));
            }
        });

    bench_setup("XmlInsertAfter", 20,
        || {
            let doc = new_doc();
            let frag: XmlFragmentRef = doc.get_or_insert_xml_fragment("x");
            { let mut txn = doc.transact_mut();
              frag.push_back(&mut txn, XmlElementPrelim::empty("div")); }
            (doc, frag)
        },
        |(doc, frag)| {
            // yrs has no insertAfter; inserting at index 1 is the same position.
            // One transaction per insert matches Go/Yjs's public-call lifecycle. The previous
            // outer transaction compared batched yrs against non-batched Go and yjs.
            for _ in 0..200 {
                let mut txn = doc.transact_mut();
                frag.insert(&mut txn, 1, XmlElementPrelim::empty("div"));
            }
        });

    // Remaining reads that had no yrs counterpart yet.
    {
        let doc = new_doc();
        let text: TextRef = doc.get_or_insert_text("t");
        { let mut txn = doc.transact_mut(); text.insert(&mut txn, 0, &ascii(2_000)); }
        let iters = 2_000;
        // yjs toJSON() on a text returns its string, so get_string is the faithful counterpart.
        let start = Instant::now();
        for _ in 0..iters {
            let txn = doc.transact();
            std::hint::black_box(text.get_string(&txn));
        }
        report("TextToJson", iters, start.elapsed().as_nanos());
    }

    // ONE apply_delta carrying a 200-operation delta, which is what the Go and JS harnesses do
    // (`t.ApplyDelta(delta)` / `t.applyDelta(delta)` with a 200-element delta). The first version
    // here made 200 SEPARATE one-operation apply_delta calls inside a single transaction, which is
    // a different workload twice over: it measured per-call overhead the others never pay, and it
    // batched 200 operations into one transaction where the others use one. Same defect class as
    // the XmlInsertAfter/XmlRemoveAttribute mismatch.
    // Go creates the Doc/Text under b.StopTimer and times ONLY ApplyDelta, with the delta built
    // once outside the loop. This previously timed Doc creation, Text lookup AND construction of
    // the 200-entry delta inside the measured closure, so the yrs figure covered work the Go
    // figure excluded. bench_setup moves all of it out.
    bench_setup("TextApplyDelta", 20,
        || {
            let doc = new_doc();
            let text: TextRef = doc.get_or_insert_text("t");
            let delta: Vec<Delta<String>> = (0..200)
                .map(|j| {
                    let a = attrs_of(&[("bold", Any::Bool(j % 2 == 0))]);
                    Delta::<String>::Inserted("chunk".to_string(), Some(Box::new(a)))
                })
                .collect();
            (doc, text, delta)
        },
        |(doc, text, delta)| {
            let mut txn = doc.transact_mut();
            text.apply_delta(&mut txn, delta.clone());
        });

    {
        let doc = new_doc();
        let arr: ArrayRef = doc.get_or_insert_array("a");
        {
            let mut txn = doc.transact_mut();
            for j in 0..2_000i64 { arr.push_back(&mut txn, j); }
        }
        let iters = 2_000;

        // yrs has no splice()/map(); the faithful equivalents are a bounded range read and an
        // iterate-and-transform, which is what the Go and JS versions cost too.
        let start = Instant::now();
        for _ in 0..iters {
            let txn = doc.transact();
            let v: Vec<_> = arr.iter(&txn).take(1_999).collect();
            std::hint::black_box(v);
        }
        report("ArraySplice", iters, start.elapsed().as_nanos());

        let start = Instant::now();
        for _ in 0..iters {
            let txn = doc.transact();
            let v: Vec<_> = arr.iter(&txn).map(|v| v).collect();
            std::hint::black_box(v);
        }
        report("ArrayMap", iters, start.elapsed().as_nanos());
    }

    // Codec: 10k-op document, built once outside the timed regions.
    {
        let doc = built_doc(10_000);
        let update = doc.transact().encode_state_as_update_v1(&StateVector::default());

        let iters = 2_000;
        let start = Instant::now();
        for _ in 0..iters {
            let _ = doc.transact().encode_state_as_update_v1(&StateVector::default());
        }
        report("EncodeV1", iters, start.elapsed().as_nanos());

        let start = Instant::now();
        for _ in 0..iters {
            let sink = new_doc();
            let mut txn = sink.transact_mut();
            txn.apply_update(Update::decode_v1(&update).unwrap()).unwrap();
        }
        report("ApplyV1", iters, start.elapsed().as_nanos());

        // V2 was previously absent from this harness, which left the EncodeV2/ApplyV2 rows blank
        // and reading as though yrs lacked the format. It does not -- encode_state_as_update_v2
        // and Update::decode_v2 are both public API -- so the blanks were a gap in the measurement,
        // not in the implementation. Same document, same iteration count as V1.
        let update_v2 = doc.transact().encode_state_as_update_v2(&StateVector::default());

        let start = Instant::now();
        for _ in 0..iters {
            let _ = doc.transact().encode_state_as_update_v2(&StateVector::default());
        }
        report("EncodeV2", iters, start.elapsed().as_nanos());

        let start = Instant::now();
        for _ in 0..iters {
            let sink = new_doc();
            let mut txn = sink.transact_mut();
            txn.apply_update(Update::decode_v2(&update_v2).unwrap()).unwrap();
        }
        report("ApplyV2", iters, start.elapsed().as_nanos());
    }

    // Concurrent merge: two independently-edited documents applied into one.
    {
        let mk = |client: u64, tag: &str| {
            let mut o = Options::default();
            o.client_id = client;
            o.skip_gc = true;
            let d = Doc::with_options(o);
            let t: TextRef = d.get_or_insert_text("t");
            let mut rng = Lcg::new();
            for j in 0..2_000 {
                let idx = rng.intn(j + 1) as u32;
                let mut txn = d.transact_mut();
                t.insert(&mut txn, idx, tag);
            }
            let out = d.transact().encode_state_as_update_v1(&StateVector::default());
            out
        };
        let (u1, u2) = (mk(1, "a"), mk(2, "b"));
        let iters = 500;
        let start = Instant::now();
        for _ in 0..iters {
            let d = new_doc();
            let mut txn = d.transact_mut();
            txn.apply_update(Update::decode_v1(&u1).unwrap()).unwrap();
            txn.apply_update(Update::decode_v1(&u2).unwrap()).unwrap();
        }
        report("ConcurrentMerge", iters, start.elapsed().as_nanos());
    }

    // ygo-shaped: ONE random single-char insert into a ~100k-char text, at fixed counts.
    for iters in [10usize, 1_000, 10_000] {
        let doc = new_doc();
        let text: TextRef = doc.get_or_insert_text("t");
        { let mut txn = doc.transact_mut(); text.insert(&mut txn, 0, &ascii(100_000)); }
        let mut rng = Lcg::new();
        let start = Instant::now();
        for _ in 0..iters {
            let len = { let txn = doc.transact(); text.len(&txn) as usize };
            let idx = rng.intn(len) as u32;
            let mut txn = doc.transact_mut();
            text.insert(&mut txn, idx, "x");
        }
        report("YText_RandomInsert_100k", iters, start.elapsed().as_nanos());
    }
}

// Same key derivation as the Go mapKey and the JS mapKey, so all three build keys of identical
// length and distribution rather than one side getting cheap short keys.
/// The 2000 map keys, built ONCE as a fixture. Building them inside the timed loop made these rows
/// partly a comparison of each language's string formatting rather than of its CRDT: Go and ygo
/// used fmt.Sprintf, this harness used format!, and yjs used direct concatenation.
fn perf_keys() -> Vec<String> {
    (0..2_000).map(|i| format!("k{}", i)).collect()
}

fn map_key(j: usize) -> String {
    format!("k{}{}{}",
        (b'a' + (j % 26) as u8) as char,
        (b'a' + ((j / 26) % 26) as u8) as char,
        (b'a' + ((j / 676) % 26) as u8) as char)
}
