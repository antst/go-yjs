package crdt

import "testing"

func TestApplyEntryPointsReturnErrorAfterPartialTransactionCleanup(t *testing.T) {
	const inserted = "applied-before-delete-set-error"

	source := newDoc("apply-error-source", false, nil, nil, false, WithClientID(1))
	sourceText := source.GetText("t")
	var committedSourceStates []string
	sourceText.Observe(func(interface{}, interface{}) {
		committedSourceStates = append(committedSourceStates, sourceText.ToString())
	})
	Transact(source, func(*Transaction) {
		sourceText.Insert(0, inserted, Object{})
		sourceText.Delete(0, 3)
	}, "fixture", true)
	wantSource := sourceText.ToString()
	if wantSource == inserted {
		t.Fatal("fixture did not create a delete set")
	}
	if len(committedSourceStates) != 1 || committedSourceStates[0] != wantSource {
		t.Fatalf("source committed states=%q, want exactly final state %q", committedSourceStates, wantSource)
	}

	tests := []struct {
		name   string
		encode func(*Doc, []uint8) ([]uint8, error)
		apply  func(*Doc, []uint8) error
	}{
		{
			name:   "ApplyUpdate/v1",
			encode: EncodeStateAsUpdate,
			apply: func(doc *Doc, update []uint8) error {
				return ApplyUpdate(doc, update, "truncated")
			},
		},
		{
			name:   "readUpdate/v1",
			encode: EncodeStateAsUpdate,
			apply: func(doc *Doc, update []uint8) error {
				return readUpdate(newUpdateDecoderV1(update), doc, "truncated")
			},
		},
		{
			name:   "ApplyUpdateWith/v1",
			encode: EncodeStateAsUpdate,
			apply: func(doc *Doc, update []uint8) error {
				return applyUpdateWith(doc, update, "truncated", newUpdateDecoderV1(update))
			},
		},
		{
			name:   "ApplyUpdateV2/v2",
			encode: EncodeStateAsUpdateV2,
			apply: func(doc *Doc, update []uint8) error {
				return ApplyUpdateV2(doc, update, "truncated")
			},
		},
		{
			name:   "readUpdateV2/v2",
			encode: EncodeStateAsUpdateV2,
			apply: func(doc *Doc, update []uint8) error {
				decoder := newUpdateDecoderV2(update)
				return readUpdateV2(decoder, doc, "truncated", decoder)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			update, err := tc.encode(source, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(update) < 2 {
				t.Fatalf("fixture update is only %d byte(s)", len(update))
			}

			destination := newDoc("apply-error-destination", false, nil, nil, false, WithClientID(2))
			destinationText := destination.GetText("t")
			observerCalls := 0
			destinationText.Observe(func(interface{}, interface{}) {
				observerCalls++
			})

			if err := tc.apply(destination, update[:len(update)-1]); err == nil {
				t.Fatal("truncated update returned nil error")
			}
			if destination.trans != nil || len(destination.transCleanup) != 0 {
				t.Fatalf("apply returned before transaction cleanup: active=%v queued=%d", destination.trans != nil, len(destination.transCleanup))
			}
			if observerCalls != 1 {
				t.Fatalf("observer calls=%d, want 1 cleanup dispatch before the error returns", observerCalls)
			}

			// Pinned yjs applies the decoded structs, then throws while reading the
			// truncated delete set. Preserve that non-rollback behaviour: the receiver
			// contains the inserted text, but not the sender's deletion, and therefore
			// occupies a state that the sender never committed or exposed to observers.
			if got := destinationText.ToString(); got != inserted {
				t.Fatalf("partially applied text=%q, want pre-delete %q", got, inserted)
			} else if got == wantSource {
				t.Fatalf("truncated receiver unexpectedly equals sender state %q", wantSource)
			} else {
				for _, committed := range committedSourceStates {
					if got == committed {
						t.Fatalf("truncated receiver unexpectedly equals committed sender state %q", got)
					}
				}
			}

			control := newDoc("apply-error-control", false, nil, nil, false, WithClientID(3))
			if err := tc.apply(control, update); err != nil {
				t.Fatalf("valid update returned error: %v", err)
			}
			if got := control.GetText("t").ToString(); got != wantSource {
				t.Fatalf("valid update text=%q, want sender state %q", got, wantSource)
			}
		})
	}
}
