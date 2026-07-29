package pi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tasercake/pi-connect/core"
)

func rpcAssistant(text, errMsg string) map[string]any {
	msg := map[string]any{"role": "assistant"}
	if text != "" {
		msg["stopReason"] = "stop"
		msg["content"] = []any{map[string]any{"type": "text", "text": text}}
	}
	if errMsg != "" {
		msg["stopReason"] = "error"
		msg["errorMessage"] = errMsg
	}
	return msg
}

func terminalRPCEvents(events []core.Event) []core.Event {
	var terminal []core.Event
	for _, event := range events {
		if event.Type == core.EventResult || event.Type == core.EventError {
			terminal = append(terminal, event)
		}
	}
	return terminal
}

func TestPiRPCRetrySuccessWaitsForAgentSettled(t *testing.T) {
	s := newTestRPCSession()
	defer s.Close()

	s.handleAgentEvent(map[string]any{"type": "agent_start"})
	s.handleAgentEvent(map[string]any{"type": "message_update", "assistantMessageEvent": map[string]any{"type": "text_delta", "delta": "failed partial"}})
	s.handleAgentEvent(map[string]any{"type": "message_end", "message": rpcAssistant("", "You can retry this request")})
	s.handleAgentEvent(map[string]any{"type": "agent_end", "willRetry": true, "messages": []any{rpcAssistant("", "You can retry this request")}})
	if got := drainRPCEvents(s); len(got) != 0 {
		t.Fatalf("failed attempt emitted outward events: %+v", got)
	}

	s.handleAgentEvent(map[string]any{"type": "auto_retry_start", "attempt": float64(1), "maxAttempts": float64(3)})
	s.handleAgentEvent(map[string]any{"type": "agent_start"})
	s.handleAgentEvent(map[string]any{"type": "message_update", "assistantMessageEvent": map[string]any{"type": "text_delta", "delta": "successful answer"}})
	s.handleAgentEvent(map[string]any{"type": "message_end", "message": rpcAssistant("successful answer", "")})
	s.handleAgentEvent(map[string]any{"type": "agent_end", "willRetry": false, "messages": []any{rpcAssistant("successful answer", "")}})
	for _, event := range drainRPCEvents(s) {
		if event.Type == core.EventResult || event.Type == core.EventError || event.Type == core.EventText {
			t.Fatalf("pre-settlement outward response event: %+v", event)
		}
	}

	s.handleAgentEvent(map[string]any{"type": "agent_settled"})
	got := terminalRPCEvents(drainRPCEvents(s))
	if len(got) != 1 || got[0].Type != core.EventResult || got[0].Content != "successful answer" {
		t.Fatalf("settled events = %+v, want one successful result", got)
	}
	if !s.Alive() {
		t.Fatal("provider retry killed persistent Pi session")
	}
}

func TestPiRPCRetryPreservesCompletedToolTurnText(t *testing.T) {
	s := newTestRPCSession()
	defer s.Close()

	completed := map[string]any{"role": "assistant", "stopReason": "toolUse", "content": []any{map[string]any{"type": "text", "text": "preface"}}}
	failed := rpcAssistant("", "retry final call")
	s.handleAgentEvent(map[string]any{"type": "agent_start"})
	s.handleAgentEvent(map[string]any{"type": "agent_end", "willRetry": true, "messages": []any{completed, failed}})
	s.handleAgentEvent(map[string]any{"type": "auto_retry_start"})
	s.handleAgentEvent(map[string]any{"type": "agent_start"})
	s.handleAgentEvent(map[string]any{"type": "agent_end", "messages": []any{rpcAssistant("clean", "")}})
	s.handleAgentEvent(map[string]any{"type": "agent_settled"})

	got := terminalRPCEvents(drainRPCEvents(s))
	if len(got) != 1 || got[0].Type != core.EventResult || got[0].Content != "prefaceclean" {
		t.Fatalf("events = %+v, want completed pre-retry text plus clean retry", got)
	}
}

