package pi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tasercake/pi-connect/core"
)

type rpcBlockingWriteCloser struct {
	started      chan struct{}
	closed       chan struct{}
	startedOnce  sync.Once
	closeOnce    sync.Once
	bytesOnClose int
}

func newRPCBlockingWriteCloser() *rpcBlockingWriteCloser {
	return &rpcBlockingWriteCloser{started: make(chan struct{}), closed: make(chan struct{})}
}

func (w *rpcBlockingWriteCloser) Write(p []byte) (int, error) {
	w.startedOnce.Do(func() { close(w.started) })
	<-w.closed
	n := w.bytesOnClose
	if n > len(p) {
		n = len(p)
	}
	return n, io.ErrClosedPipe
}

func (w *rpcBlockingWriteCloser) Close() error {
	w.closeOnce.Do(func() { close(w.closed) })
	return nil
}

type rpcRecordingWriteCloser struct {
	mu       sync.Mutex
	writes   [][]byte
	writeCh  chan map[string]any
	failErr  error
	failPart int
}

func newRPCRecordingWriteCloser() *rpcRecordingWriteCloser {
	return &rpcRecordingWriteCloser{writeCh: make(chan map[string]any, 32)}
}

func (w *rpcRecordingWriteCloser) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failErr != nil {
		n := w.failPart
		if n > len(p) {
			n = len(p)
		}
		return n, w.failErr
	}
	copyBytes := append([]byte(nil), p...)
	w.writes = append(w.writes, copyBytes)
	var command map[string]any
	if err := json.Unmarshal(copyBytes, &command); err == nil {
		w.writeCh <- command
	}
	return len(p), nil
}

func (w *rpcRecordingWriteCloser) Close() error { return nil }

func (w *rpcRecordingWriteCloser) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.writes)
}

func newAcceptanceRPCSession(t *testing.T, writer io.WriteCloser) *piRPCSession {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := &piRPCSession{
		ctx:                 ctx,
		cancel:              cancel,
		stdin:               writer,
		pending:             make(map[string]*rpcPending),
		writeGate:           make(chan struct{}, 1),
		mutatingGate:        make(chan struct{}, 1),
		slowAcceptanceAfter: time.Hour,
	}
	s.writeGate <- struct{}{}
	s.mutatingGate <- struct{}{}
	s.alive.Store(true)
	t.Cleanup(cancel)
	return s
}

func rpcCommandID(t *testing.T, command map[string]any) string {
	t.Helper()
	id, ok := command["id"].(string)
	if !ok || id == "" {
		t.Fatalf("command id = %#v", command["id"])
	}
	return id
}

func respondRPC(s *piRPCSession, id, command string, success bool, errMsg string) {
	line, _ := json.Marshal(map[string]any{
		"id": id, "type": "response", "command": command,
		"success": success, "error": errMsg,
	})
	s.handleResponse(line)
}

func pendingRPCCount(s *piRPCSession) int {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	return len(s.pending)
}

func runSerializedMutation(s *piRPCSession, ctx context.Context, typ string) (rpcResponse, error) {
	if err := s.acquireMutating(ctx, typ); err != nil {
		return rpcResponse{}, err
	}
	defer s.releaseMutating()
	return s.callMutating(ctx, map[string]any{"type": typ})
}

func TestPiRPCStartupStateProbeHonorsSendCallerContext(t *testing.T) {
	tmp := t.TempDir()
	commands := filepath.Join(tmp, "commands.jsonl")
	script := filepath.Join(tmp, "fake-rpc-no-state-ack.sh")
	body := "#!/bin/sh\nwhile IFS= read -r line; do printf '%s\\n' \"$line\" >> \"" + commands + "\"; done\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := newPiRPCSession(context.Background(), script, tmp, "", "default", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	started := time.Now()
	err = s.SendContext(ctx, "must not be written", nil, nil)
	var notSent *RPCCommandNotSentError
	if !errors.As(err, &notSent) {
		t.Fatalf("SendContext() error = %T %v, want not sent", err, err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("startup state probe ignored caller deadline: %v", elapsed)
	}
	var data []byte
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		data, _ = os.ReadFile(commands)
		if len(bytes.TrimSpace(data)) > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		t.Fatal("startup get_state was not recorded")
	}
	var command map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(data), &command); err != nil {
		t.Fatal(err)
	}
	if command["type"] != "get_state" {
		t.Fatalf("commands = %s, want only startup get_state", data)
	}
}

