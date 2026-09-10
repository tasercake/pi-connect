package core

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type titleGeneratingStubAgent struct {
	stubAgent
}

type deferredStartAgent struct {
	stubAgent
	started chan struct{}
	session *deferredStartSession
}

func (a *deferredStartAgent) StartSession(context.Context, string) (AgentSession, error) {
	close(a.started)
	return a.session, nil
}

type deferredStartSession struct {
	stubAgentSession
	sent      chan struct{}
	events    chan Event
	closed    chan struct{}
	closeOnce sync.Once
}

func (s *deferredStartSession) Send(string, []ImageAttachment, []FileAttachment) error {
	close(s.sent)
	return nil
}

func (s *deferredStartSession) Events() <-chan Event { return s.events }

func (s *deferredStartSession) Close() error {
	s.closeOnce.Do(func() {
		close(s.closed)
		close(s.events)
	})
	return nil
}

func (a *titleGeneratingStubAgent) GenerateConversationTitle(_ context.Context, content string) (string, error) {
	return "title: " + content, nil
}

type titleSetterStubPlatform struct {
	stubPlatformEngine
	generator func(context.Context, string) (string, error)
}

func (p *titleSetterStubPlatform) SetConversationTitleGenerator(generator func(context.Context, string) (string, error)) {
	p.generator = generator
}

func TestEngineInjectsConversationTitleGeneratorBeforePlatformStart(t *testing.T) {
	agent := &titleGeneratingStubAgent{}
	platform := &titleSetterStubPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	engine := NewEngine("project", agent, []Platform{platform}, "", LangEnglish)
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Stop() })

	if platform.generator == nil {
		t.Fatal("conversation title generator was not injected")
	}
	title, err := platform.generator(context.Background(), "request")
	if err != nil || title != "title: request" {
		t.Fatalf("title/error = %q/%v", title, err)
	}
}

func TestEngineStartsBackendBeforeDeferredRouteResolves(t *testing.T) {
	agentSession := &deferredStartSession{sent: make(chan struct{}), events: make(chan Event, 1), closed: make(chan struct{})}
	agent := &deferredStartAgent{started: make(chan struct{}), session: agentSession}
	platform := &stubPlatformEngine{n: "test"}
	engine := NewEngine("project", agent, nil, "", LangEnglish)
	t.Cleanup(func() { _ = engine.Stop() })

	results := make(chan DeferredRouteResult, 1)
	engine.ReceiveMessage(platform, &Message{
		SessionKey:    "test:pending-10",
		Platform:      "test",
		MessageID:     "10",
		UserID:        "7",
		Content:       "do useful work",
		ReplyCtx:      "deferred-reply",
		DeferredRoute: &DeferredRoute{Result: results},
	})

	select {
	case <-agent.started:
	case <-time.After(time.Second):
		t.Fatal("backend session did not start before route resolution")
	}
	select {
	case <-agentSession.sent:
	case <-time.After(time.Second):
		t.Fatal("backend processing did not start before route resolution")
	}
	if got := engine.sessions.ActiveSessionID("test:final-77"); got != "" {
		t.Fatalf("final route bound before creation: %q", got)
	}

	results <- DeferredRouteResult{SessionKey: "test:final-77", ChannelKey: "final-77", Title: "Useful title"}
	agentSession.events <- Event{Type: EventResult, Content: "done", Done: true}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && engine.sessions.ActiveSessionID("test:final-77") == "" {
		time.Sleep(time.Millisecond)
	}
	if got := engine.sessions.ActiveSessionID("test:final-77"); got == "" {
		t.Fatal("final route was not bound after creation")
	}
}