func TestPiRPCRetryExhaustionIsNonTransportAndNextTurnWorks(t *testing.T) {
	s := newTestRPCSession()
	defer s.Close()

	s.handleAgentEvent(map[string]any{"type": "agent_start"})
	s.handleAgentEvent(map[string]any{"type": "message_end", "message": rpcAssistant("", "overloaded after retries")})
	s.handleAgentEvent(map[string]any{"type": "agent_end", "willRetry": false, "messages": []any{rpcAssistant("", "overloaded after retries")}})
	s.handleAgentEvent(map[string]any{"type": "auto_retry_end", "success": false, "attempt": float64(3), "finalError": "overloaded after retries"})
	s.handleAgentEvent(map[string]any{"type": "agent_settled"})

	got := terminalRPCEvents(drainRPCEvents(s))
	if len(got) != 1 || got[0].Type != core.EventError || got[0].Error == nil {
		t.Fatalf("settled events = %+v, want one provider error", got)
	}
	if s.EventErrorIsTerminal(got[0].Error) {
		t.Fatalf("provider error classified as dead transport: %v", got[0].Error)
	}
	if !s.Alive() {
		t.Fatal("retry exhaustion killed persistent Pi session")
	}

	s.handleAgentEvent(map[string]any{"type": "agent_start"})
	s.handleAgentEvent(map[string]any{"type": "message_end", "message": rpcAssistant("next turn ok", "")})
	s.handleAgentEvent(map[string]any{"type": "agent_end", "messages": []any{rpcAssistant("next turn ok", "")}})
	s.handleAgentEvent(map[string]any{"type": "agent_settled"})
	got = terminalRPCEvents(drainRPCEvents(s))
	if len(got) != 1 || got[0].Type != core.EventResult || got[0].Content != "next turn ok" {
		t.Fatalf("next turn events = %+v", got)
	}
}

func TestPiRPCStopReasonErrorWithoutMessageSettlesAsError(t *testing.T) {
	s := newTestRPCSession()
	defer s.Close()

	msg := map[string]any{"role": "assistant", "stopReason": "error"}
	s.handleAgentEvent(map[string]any{"type": "agent_start"})
	s.handleAgentEvent(map[string]any{"type": "message_end", "message": msg})
	s.handleAgentEvent(map[string]any{"type": "agent_end", "messages": []any{msg}})
	s.handleAgentEvent(map[string]any{"type": "agent_settled"})

	got := terminalRPCEvents(drainRPCEvents(s))
	if len(got) != 1 || got[0].Type != core.EventError || got[0].Error == nil {
		t.Fatalf("events = %+v, want settled provider error", got)
	}
}

func TestPiRPCFailedPartialIsNotConcatenatedWithRetry(t *testing.T) {
	s := newTestRPCSession()
	defer s.Close()

	s.handleAgentEvent(map[string]any{"type": "agent_start"})
	s.handleAgentEvent(map[string]any{"type": "message_update", "assistantMessageEvent": map[string]any{"type": "text_delta", "delta": "DO NOT LEAK"}})
	s.handleAgentEvent(map[string]any{"type": "message_end", "message": rpcAssistant("", "retry me")})
	s.handleAgentEvent(map[string]any{"type": "agent_end", "willRetry": true, "messages": []any{rpcAssistant("", "retry me")}})
	s.handleAgentEvent(map[string]any{"type": "auto_retry_start"})
	s.handleAgentEvent(map[string]any{"type": "agent_start"})
	s.handleAgentEvent(map[string]any{"type": "message_end", "message": rpcAssistant("clean", "")})
	s.handleAgentEvent(map[string]any{"type": "agent_end", "messages": []any{rpcAssistant("clean", "")}})
	s.handleAgentEvent(map[string]any{"type": "agent_settled"})

	events := drainRPCEvents(s)
	for _, event := range events {
		if event.Type == core.EventText {
			t.Fatalf("failed partial leaked as EventText: %+v", events)
		}
	}
	got := terminalRPCEvents(events)
	if len(got) != 1 || got[0].Content != "clean" {
		t.Fatalf("events = %+v, failed partial was retained", events)
	}
}

func TestPiRPCCompactionAndQueuedContinuationSettleOnce(t *testing.T) {
	s := newTestRPCSession()
	defer s.Close()

	s.handleAgentEvent(map[string]any{"type": "agent_start"})
	s.handleAgentEvent(map[string]any{"type": "message_end", "message": rpcAssistant("first", "")})
	s.handleAgentEvent(map[string]any{"type": "agent_end", "messages": []any{rpcAssistant("first", "")}})
	s.handleAgentEvent(map[string]any{"type": "compaction_start", "reason": "overflow"})
	s.handleAgentEvent(map[string]any{"type": "compaction_end", "willRetry": true})
	s.handleAgentEvent(map[string]any{"type": "queue_update", "followUp": []any{"continue"}})
	s.handleAgentEvent(map[string]any{"type": "agent_start"})
	s.handleAgentEvent(map[string]any{"type": "message_end", "message": rpcAssistant("second", "")})
	s.handleAgentEvent(map[string]any{"type": "agent_end", "messages": []any{rpcAssistant("second", "")}})
	if got := terminalRPCEvents(drainRPCEvents(s)); len(got) != 0 {
		t.Fatalf("pre-settlement terminal events: %+v", got)
	}
	s.handleAgentEvent(map[string]any{"type": "agent_settled"})
	s.handleAgentEvent(map[string]any{"type": "agent_settled"})

	got := terminalRPCEvents(drainRPCEvents(s))
	if len(got) != 1 || got[0].Type != core.EventResult || got[0].Content != "firstsecond" {
		t.Fatalf("settled events = %+v", got)
	}
}