func TestPiRPCPromptDelayedBeyondLegacyThresholdStaysPending(t *testing.T) {
	writer := newRPCRecordingWriteCloser()
	s := newAcceptanceRPCSession(t, writer)
	s.slowAcceptanceAfter = 10 * time.Millisecond
	warned := make(chan struct{}, 1)
	s.slowAcceptanceLog = func(command string, _ time.Duration) {
		if command != "prompt" {
			t.Errorf("slow command = %q", command)
		}
		warned <- struct{}{}
	}

	done := make(chan error, 1)
	go func() {
		_, err := runSerializedMutation(s, context.Background(), "prompt")
		done <- err
	}()
	command := <-writer.writeCh
	id := rpcCommandID(t, command)

	select {
	case <-warned:
	case <-time.After(time.Second):
		t.Fatal("slow acceptance warning not emitted")
	}
	select {
	case err := <-done:
		t.Fatalf("slow warning completed prompt: %v", err)
	default:
	}
	if got := pendingRPCCount(s); got != 1 {
		t.Fatalf("pending after slow warning = %d, want 1", got)
	}
	respondRPC(s, id, "prompt", true, "")
	if err := <-done; err != nil {
		t.Fatalf("late prompt ACK: %v", err)
	}
	if writer.count() != 1 || pendingRPCCount(s) != 0 {
		t.Fatalf("writes=%d pending=%d, want one write and clean map", writer.count(), pendingRPCCount(s))
	}
}

func TestPiRPCCompactDelayedACKStaysPendingAndSucceeds(t *testing.T) {
	writer := newRPCRecordingWriteCloser()
	s := newAcceptanceRPCSession(t, writer)
	s.slowAcceptanceAfter = 5 * time.Millisecond

	done := make(chan error, 1)
	go func() {
		_, err := runSerializedMutation(s, context.Background(), "compact")
		done <- err
	}()
	command := <-writer.writeCh
	id := rpcCommandID(t, command)
	time.Sleep(20 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("compact completed before ACK: %v", err)
	default:
	}
	if pendingRPCCount(s) != 1 {
		t.Fatalf("compact pending = %d, want 1", pendingRPCCount(s))
	}
	respondRPC(s, id, "compact", true, "")
	if err := <-done; err != nil {
		t.Fatalf("late compact ACK: %v", err)
	}
	if writer.count() != 1 {
		t.Fatalf("compact writes = %d, want 1", writer.count())
	}
}

func TestPiRPCConcurrentMutatingAcceptanceIsSerialized(t *testing.T) {
	writer := newRPCRecordingWriteCloser()
	s := newAcceptanceRPCSession(t, writer)
	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)
	go func() {
		_, err := runSerializedMutation(s, context.Background(), "prompt")
		firstDone <- err
	}()
	first := <-writer.writeCh
	go func() {
		_, err := runSerializedMutation(s, context.Background(), "compact")
		secondDone <- err
	}()

	select {
	case command := <-writer.writeCh:
		t.Fatalf("second mutation wrote before first ACK: %#v", command)
	case <-time.After(20 * time.Millisecond):
	}
	respondRPC(s, rpcCommandID(t, first), "prompt", true, "")
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	second := <-writer.writeCh
	if second["type"] != "compact" {
		t.Fatalf("second command type = %v", second["type"])
	}
	respondRPC(s, rpcCommandID(t, second), "compact", true, "")
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
}