func TestEngineDeferredRouteFailureAbortsBackendAndDeletesSession(t *testing.T) {
	agentSession := &deferredStartSession{sent: make(chan struct{}), events: make(chan Event), closed: make(chan struct{})}
	agent := &deferredStartAgent{started: make(chan struct{}), session: agentSession}
	engine := NewEngine("project", agent, nil, "", LangEnglish)
	t.Cleanup(func() { _ = engine.Stop() })

	results := make(chan DeferredRouteResult, 1)
	bound := make(chan error, 1)
	engine.ReceiveMessage(&stubPlatformEngine{n: "test"}, &Message{
		SessionKey:    "test:pending-10",
		Platform:      "test",
		MessageID:     "10",
		UserID:        "7",
		Content:       "do useful work",
		ReplyCtx:      "deferred-reply",
		DeferredRoute: &DeferredRoute{Result: results, Bound: bound},
	})
	select {
	case <-agentSession.sent:
	case <-time.After(time.Second):
		t.Fatal("backend processing did not start")
	}
	results <- DeferredRouteResult{Err: context.DeadlineExceeded}

	select {
	case err := <-bound:
		if err == nil {
			t.Fatal("binding error is nil")
		}
	case <-time.After(time.Second):
		t.Fatal("route failure was not acknowledged")
	}
	select {
	case <-agentSession.closed:
	case <-time.After(time.Second):
		t.Fatal("backend session was not closed after route failure")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && engine.sessions.ActiveSessionID("test:pending-10") != "" {
		time.Sleep(time.Millisecond)
	}
	if got := engine.sessions.ActiveSessionID("test:pending-10"); got != "" {
		t.Fatalf("provisional session remains active: %q", got)
	}
}

func TestEngineCopiesGeneralWorkspaceBindingToDeferredTopic(t *testing.T) {
	dir := t.TempDir()
	engine := NewEngine("project", &stubAgent{}, nil, "", LangEnglish)
	t.Cleanup(func() { _ = engine.Stop() })
	engine.SetMultiWorkspace(dir, filepath.Join(dir, "bindings.json"))
	source := workspaceChannelKey("telegram", "-100123:1")
	engine.workspaceBindings.Bind("project:project", source, "General", dir)

	engine.copyDeferredWorkspaceBinding("telegram", source, "-100123:77")
	final := workspaceChannelKey("telegram", "-100123:77")
	binding := engine.workspaceBindings.Lookup("project:project", final)
	if binding == nil || binding.Workspace != dir {
		t.Fatalf("final workspace binding = %#v", binding)
	}
}

func TestEngineDeferredRouteRebindsSessionAndInteractiveState(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "sessions.json")
	engine := NewEngine("project", &stubAgent{}, nil, storePath, LangEnglish)
	provisional := "telegram:1:pending-10:7"
	final := "telegram:1:77:7"
	session := engine.sessions.GetOrCreateActive(provisional)
	state := &interactiveState{deliverySessionKey: provisional}
	engine.interactiveStates[provisional] = state

	results := make(chan DeferredRouteResult, 1)
	engine.watchDeferredRoute(&DeferredRoute{Result: results}, provisional, provisional, "telegram", "", engine.sessions, session)
	results <- DeferredRouteResult{SessionKey: final, ChannelKey: "1:77", Title: "Useful title"}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if engine.sessions.ActiveSessionID(final) == session.ID && engine.resolveDeferredRouteAlias(final) == provisional {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if got := engine.sessions.ActiveSessionID(final); got != session.ID {
		t.Fatalf("final active session = %q, want %q", got, session.ID)
	}
	if got := engine.sessions.ActiveSessionID(provisional); got != "" {
		t.Fatalf("provisional active session remains: %q", got)
	}
	if got := engine.resolveDeferredRouteAlias(final); got != provisional {
		t.Fatalf("resolved alias = %q, want %q", got, provisional)
	}
	_, _, commandKey, err := engine.commandContext(&stubPlatformEngine{n: "telegram"}, &Message{SessionKey: final})
	if err != nil || commandKey != provisional {
		t.Fatalf("command context key/error = %q/%v, want provisional key", commandKey, err)
	}
	state.mu.Lock()
	deliveryKey := state.deliverySessionKey
	state.mu.Unlock()
	if deliveryKey != final {
		t.Fatalf("delivery key = %q, want %q", deliveryKey, final)
	}

	reloaded := NewSessionManager(storePath)
	if got := reloaded.ActiveSessionID(final); got != session.ID {
		t.Fatalf("reloaded active session = %q, want %q", got, session.ID)
	}
}