func TestPiRPCThresholdCompactionFailurePreservesValidAnswer(t *testing.T) {
	s := newTestRPCSession()
	defer s.Close()

	s.handleAgentEvent(map[string]any{"type": "agent_start"})
	s.handleAgentEvent(map[string]any{"type": "agent_end", "messages": []any{rpcAssistant("valid answer", "")}})
	s.handleAgentEvent(map[string]any{"type": "compaction_start", "reason": "threshold"})
	s.handleAgentEvent(map[string]any{"type": "compaction_end", "reason": "threshold", "errorMessage": "summary provider failed"})
	s.handleAgentEvent(map[string]any{"type": "agent_settled"})

	got := terminalRPCEvents(drainRPCEvents(s))
	if len(got) != 1 || got[0].Type != core.EventResult || got[0].Content != "valid answer" {
		t.Fatalf("events = %+v, compaction failure replaced valid answer", got)
	}
}

func TestPiRPCSaturatedProgressCannotLoseSettledEvent(t *testing.T) {
	s := newTestRPCSession()
	defer s.Close()

	s.handleAgentEvent(map[string]any{"type": "agent_start"})
	s.handleAgentEvent(map[string]any{"type": "message_end", "message": rpcAssistant("done", "")})
	s.handleAgentEvent(map[string]any{"type": "agent_end", "messages": []any{rpcAssistant("done", "")}})
	for range cap(s.events) * 2 {
		s.tryEmit(core.Event{Type: core.EventThinking, Content: "progress"})
	}
	s.handleAgentEvent(map[string]any{"type": "agent_settled"})

	got := terminalRPCEvents(drainRPCEvents(s))
	if len(got) != 1 || got[0].Type != core.EventResult {
		t.Fatalf("terminal event lost under saturation: %+v", got)
	}
}

