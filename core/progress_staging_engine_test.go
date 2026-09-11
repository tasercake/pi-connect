package core

import (
	"strings"
	"testing"
	"time"
)

func TestProcessInteractiveEventsStagingUsesOneTimelineAndSeparateFinal(t *testing.T) {
	p := &stagingCapturePlatform{}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.SetDisplayConfig(DisplayCfg{
		Mode:             "staging",
		ThinkingMessages: true,
		ThinkingMaxLen:   300,
		ToolMessages:     true,
		ToolMaxLen:       500,
	})
	sessionKey := "capable:staging"
	session := e.sessions.GetOrCreateActive(sessionKey)
	agentSession := newControllableSession("staging-session")
	state := &interactiveState{agentSession: agentSession, platform: p, replyCtx: "ctx"}
	e.interactiveStates[sessionKey] = state

	code, success := 0, true
	agentSession.events <- Event{Type: EventThinking, Content: "inspect"}
	agentSession.events <- Event{Type: EventToolUse, ToolName: "Bash", ToolInput: "echo hi", ToolInputRaw: map[string]any{"secret": "must-not-appear"}}
	agentSession.events <- Event{Type: EventToolResult, ToolName: "Bash", ToolResult: "hi", ToolStatus: "completed", ToolExitCode: &code, ToolSuccess: &success}
	agentSession.events <- Event{Type: EventText, Content: "draft "}
	agentSession.events <- Event{Type: EventText, Content: "text"}
	agentSession.events <- Event{Type: EventResult, Content: "final answer", Done: true}

	e.processInteractiveEvents(state, session, e.sessions, sessionKey, "message", time.Now(), nil, nil, state.replyCtx)

	starts, updates, sent := p.snapshot()
	if len(starts) != 1 {
		t.Fatalf("preview starts = %d, want exactly one", len(starts))
	}
	if len(sent) != 1 || sent[0] != "final answer" {
		t.Fatalf("standalone sends = %#v, want separate final answer only", sent)
	}
	if len(updates) == 0 {
		t.Fatal("staging timeline was not edited")
	}
	finalTimeline := updates[len(updates)-1]
	for _, want := range []string{"✅ ", "💭 inspect", "🔧 #1 Bash", "echo hi", "✅ #1 Bash", "hi", "✍️ draft text"} {
		if !strings.Contains(finalTimeline, want) {
			t.Fatalf("final timeline missing %q:\n%s", want, finalTimeline)
		}
	}
	if strings.Contains(finalTimeline, "must-not-appear") {
		t.Fatalf("raw ToolInputRaw leaked:\n%s", finalTimeline)
	}
	if strings.Count(finalTimeline, "✍️") != 1 {
		t.Fatalf("intermediate text was not coalesced:\n%s", finalTimeline)
	}
}

type queuedStagingSession struct {
	*controllableAgentSession
	sent chan struct{}
}

func (s *queuedStagingSession) Send(string, []ImageAttachment, []FileAttachment) error {
	s.sent <- struct{}{}
	return nil
}

func TestProcessInteractiveEventsStagingResetsForQueuedTurn(t *testing.T) {
	p := &stagingCapturePlatform{}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.SetDisplayConfig(DisplayCfg{Mode: "staging", ThinkingMessages: true, ToolMessages: true, ThinkingMaxLen: 300, ToolMaxLen: 500})
	sessionKey := "capable:staging-queue"
	session := e.sessions.GetOrCreateActive(sessionKey)
	agentSession := &queuedStagingSession{controllableAgentSession: newControllableSession("staging-queue-session"), sent: make(chan struct{}, 1)}
	state := &interactiveState{
		agentSession: agentSession,
		platform:     p,
		replyCtx:     "ctx-1",
		pendingMessages: []queuedMessage{{
			messageID: "message-2",
			platform:  p,
			replyCtx:  "ctx-2",
			content:   "second prompt",
		}},
	}
	e.interactiveStates[sessionKey] = state

	agentSession.events <- Event{Type: EventThinking, Content: "first thought"}
	agentSession.events <- Event{Type: EventResult, Content: "first answer", Done: true}
	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		<-agentSession.sent
		agentSession.events <- Event{Type: EventThinking, Content: "second thought"}
		agentSession.events <- Event{Type: EventResult, Content: "second answer", Done: true}
	}()

	e.processInteractiveEvents(state, session, e.sessions, sessionKey, "message-1", time.Now(), nil, nil, state.replyCtx)
	<-secondDone
	starts, updates, sent := p.snapshot()
	if len(starts) != 2 {
		t.Fatalf("preview starts = %d, want one per queued turn", len(starts))
	}
	if len(sent) != 2 || sent[0] != "first answer" || sent[1] != "second answer" {
		t.Fatalf("queued final answers = %#v", sent)
	}
	if !strings.Contains(strings.Join(updates, "\n"), "second thought") {
		t.Fatalf("second staging timeline missing: %#v", updates)
	}
}

func TestConfigModeSetterAcceptsStaging(t *testing.T) {
	e := NewEngine("test", &stubAgent{}, nil, "", LangEnglish)
	var modeItem *configItem
	items := e.configItems()
	for i := range items {
		if items[i].key == "mode" {
			modeItem = &items[i]
			break
		}
	}
	if modeItem == nil {
		t.Fatal("mode config item missing")
	}
	if err := modeItem.setFunc("staging"); err != nil {
		t.Fatalf("staging rejected: %v", err)
	}
	if e.display.Mode != "staging" || !e.display.ThinkingMessages || !e.display.ToolMessages {
		t.Fatalf("staging defaults = %q/%v/%v", e.display.Mode, e.display.ThinkingMessages, e.display.ToolMessages)
	}
	if err := modeItem.setFunc("invalid"); err == nil || !strings.Contains(err.Error(), "staging") {
		t.Fatalf("invalid mode error = %v", err)
	}
}

func TestProcessInteractiveEventsStagingUpdateFailureFallsBackWithoutEventLoss(t *testing.T) {
	p := &stagingCapturePlatform{updateErrAt: 1}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.SetDisplayConfig(DisplayCfg{Mode: "staging", ThinkingMessages: true, ToolMessages: true, ThinkingMaxLen: 300, ToolMaxLen: 500})
	sessionKey := "capable:staging-failure"
	session := e.sessions.GetOrCreateActive(sessionKey)
	agentSession := newControllableSession("staging-failure-session")
	state := &interactiveState{agentSession: agentSession, platform: p, replyCtx: "ctx"}
	e.interactiveStates[sessionKey] = state

	agentSession.events <- Event{Type: EventThinking, Content: "fallback thought"}
	agentSession.events <- Event{Type: EventResult, Content: "final answer", Done: true}
	e.processInteractiveEvents(state, session, e.sessions, sessionKey, "message", time.Now(), nil, nil, state.replyCtx)

	starts, _, sent := p.snapshot()
	if len(starts) != 1 {
		t.Fatalf("preview starts = %d, want one attempted staging message", len(starts))
	}
	joined := strings.Join(sent, "\n")
	if !strings.Contains(joined, "fallback thought") || !strings.Contains(joined, "final answer") {
		t.Fatalf("fallback lost event/final answer: %#v", sent)
	}
}
