package crdt

import "testing"

func TestStringLengthCountsUTF16CodeUnits(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		text string
		want Number
	}{
		{name: "empty", text: "", want: 0},
		{name: "ascii", text: "abc", want: 3},
		{name: "bmp", text: "é", want: 1},
		{name: "supplementary", text: "😀", want: 2},
		{name: "mixed", text: "a😀b", want: 4},
		{name: "invalid utf8 becomes replacement rune", text: string([]byte{0xff}), want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := stringLength(tc.text); got != tc.want {
				t.Fatalf("stringLength(%q) = %d, want %d", tc.text, got, tc.want)
			}
		})
	}
}

func TestBinarySearchUsesClockIntervals(t *testing.T) {
	t.Parallel()

	lengths := []Number{1, 4, 2, 1}
	structs := make([]abstractStruct, 0, len(lengths))
	clock := Number(0)
	for _, length := range lengths {
		structs = append(structs, &abstractStructBase{id: GenID(1, clock), length: length})
		clock += length
	}

	wants := []Number{0, 1, 1, 1, 1, 2, 2, 3}
	for target, want := range wants {
		got, err := findIndexSS(structs, target)
		if err != nil {
			t.Fatalf("FindIndexSS(clock=%d): %v", target, err)
		}
		if got != want {
			t.Fatalf("FindIndexSS(clock=%d) = %d, want %d", target, got, want)
		}
	}

	if _, err := findIndexSS(nil, 0); err == nil {
		t.Fatal("FindIndexSS on an empty store succeeded")
	}
}