func TestPiRPCActualProcessFailureIsTerminal(t *testing.T) {
	tmp := t.TempDir()
	script := filepath.Join(tmp, "fake-rpc-crash.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'rpc crashed' >&2\nexit 9\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := newPiRPCSession(context.Background(), script, tmp, "", "default", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.startProcess(); err != nil {
		t.Fatal(err)
	}

	select {
	case event := <-s.Events():
		if event.Type != core.EventError || event.Error == nil || !s.EventErrorIsTerminal(event.Error) {
			t.Fatalf("process failure event = %+v, want terminal transport error", event)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for process failure")
	}
	if s.Alive() {
		t.Fatal("process failure left session alive")
	}
	if (&piRPCSession{}).EventErrorIsTerminal(errors.New("provider failed")) {
		t.Fatal("untyped/provider error classified terminal")
	}
}

func TestPiRPCExtensionErrorDuringRunDoesNotDuplicateSettlement(t *testing.T) {
	s := newTestRPCSession()
	defer s.Close()

	s.handleAgentEvent(map[string]any{"type": "agent_start"})
	s.handleAgentEvent(map[string]any{"type": "extension_error", "extensionPath": "/tmp/ext.ts", "error": "hook failed"})
	s.handleAgentEvent(map[string]any{"type": "agent_end", "messages": []any{rpcAssistant("answer", "")}})
	s.handleAgentEvent(map[string]any{"type": "agent_settled"})

	got := terminalRPCEvents(drainRPCEvents(s))
	if len(got) != 1 || got[0].Type != core.EventResult || got[0].Content != "answer" {
		t.Fatalf("events = %+v, want one settled answer", got)
	}
}

func TestPiRPCCustomNotificationDefersAcrossPromptStartRace(t *testing.T) {
	s := newTestRPCSession()
	defer s.Close()

	s.turnMu.Lock()
	s.promptPending = true
	s.turnMu.Unlock()
	s.handleAgentEvent(map[string]any{"type": "custom_message", "customType": "subagent-notify", "display": true, "content": "early child"})
	if got := drainRPCEvents(s); len(got) != 0 {
		t.Fatalf("custom notification prematurely settled pending prompt: %+v", got)
	}
	s.turnMu.Lock()
	s.promptPending = false
	s.turnMu.Unlock()
	s.handleAgentEvent(map[string]any{"type": "agent_start"})
	s.handleAgentEvent(map[string]any{"type": "agent_end", "messages": []any{rpcAssistant("answer", "")}})
	s.handleAgentEvent(map[string]any{"type": "agent_settled"})

	got := terminalRPCEvents(drainRPCEvents(s))
	if len(got) != 2 || got[0].Content != "answer" || got[1].Content != "early child" {
		t.Fatalf("events = %+v, want answer then deferred early notification", got)
	}
}

func TestPiRPCCustomNotificationDefersBehindActiveSettlement(t *testing.T) {
	s := newTestRPCSession()
	defer s.Close()

	s.handleAgentEvent(map[string]any{"type": "agent_start"})
	s.handleAgentEvent(map[string]any{"type": "custom_message", "customType": "subagent-notify", "display": true, "content": "child done"})
	s.handleAgentEvent(map[string]any{"type": "agent_end", "messages": []any{rpcAssistant("answer", "")}})
	s.handleAgentEvent(map[string]any{"type": "agent_settled"})

	got := terminalRPCEvents(drainRPCEvents(s))
	if len(got) != 2 || got[0].Type != core.EventResult || got[0].Content != "answer" || got[1].Content != "child done" {
		t.Fatalf("events = %+v, want answer settlement then child notification", got)
	}
}

func TestPiRPCFailedSettlementIncludesDeferredCustomNotification(t *testing.T) {
	s := newTestRPCSession()
	defer s.Close()

	s.handleAgentEvent(map[string]any{"type": "agent_start"})
	s.handleAgentEvent(map[string]any{"type": "custom_message", "customType": "subagent-notify", "display": true, "content": "child survived"})
	s.handleAgentEvent(map[string]any{"type": "agent_end", "messages": []any{rpcAssistant("", "provider failed")}})
	s.handleAgentEvent(map[string]any{"type": "agent_settled"})

	got := terminalRPCEvents(drainRPCEvents(s))
	if len(got) != 1 || got[0].Type != core.EventError || got[0].Error == nil || !strings.Contains(got[0].Error.Error(), "child survived") {
		t.Fatalf("events = %+v, deferred notification lost after failed settlement", got)
	}
}

func TestPiRPCExtensionACKWithoutAgentRunCompletes(t *testing.T) {
	tmp := t.TempDir()
	fake, _, _ := writeFakePiRPC(t, tmp)
	s, err := newPiRPCSession(context.Background(), fake, tmp, "", "default", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Send("/extension-no-agent-run", nil, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-s.Events():
		if event.Type != core.EventResult || !event.Done {
			t.Fatalf("extension ACK event = %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("extension ACK hung without agent_settled")
	}
	if !s.Alive() {
		t.Fatal("extension ACK closed persistent session")
	}
}

func TestPiRPCExtensionCompletionStateFailureIsTerminal(t *testing.T) {
	tmp := t.TempDir()
	script := filepath.Join(tmp, "fake-rpc-state-failure.sh")
	body := `#!/bin/sh
while IFS= read -r line; do
  id=$(printf '%s' "$line" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
  typ=$(printf '%s' "$line" | python3 -c 'import json,sys; print(json.load(sys.stdin)["type"])')
  if [ "$typ" = "get_state" ]; then
    printf '{"id":"%s","type":"response","command":"get_state","success":false,"error":"state unavailable"}\n' "$id"
  elif [ "$typ" = "prompt" ]; then
    printf '{"id":"%s","type":"response","command":"prompt","success":true}\n' "$id"
  fi
done
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := newPiRPCSession(context.Background(), script, tmp, "", "default", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Send("/extension-without-state", nil, nil); err != nil {
		t.Fatal(err)
	}

	got := terminalRPCEvents(drainRPCEvents(s))
	if len(got) != 1 || got[0].Type != core.EventError || !s.EventErrorIsTerminal(got[0].Error) {
		t.Fatalf("events = %+v, want terminal protocol-state error", got)
	}
}

func TestPiRPCExtensionFailureACKEmitsOneError(t *testing.T) {
	tmp := t.TempDir()
	script := filepath.Join(tmp, "fake-rpc-extension-error.sh")
	body := `#!/bin/sh
while IFS= read -r line; do
  id=$(printf '%s' "$line" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
  typ=$(printf '%s' "$line" | python3 -c 'import json,sys; print(json.load(sys.stdin)["type"])')
  if [ "$typ" = "get_state" ]; then
    printf '{"id":"%s","type":"response","command":"get_state","success":true,"data":{"isStreaming":false}}\n' "$id"
  elif [ "$typ" = "prompt" ]; then
    printf '{"type":"extension_error","extensionPath":"/tmp/fail.ts","error":"command failed"}\n'
    printf '{"id":"%s","type":"response","command":"prompt","success":true}\n' "$id"
  fi
done
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := newPiRPCSession(context.Background(), script, tmp, "", "default", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Send("/failing-extension", nil, nil); err != nil {
		t.Fatal(err)
	}

	got := terminalRPCEvents(drainRPCEvents(s))
	if len(got) != 1 || got[0].Type != core.EventError || got[0].Error == nil {
		t.Fatalf("events = %+v, want exactly one extension error", got)
	}
	if s.EventErrorIsTerminal(got[0].Error) || !s.Alive() {
		t.Fatalf("extension failure damaged live transport: event=%+v alive=%v", got[0], s.Alive())
	}
}
