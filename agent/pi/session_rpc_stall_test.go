package pi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tasercake/pi-connect/core"
)

func TestBoundedDurableCursorTracksAppendWithoutBodies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	body := `{"type":"message","id":"leaf-1","message":{"role":"user","content":"TOP-SECRET-BODY"}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := boundedDurableCursor(context.Background(), path, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first, "leaf-1") || strings.Contains(first, "TOP-SECRET") {
		t.Fatalf("unsafe or incomplete cursor: %q", first)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(`{"type":"message","id":"leaf-2","message":{"role":"assistant","content":"PRIVATE"}}` + "\n")
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	second, err := boundedDurableCursor(context.Background(), path, 2)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || !strings.Contains(second, "leaf-2") || strings.Contains(second, "PRIVATE") {
		t.Fatalf("cursor did not advance safely: first=%q second=%q", first, second)
	}
}

func TestBoundedDurableCursorNoSessionFileUsesMessageCount(t *testing.T) {
	got, err := boundedDurableCursor(context.Background(), "", 7)
	if err != nil || got != "messages:7" {
		t.Fatalf("cursor=%q err=%v", got, err)
	}
}

func TestBoundedDurableCursorHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := boundedDurableCursor(ctx, "", 1); err == nil {
		t.Fatal("expected context cancellation")
	}
}

func TestProbeStallUsesReadOnlyStateAndBodyFreeCursor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"message","id":"leaf-1","message":{"content":"DO-NOT-LOG"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writer := newRPCRecordingWriteCloser()
	s := newAcceptanceRPCSession(t, writer)
	s.turnMu.Lock()
	s.turn.active = true
	s.turnMu.Unlock()
	done := make(chan struct {
		snapshot core.StallProbeSnapshot
		err      error
	}, 1)
	go func() {
		snapshot, err := s.ProbeStall(context.Background())
		done <- struct {
			snapshot core.StallProbeSnapshot
			err      error
		}{snapshot, err}
	}()
	var command map[string]any
	select {
	case command = <-writer.writeCh:
	case <-time.After(time.Second):
		t.Fatal("get_state was not sent")
	}
	if command["type"] != "get_state" || writer.count() != 1 {
		t.Fatalf("probe command=%v writes=%d", command, writer.count())
	}
	line, _ := json.Marshal(map[string]any{
		"id":      rpcCommandID(t, command),
		"type":    "response",
		"command": "get_state",
		"success": true,
		"data": map[string]any{
			"isStreaming": false, "isCompacting": false, "sessionFile": path,
			"messageCount": 1, "pendingMessageCount": 0,
		},
	})
	s.handleResponse(line)
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if !got.snapshot.ProcessAlive || !got.snapshot.TransportResponsive || got.snapshot.Cursor == "" {
			t.Fatalf("incomplete probe snapshot: %#v", got.snapshot)
		}
		if strings.Contains(got.snapshot.Cursor, "DO-NOT-LOG") {
			t.Fatalf("cursor exposed entry body: %q", got.snapshot.Cursor)
		}
	case <-time.After(time.Second):
		t.Fatal("probe did not complete")
	}
}

func TestInterruptStallRevalidatesSettledAndAwaitingWithoutWrite(t *testing.T) {
	for _, phase := range []core.StallPhase{core.StallPhaseSettled, core.StallPhaseAwaitingAcceptance, core.StallPhaseCompacting} {
		t.Run(string(phase), func(t *testing.T) {
			writer := newRPCRecordingWriteCloser()
			s := newAcceptanceRPCSession(t, writer)
			s.setHealthPhase(phase, phase == core.StallPhaseCompacting)
			if phase == core.StallPhaseAwaitingAcceptance {
				s.turnMu.Lock()
				s.promptPending = true
				s.turnMu.Unlock()
			}
			if err := s.InterruptStall(context.Background()); err != nil {
				t.Fatal(err)
			}
			if writer.count() != 0 {
				t.Fatalf("phase %s wrote abort after revalidation", phase)
			}
		})
	}
}

func TestInterruptStallSerializesAndWritesAbortOnceForRunningPhase(t *testing.T) {
	writer := newRPCRecordingWriteCloser()
	s := newAcceptanceRPCSession(t, writer)
	s.setHealthPhase(core.StallPhaseRunning, false)
	done := make(chan error, 1)
	go func() { done <- s.InterruptStall(context.Background()) }()
	var command map[string]any
	select {
	case command = <-writer.writeCh:
	case <-time.After(time.Second):
		t.Fatal("abort not written")
	}
	if command["type"] != "abort" {
		t.Fatalf("command=%v", command)
	}
	respondRPC(s, rpcCommandID(t, command), "abort", true, "")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("abort did not complete")
	}
	if writer.count() != 1 {
		t.Fatalf("writes=%d", writer.count())
	}
}

func TestInterruptStallCancellationWhileMutationQueuedDoesNotWrite(t *testing.T) {
	writer := newRPCRecordingWriteCloser()
	s := newAcceptanceRPCSession(t, writer)
	s.setHealthPhase(core.StallPhaseRunning, false)
	<-s.mutatingGate // simulate prompt/compact acceptance window
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := s.InterruptStall(ctx); err == nil {
		t.Fatal("expected bounded admission cancellation")
	}
	if writer.count() != 0 {
		t.Fatalf("queued interrupt wrote %d commands", writer.count())
	}
	s.releaseMutating()
}