func TestPiRPCCancelledQueuedMutationIsNotSent(t *testing.T) {
	writer := newRPCRecordingWriteCloser()
	s := newAcceptanceRPCSession(t, writer)
	firstDone := make(chan error, 1)
	go func() {
		_, err := runSerializedMutation(s, context.Background(), "prompt")
		firstDone <- err
	}()
	first := <-writer.writeCh

	ctx, cancel := context.WithCancel(context.Background())
	secondDone := make(chan error, 1)
	go func() {
		_, err := runSerializedMutation(s, ctx, "compact")
		secondDone <- err
	}()
	cancel()
	err := <-secondDone
	var notSent *RPCCommandNotSentError
	if !errors.As(err, &notSent) || notSent.Command != "compact" {
		t.Fatalf("queued cancellation = %T %v, want compact not sent", err, err)
	}
	if writer.count() != 1 {
		t.Fatalf("writes after queued cancellation = %d, want 1", writer.count())
	}
	respondRPC(s, rpcCommandID(t, first), "prompt", true, "")
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}

func TestPiRPCDefiniteRejectionReleasesMutationQueue(t *testing.T) {
	writer := newRPCRecordingWriteCloser()
	s := newAcceptanceRPCSession(t, writer)
	firstDone := make(chan rpcCallResult, 1)
	go func() {
		resp, err := runSerializedMutation(s, context.Background(), "prompt")
		firstDone <- rpcCallResult{response: resp, err: err}
	}()
	first := <-writer.writeCh
	respondRPC(s, rpcCommandID(t, first), "prompt", false, "rejected")
	result := <-firstDone
	if result.err != nil || result.response.success || result.response.errMsg != "rejected" {
		t.Fatalf("rejection result = %+v", result)
	}

	secondDone := make(chan error, 1)
	go func() {
		_, err := runSerializedMutation(s, context.Background(), "compact")
		secondDone <- err
	}()
	second := <-writer.writeCh
	respondRPC(s, rpcCommandID(t, second), "compact", true, "")
	if err := <-secondDone; err != nil {
		t.Fatalf("next call after rejection: %v", err)
	}
}

func TestPiRPCCancellationBeforeWriteIsNotSent(t *testing.T) {
	writer := newRPCRecordingWriteCloser()
	s := newAcceptanceRPCSession(t, writer)
	// Hold the cancellable write gate after mutation admission.
	<-s.writeGate
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := runSerializedMutation(s, ctx, "prompt")
		done <- err
	}()
	for pendingRPCCount(s) == 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	err := <-done
	s.writeGate <- struct{}{}
	var notSent *RPCCommandNotSentError
	if !errors.As(err, &notSent) || writer.count() != 0 {
		t.Fatalf("pre-write cancellation error=%T %v writes=%d", err, err, writer.count())
	}
	if !s.Alive() || pendingRPCCount(s) != 0 {
		t.Fatalf("pre-write cancellation damaged session: alive=%v pending=%d", s.Alive(), pendingRPCCount(s))
	}
}

func TestPiRPCBlockedWriteCancellationUnblocksAndClosesTransport(t *testing.T) {
	writer := newRPCBlockingWriteCloser()
	s := newAcceptanceRPCSession(t, writer)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := runSerializedMutation(s, ctx, "prompt")
		done <- err
	}()
	<-writer.started
	cancel()
	select {
	case err := <-done:
		var notSent *RPCCommandNotSentError
		if !errors.As(err, &notSent) {
			t.Fatalf("blocked zero-byte write error = %T %v, want not sent", err, err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked write did not unblock on cancellation")
	}
	if s.Alive() || pendingRPCCount(s) != 0 {
		t.Fatalf("cancelled blocked transport alive=%v pending=%d", s.Alive(), pendingRPCCount(s))
	}
}

func TestPiRPCMutatingCancellationAfterWriteIsOutcomeUnknown(t *testing.T) {
	writer := newRPCRecordingWriteCloser()
	s := newAcceptanceRPCSession(t, writer)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := runSerializedMutation(s, ctx, "prompt")
		done <- err
	}()
	command := <-writer.writeCh
	cancel()
	err := <-done
	var unknown *RPCAcceptanceUnknownError
	if !errors.As(err, &unknown) || unknown.Command != "prompt" {
		t.Fatalf("cancel error = %T %v, want prompt outcome unknown", err, err)
	}
	if pendingRPCCount(s) != 0 || writer.count() != 1 || s.Alive() {
		t.Fatalf("after cancel writes=%d pending=%d alive=%v", writer.count(), pendingRPCCount(s), s.Alive())
	}
	// Late ACK is unmatched and cannot complete a future request.
	respondRPC(s, rpcCommandID(t, command), "prompt", true, "")
}

