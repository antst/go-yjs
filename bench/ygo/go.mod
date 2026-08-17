// Separate module: reearth/ygo pulls SQLite, Redis and a WebSocket library, and none of that
// belongs in the library under test's go.sum for the sake of a benchmark.
module ygobench

go 1.23

require github.com/reearth/ygo v1.48.0
