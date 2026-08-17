package protocol

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/antst/go-yjs/crdt"
)

type awarenessFrameOracleResult struct {
	MessageType uint8          `json:"messageType"`
	SyncType    int            `json:"syncType"`
	FrameHex    string         `json:"frameHex"`
	BodyHex     string         `json:"bodyHex"`
	ClientID    crdt.Number    `json:"clientID"`
	State       map[string]any `json:"state"`
}

func TestEmittedSyncFramesMatchYProtocols(t *testing.T) {
	doc := crdt.NewDoc("sync-frame-audit", crdt.WithGC(false), crdt.WithClientID(19))
	doc.GetText("t").Insert(0, "sync-frame", crdt.Object{})
	update, err := crdt.EncodeStateAsUpdate(doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	step2, err := EncodeSyncStep2(doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		frame    []byte
		syncType int
	}{
		{"SyncStep1", EncodeSyncStep1(doc), SyncMessageStep1},
		{"SyncStep2", step2, SyncMessageStep2},
		{"Update", EncodeUpdate(update), SyncMessageUpdate},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			result := runAwarenessFrameOracle(t, "decode-sync", test.frame)
			if result.MessageType != MessageSync || result.SyncType != test.syncType {
				t.Fatalf("y-protocols decoded outer/sub type = %d/%d, want %d/%d", result.MessageType, result.SyncType, MessageSync, test.syncType)
			}
			info, err := InspectMessage(test.frame)
			if err != nil {
				t.Fatalf("InspectMessage rejected frame accepted by y-protocols: %v", err)
			}
			if info.Type != result.MessageType || info.SyncType != result.SyncType {
				t.Fatalf("InspectMessage classified outer/sub type = %d/%d, y-protocols decoded %d/%d", info.Type, info.SyncType, result.MessageType, result.SyncType)
			}
		})
	}
}

func requireAwarenessFrameOracle(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		if os.Getenv("ORACLE_REQUIRED") == "1" {
			t.Fatalf("node is required for the awareness-frame differential: %v", err)
		}
		t.Skipf("node is unavailable: %v", err)
	}
	if _, err := os.Stat(filepath.Join("..", "fuzz", "node_modules", "y-protocols")); err != nil {
		msg := "../fuzz/node_modules/y-protocols is missing; link the repository's pinned fuzz/node_modules install"
		if os.Getenv("ORACLE_REQUIRED") == "1" {
			t.Fatal(msg)
		}
		t.Skip(msg)
	}
}

func runAwarenessFrameOracle(t *testing.T, mode string, frame []byte) awarenessFrameOracleResult {
	t.Helper()
	requireAwarenessFrameOracle(t)
	args := []string{filepath.Join("..", "fuzz", "protocol_awareness_frame.mjs"), mode}
	if frame != nil {
		args = append(args, hex.EncodeToString(frame))
	}
	cmd := exec.Command("node", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("y-protocols awareness-frame %s failed: %v\n%s", mode, err, stderr.Bytes())
	}
	var result awarenessFrameOracleResult
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode y-protocols awareness-frame %s result: %v\n%s", mode, err, output)
	}
	return result
}