func TestPiRPCAcknowledgedMutationWinsLateWriterError(t *testing.T) {
	writer := newRPCBlockingWriteCloser()
	writer.bytesOnClose = 1
	s := newAcceptanceRPCSession(t, writer)
	s.events = make(chan core.Event, 2)
	done := make(chan rpcCallResult, 1)
	go func() {
		resp, err := runSerializedMutation(s, context.Background(), "prompt")
		done <- rpcCallResult{response: resp, err: err}
	}()
	<-writer.started
	respondRPC(s, "1", "prompt", true, "")
	_ = writer.Close()
	result := <-done
	if result.err != nil || !result.response.success {
		t.Fatalf("ACK-late-write result = %+v", result)
	}
	event := <-s.events
	var unknown *RPCAcceptanceUnknownError
	if errors.As(event.Error, &unknown) || !s.EventErrorIsTerminal(event.Error) {
		t.Fatalf("late writer transport event = %T %v", event.Error, event.Error)
	}
}

func TestPiRPCAcknowledgedMutationThenTransportExitIsNotUnknown(t *testing.T) {
	writer := newRPCRecordingWriteCloser()
	s := newAcceptanceRPCSession(t, writer)
	s.events = make(chan core.Event, 2)
	pending := &rpcPending{ch: make(chan rpcCallResult, 1), mutating: true}
	s.pendingMu.Lock()
	s.pending["claimed"] = pending
	s.pendingMu.Unlock()

	respondRPC(s, "claimed", "prompt", true, "")
	s.emitTransportFailure(errors.New("exit after ACK"))
	result := <-pending.ch
	if result.err != nil || !result.response.success {
		t.Fatalf("claimed ACK result = %+v", result)
	}
	event := <-s.events
	var unknown *RPCAcceptanceUnknownError
	if errors.As(event.Error, &unknown) || !s.EventErrorIsTerminal(event.Error) {
		t.Fatalf("post-ACK transport event = %T %v", event.Error, event.Error)
	}
}

func TestPiRPCTransportFailureDuringWriteHasOneAuthoritativeOutcome(t *testing.T) {
	for _, tc := range []struct {
		name    string
		written int
		unknown bool
	}{
		{name: "zero bytes is not sent", written: 0},
		{name: "partial bytes is unknown", written: 1, unknown: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writer := newRPCBlockingWriteCloser()
			writer.bytesOnClose = tc.written
			s := newAcceptanceRPCSession(t, writer)
			s.events = make(chan core.Event, 2)
			done := make(chan error, 1)
			go func() {
				_, err := runSerializedMutation(s, context.Background(), "prompt")
				done <- err
			}()
			<-writer.started
			s.emitTransportFailure(errors.New("stdout failed during write"))
			err := <-done
			var unknown *RPCAcceptanceUnknownError
			var notSent *RPCCommandNotSentError
			if tc.unknown && !errors.As(err, &unknown) {
				t.Fatalf("partial write error = %T %v, want unknown", err, err)
			}
			if !tc.unknown && !errors.As(err, &notSent) {
				t.Fatalf("zero write error = %T %v, want not sent", err, err)
			}
			select {
			case event := <-s.events:
				t.Fatalf("competing transport event = %+v", event)
			default:
			}
		})
	}
}

