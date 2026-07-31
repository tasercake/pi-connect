package core

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOutcomeUnknownPersistsInFlightLeaseWithoutPromptContent(t *testing.T) {
	dataDir := t.TempDir()
	store, err := NewRuntimeLifecycleStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("project", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.SetRuntimeLifecycleStore(store)
	defer e.Stop()

	key := "test:chat:user"
	session := e.sessions.GetOrCreateActive(key)
	sess := newTerminalEventErrorSession("saved-session", false)
	state := &interactiveState{
		agentSession: sess, platform: p, replyCtx: "ctx", agent: &stubAgent{},
		runtimeLeaseID: store.NewLeaseID("project", key), deliverySessionKey: key, leaseStartedAt: time.Now().UTC(),
	}
	e.interactiveMu.Lock()
	e.interactiveStates[key] = state
	e.interactiveMu.Unlock()
	e.persistRuntimeLease(state)

	sendDone := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		e.processInteractiveEvents(state, session, e.sessions, key, "operation-1", time.Now(), nil, sendDone, "ctx")
		close(done)
	}()
	sendDone <- fmt.Errorf("rpc send: %w", testOutcomeUnknownError{})
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("event loop did not return after unknown acceptance")
	}

	store.mu.Lock()
	lease, ok := store.state.Leases[state.runtimeLeaseID]
	store.mu.Unlock()
	if !ok || !lease.TurnInFlight || !lease.OutcomeUnknown || lease.OperationID != "operation-1" {
		t.Fatalf("persisted unknown lease = %+v, exists=%v", lease, ok)
	}
	encoded := fmt.Sprintf("%+v", lease)
	if strings.Contains(encoded, "raw prompt acceptance") || strings.Contains(encoded, "secret") {
		t.Fatalf("lease persisted error/prompt content: %s", encoded)
	}
}

func TestPromptAcceptancePersistsLearnedSessionIDBeforeResult(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "sessions.json")
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("project", &stubAgent{}, []Platform{p}, storePath, LangEnglish)
	defer e.Stop()

	key := "test:chat:user"
	session := e.sessions.GetOrCreateActive(key)
	sess := newTerminalEventErrorSession("learned-before-result", false)
	state := &interactiveState{agentSession: sess, platform: p, replyCtx: "ctx", agent: &stubAgent{}}
	e.interactiveMu.Lock()
	e.interactiveStates[key] = state
	e.interactiveMu.Unlock()

	sendDone := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		e.processInteractiveEvents(state, session, e.sessions, key, "operation", time.Now(), nil, sendDone, "ctx")
		close(done)
	}()
	sendDone <- nil // authoritative command acceptance; no EventResult yet

	deadline := time.Now().Add(time.Second)
	for session.GetAgentSessionID() != "learned-before-result" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := session.GetAgentSessionID(); got != "learned-before-result" {
		t.Fatalf("session id before result = %q", got)
	}
	loaded := NewSessionManager(storePath)
	if got := loaded.GetOrCreateActive(key).GetAgentSessionID(); got != "learned-before-result" {
		t.Fatalf("durable session id before result = %q", got)
	}

	// End waiter without emitting a result; early persistence must stand alone.
	sess.events <- Event{Type: EventError, Error: errors.New("turn failed later")}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("event loop did not finish")
	}
}