// TestAwarenessMessageFrameMatchesYProtocols is intentionally bidirectional.
// Go-only round trips previously proved the encoder and decoder agreed with each
// other while both disagreed with y-websocket's length-prefixed awareness body.
func TestAwarenessMessageFrameMatchesYProtocols(t *testing.T) {
	t.Run("Go frame decodes in y-protocols", func(t *testing.T) {
		doc := crdt.NewDoc("awareness-frame-go", crdt.WithClientID(31337))
		awareness := crdt.NewAwareness(doc)
		t.Cleanup(awareness.Destroy)
		if err := awareness.SetLocalState(crdt.MakeObject("name", "go-peer", "cursor", crdt.MakeObject("anchor", 1, "head", 2))); err != nil {
			t.Fatal(err)
		}
		body := crdt.EncodeAwarenessUpdate(awareness, []crdt.Number{awareness.ClientID}, nil)
		frame := EncodeAwarenessUpdateMessage(body)
		info, err := InspectMessage(frame)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(info.Body, body) {
			t.Fatalf("InspectMessage body = %x, want %x", info.Body, body)
		}
		result := runAwarenessFrameOracle(t, "decode", frame)
		if result.MessageType != MessageAwareness {
			t.Fatalf("message type = %d, want %d", result.MessageType, MessageAwareness)
		}
		if result.BodyHex != hex.EncodeToString(body) {
			t.Fatalf("y-protocols extracted body %s, want %x", result.BodyHex, body)
		}
		if result.ClientID != awareness.ClientID || result.State["name"] != "go-peer" {
			t.Fatalf("y-protocols applied client/state = %d/%v, want %d/go-peer", result.ClientID, result.State, awareness.ClientID)
		}
	})

	t.Run("y-protocols frame decodes in Go", func(t *testing.T) {
		// Before the framing fix this failed with:
		//
		//   awareness frame is truncated: entry count 55 exceeds remaining 55 bytes
		//
		// The declared body length (55) was handed directly to the awareness-update
		// decoder and misread as its client-entry count. Preserve the mechanism here,
		// not just the fact that some decode error occurred.
		result := runAwarenessFrameOracle(t, "encode", nil)
		frame, err := hex.DecodeString(result.FrameHex)
		if err != nil {
			t.Fatalf("decode reference frame: %v", err)
		}
		info, err := InspectMessage(frame)
		if err != nil {
			t.Fatalf("InspectMessage rejected canonical y-protocols awareness frame: %v", err)
		}
		if info.Type != MessageAwareness || hex.EncodeToString(info.Body) != result.BodyHex {
			t.Fatalf("InspectMessage classified type/body = %d/%x, want %d/%s", info.Type, info.Body, MessageAwareness, result.BodyHex)
		}
		doc := crdt.NewDoc("awareness-frame-js")
		awareness := crdt.NewAwareness(doc)
		t.Cleanup(awareness.Destroy)
		handler := NewSyncHandler(doc)
		handler.SetAwareness(awareness)
		var reply bytes.Buffer
		messageType, err := handler.HandleMessage(frame, &reply)
		if err != nil {
			t.Fatalf("Go rejected canonical y-protocols awareness frame: %v", err)
		}
		if messageType != MessageAwareness || reply.Len() != 0 {
			t.Fatalf("HandleMessage returned type/reply = %d/%x, want %d/empty", messageType, reply.Bytes(), MessageAwareness)
		}
		state, ok := awareness.GetStates()[result.ClientID]
		if !ok || state.GetOr("name") != "js-peer" {
			t.Fatalf("Go did not apply y-protocols client %d: states=%v", result.ClientID, awareness.GetStates())
		}
		cursor, ok := state.GetOr("cursor").(crdt.Object)
		if !ok {
			t.Fatalf("decoded cursor has type %T, want Object: %v", state.GetOr("cursor"), state.GetOr("cursor"))
		}
		if fmt.Sprint(cursor.GetOr("anchor")) != "3" || fmt.Sprint(cursor.GetOr("head")) != "5" {
			t.Fatalf("decoded cursor = %v, want anchor=3 head=5", cursor)
		}
	})
}

func TestAwarenessFrameDecoderRejectsTruncatedLengthPrefix(t *testing.T) {
	doc := crdt.NewDoc("awareness-frame-truncated")
	handler := NewSyncHandler(doc)
	// Complete awareness type, then a truncated multi-byte VarUint body length.
	_, err := handler.HandleMessage([]byte{MessageAwareness, 0x80}, &bytes.Buffer{})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated awareness body length: want io.ErrUnexpectedEOF, got %v", err)
	}
}

func TestAwarenessOverrideReceivesDecodedBody(t *testing.T) {
	doc := crdt.NewDoc("awareness-frame-override")
	handler := NewSyncHandler(doc)
	body := EncodeAwarenessMessage(7, crdt.MakeObject("name", "override"))
	var received []byte
	handler.RegisterHandler(MessageAwareness, func(payload []byte) error {
		received = append([]byte(nil), payload...)
		return nil
	})
	frame := EncodeAwarenessUpdateMessage(body)
	if _, err := handler.HandleMessage(frame, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(received, body) {
		t.Fatalf("awareness override received %x, want decoded body %x", received, body)
	}
	if len(frame) < 2 || frame[0] != MessageAwareness || int(frame[1]) != len(body) {
		t.Fatalf("awareness frame = %x, want [type=1][body length=%d][body]", frame, len(body))
	}
}