func TestPiRPCTransportFailureRejectsQueuedAdmission(t *testing.T) {
	writer := newRPCRecordingWriteCloser()
	s := newAcceptanceRPCSession(t, writer)
	s.emitTransportFailure(errors.New("stdout failed"))
	_, err := runSerializedMutation(s, context.Background(), "prompt")
	var notSent *RPCCommandNotSentError
	if !errors.As(err, &notSent) {
		t.Fatalf("post-failure admission = %T %v, want not sent", err, err)
	}
	if writer.count() != 0 || pendingRPCCount(s) != 0 {
		t.Fatalf("post-failure writes=%d pending=%d", writer.count(), pendingRPCCount(s))
	}
}

func TestPiRPCProcessExitAfterWriteIsOutcomeUnknown(t *testing.T) {
	writer := newRPCRecordingWriteCloser()
	s := newAcceptanceRPCSession(t, writer)
	s.events = make(chan core.Event, 2)
	done := make(chan error, 1)
	go func() {
		_, err := runSerializedMutation(s, context.Background(), "compact")
		done <- err
	}()
	<-writer.writeCh
	s.emitTransportFailure(errors.New("process exited"))
	err := <-done
	var unknown *RPCAcceptanceUnknownError
	if !errors.As(err, &unknown) || unknown.Command != "compact" {
		t.Fatalf("exit error = %T %v, want compact outcome unknown", err, err)
	}
	if pendingRPCCount(s) != 0 {
		t.Fatalf("pending after exit = %d", pendingRPCCount(s))
	}
	select {
	case event := <-s.events:
		t.Fatalf("transport event raced mutating return: %+v", event)
	default:
	}
	if s.Alive() {
		t.Fatal("transport failure left session alive")
	}
}

func TestPiRPCPreWriteFailureIsTypedNotSent(t *testing.T) {
	s := newAcceptanceRPCSession(t, nil)
	_, err := runSerializedMutation(s, context.Background(), "prompt")
	var notSent *RPCCommandNotSentError
	if !errors.As(err, &notSent) || notSent.Command != "prompt" {
		t.Fatalf("pre-write error = %T %v, want prompt not sent", err, err)
	}
	var unknown *RPCAcceptanceUnknownError
	if errors.As(err, &unknown) {
		t.Fatalf("pre-write failure marked unknown: %v", err)
	}
	if pendingRPCCount(s) != 0 {
		t.Fatalf("pending after pre-write failure = %d", pendingRPCCount(s))
	}
}

func TestPiRPCPartialWriteIsOutcomeUnknown(t *testing.T) {
	writer := newRPCRecordingWriteCloser()
	writer.failErr = errors.New("broken pipe")
	writer.failPart = 1
	s := newAcceptanceRPCSession(t, writer)
	_, err := runSerializedMutation(s, context.Background(), "prompt")
	var unknown *RPCAcceptanceUnknownError
	if !errors.As(err, &unknown) {
		t.Fatalf("partial write error = %T %v, want outcome unknown", err, err)
	}
}

func TestPiRPCGetStateRemainsBoundedAndLateResponseCannotCorruptNextID(t *testing.T) {
	writer := newRPCRecordingWriteCloser()
	s := newAcceptanceRPCSession(t, writer)
	firstDone := make(chan error, 1)
	go func() {
		_, err := s.callRPC(context.Background(), map[string]any{"type": "get_state"}, 10*time.Millisecond, false)
		firstDone <- err
	}()
	first := <-writer.writeCh
	if err := <-firstDone; err == nil {
		t.Fatal("get_state unexpectedly had no timeout")
	} else {
		var timeoutErr *rpcCallTimeoutError
		if !errors.As(err, &timeoutErr) {
			t.Fatalf("get_state error = %T %v, want bounded timeout", err, err)
		}
	}
	respondRPC(s, rpcCommandID(t, first), "get_state", true, "")

	secondDone := make(chan error, 1)
	go func() {
		resp, err := s.callRPC(context.Background(), map[string]any{"type": "get_state"}, time.Second, false)
		if err == nil && !resp.success {
			err = errors.New("second get_state rejected")
		}
		secondDone <- err
	}()
	second := <-writer.writeCh
	if rpcCommandID(t, second) == rpcCommandID(t, first) {
		t.Fatal("request ID was reused")
	}
	respondRPC(s, rpcCommandID(t, second), "get_state", true, "")
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
}

func TestPiRPCCloseWakesMutatingACKWaiter(t *testing.T) {
	writer := newRPCRecordingWriteCloser()
	s := newAcceptanceRPCSession(t, writer)
	done := make(chan error, 1)
	go func() {
		_, err := runSerializedMutation(s, context.Background(), "prompt")
		done <- err
	}()
	command := <-writer.writeCh
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	err := <-done
	var unknown *RPCAcceptanceUnknownError
	if !errors.As(err, &unknown) {
		t.Fatalf("Close error = %T %v, want outcome unknown", err, err)
	}
	if pendingRPCCount(s) != 0 {
		t.Fatalf("pending after Close = %d", pendingRPCCount(s))
	}
	respondRPC(s, rpcCommandID(t, command), "prompt", true, "")
}

func TestPiRPCCloseCancelLateACKRacesCleanPending(t *testing.T) {
	for i := 0; i < 100; i++ {
		writer := newRPCRecordingWriteCloser()
		s := newAcceptanceRPCSession(t, writer)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := runSerializedMutation(s, ctx, "prompt")
			done <- err
		}()
		command := <-writer.writeCh
		id := rpcCommandID(t, command)
		go cancel()
		go s.cancelPending(&rpcTransportError{cause: errors.New("closed")})
		go respondRPC(s, id, "prompt", true, "")
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("iteration %d leaked waiter", i)
		}
		if pendingRPCCount(s) != 0 {
			t.Fatalf("iteration %d pending=%d", i, pendingRPCCount(s))
		}
		select {
		case <-s.mutatingGate:
			s.releaseMutating()
		default:
			t.Fatalf("iteration %d mutation gate not released", i)
		}
	}
}

func TestPiRPCSlowACKRetryLifecycleSettlesExactlyOnce(t *testing.T) {
	writer := newRPCRecordingWriteCloser()
	s := newAcceptanceRPCSession(t, writer)
	s.events = make(chan core.Event, 64)
	s.slowAcceptanceAfter = 5 * time.Millisecond
	accepted := make(chan error, 1)
	go func() {
		_, err := runSerializedMutation(s, context.Background(), "prompt")
		accepted <- err
	}()
	command := <-writer.writeCh
	time.Sleep(15 * time.Millisecond)
	respondRPC(s, rpcCommandID(t, command), "prompt", true, "")
	if err := <-accepted; err != nil {
		t.Fatal(err)
	}

	s.handleAgentEvent(map[string]any{"type": "agent_start"})
	s.handleAgentEvent(map[string]any{"type": "agent_end", "willRetry": true, "messages": []any{rpcAssistant("", "retry")}})
	s.handleAgentEvent(map[string]any{"type": "auto_retry_start"})
	s.handleAgentEvent(map[string]any{"type": "agent_start"})
	s.handleAgentEvent(map[string]any{"type": "agent_end", "messages": []any{rpcAssistant("done", "")}})
	s.handleAgentEvent(map[string]any{"type": "agent_settled"})

	terminal := terminalRPCEvents(drainRPCEvents(s))
	if len(terminal) != 1 || terminal[0].Content != "done" {
		t.Fatalf("terminal events = %+v", terminal)
	}
	if !s.Alive() {
		t.Fatal("slow ACK/retry lifecycle killed session")
	}
}
