package core

import (
	"context"
	"errors"
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

	waitStaging(t, func() bool { starts, _, _ := p.snapshot(); return len(starts) == 1 }, "engine staging preview")
	starts, updates, sent := p.snapshot()
	if len(starts) != 1 {
		t.Fatalf("preview starts = %d, want exactly one", len(starts))
	}
	if len(sent) != 1 || sent[0] != "final answer" {
		t.Fatalf("standalone sends = %#v, want separate final answer only", sent)
	}
	renders := append(starts, updates...)
	finalTimeline := renders[len(renders)-1]
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

type blockingStagingSchedulerPlatform struct {
	*stagingCapturePlatform
	release chan struct{}
}

func (p *blockingStagingSchedulerPlatform) WaitProgressUpdate(ctx context.Context) error {
	select {
	case <-p.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (p *blockingStagingSchedulerPlatform) DeferProgressUpdates(time.Duration) {}

func TestStagingProgressWaitDoesNotDelayPermissionPrompt(t *testing.T) {
	capture := &stagingCapturePlatform{}
	p := &blockingStagingSchedulerPlatform{stagingCapturePlatform: capture, release: make(chan struct{})}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.SetDisplayConfig(DisplayCfg{Mode: "staging", ThinkingMessages: true, ToolMessages: true, ThinkingMaxLen: 300, ToolMaxLen: 500})
	sessionKey := "capable:permission-priority"
	session := e.sessions.GetOrCreateActive(sessionKey)
	agentSession := newControllableSession("permission-priority-session")
	state := &interactiveState{agentSession: agentSession, platform: p, replyCtx: "ctx"}
	e.interactiveStates[sessionKey] = state

	done := make(chan struct{})
	go func() {
		e.processInteractiveEvents(state, session, e.sessions, sessionKey, "message", time.Now(), nil, nil, state.replyCtx)
		close(done)
	}()
	agentSession.events <- Event{Type: EventPermissionRequest, RequestID: "permission-1", ToolName: "Write", ToolInput: "file.txt"}
	waitStaging(t, func() bool { _, _, sent := capture.snapshot(); return len(sent) == 1 }, "permission prompt while progress gate is blocked")
	if starts, _, _ := capture.snapshot(); len(starts) != 0 {
		t.Fatal("progress gate was not blocked during permission prompt assertion")
	}

	state.mu.Lock()
	pending := state.pending
	state.mu.Unlock()
	if pending == nil {
		t.Fatal("permission was not actionable")
	}
	close(pending.Resolved)
	close(p.release)
	agentSession.events <- Event{Type: EventResult, Content: "final answer", Done: true}
	<-done
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
	waitStaging(t, func() bool {
		starts, updates, _ := p.snapshot()
		return len(starts) == 2 && strings.Contains(strings.Join(append(starts, updates...), "\n"), "second thought")
	}, "queued staging final timeline")
	starts, updates, sent := p.snapshot()
	if len(starts) != 2 {
		t.Fatalf("preview starts = %d, want one per queued turn", len(starts))
	}
	if len(sent) != 2 || sent[0] != "first answer" || sent[1] != "second answer" {
		t.Fatalf("queued final answers = %#v", sent)
	}
	if !strings.Contains(strings.Join(append(starts, updates...), "\n"), "second thought") {
		t.Fatalf("second staging timeline missing: starts=%#v updates=%#v", starts, updates)
	}
}

func TestCmdQuietMidTurnAppliesStagingOnlyToNewTurns(t *testing.T) {
	p := &stagingCapturePlatform{}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.SetDisplayConfig(DisplayCfg{Mode: "full", ThinkingMessages: true, ToolMessages: true, ThinkingMaxLen: 300, ToolMaxLen: 500})
	sessionKey := "capable:mid-turn-mode"
	session := e.sessions.GetOrCreateActive(sessionKey)
	agentSession := newControllableSession("mid-turn-mode-session")
	state := &interactiveState{agentSession: agentSession, platform: p, replyCtx: "ctx"}
	e.interactiveStates[sessionKey] = state

	done := make(chan struct{})
	go func() {
		e.processInteractiveEvents(state, session, e.sessions, sessionKey, "message", time.Now(), nil, nil, state.replyCtx)
		close(done)
	}()
	agentSession.events <- Event{Type: EventThinking, Content: "before command"}
	waitStaging(t, func() bool { _, _, sent := p.snapshot(); return len(sent) >= 1 }, "first full-mode progress")

	e.cmdQuiet(p, &Message{ReplyCtx: "command"}, []string{"staging"})
	replies := p.replySnapshot()
	if len(replies) != 1 || !strings.Contains(replies[0], "new turns") || !strings.Contains(replies[0], "Active turns") {
		t.Fatalf("mid-turn command response is ambiguous: %#v", replies)
	}

	agentSession.events <- Event{Type: EventThinking, Content: "after command"}
	agentSession.events <- Event{Type: EventResult, Content: "final answer", Done: true}
	<-done
	starts, _, sent := p.snapshot()
	if len(starts) != 0 {
		t.Fatalf("active full-mode turn hot-switched to staging: %d previews", len(starts))
	}
	joined := strings.Join(sent, "\n")
	for _, want := range []string{"before command", "after command", "final answer"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("active turn did not retain full display; missing %q in %#v", want, sent)
		}
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

func TestProcessInteractiveEventsStaging429RetriesWithoutBlockingFinal(t *testing.T) {
	transientErr := errors.New("telegram 429")
	p := &stagingCapturePlatform{updateErrAt: 1, updateErr: transientErr, retryDelay: 250 * time.Millisecond}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.SetDisplayConfig(DisplayCfg{Mode: "staging", ThinkingMessages: true, ToolMessages: true, ThinkingMaxLen: 300, ToolMaxLen: 500})
	sessionKey := "capable:staging-429"
	session := e.sessions.GetOrCreateActive(sessionKey)
	agentSession := newControllableSession("staging-429-session")
	state := &interactiveState{agentSession: agentSession, platform: p, replyCtx: "ctx"}
	e.interactiveStates[sessionKey] = state

	done := make(chan struct{})
	go func() {
		e.processInteractiveEvents(state, session, e.sessions, sessionKey, "message", time.Now(), nil, nil, state.replyCtx)
		close(done)
	}()
	waitStaging(t, func() bool { starts, _, _ := p.snapshot(); return len(starts) == 1 }, "429 test preview")
	agentSession.events <- Event{Type: EventThinking, Content: "coalesced thought"}
	agentSession.events <- Event{Type: EventToolUse, ToolName: "Bash", ToolInput: "latest state"}
	agentSession.events <- Event{Type: EventResult, Content: "final answer", Done: true}
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("429 retry blocked final answer/event loop")
	}
	_, _, sent := p.snapshot()
	if len(sent) != 1 || sent[0] != "final answer" {
		t.Fatalf("429 emitted legacy progress: %#v", sent)
	}
	waitStaging(t, func() bool { calls, _ := p.attempts(); return calls == 2 }, "429 latest-snapshot retry")
	_, updates, _ := p.snapshot()
	if len(updates) != 1 || !strings.Contains(updates[0], "latest state") {
		t.Fatalf("429 retry did not retain latest timeline: %#v", updates)
	}
}

func TestProcessInteractiveEventsStagingPermanentUpdateFailureDoesNotFlood(t *testing.T) {
	p := &stagingCapturePlatform{updateErrAt: 1}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.SetDisplayConfig(DisplayCfg{Mode: "staging", ThinkingMessages: true, ToolMessages: true, ThinkingMaxLen: 300, ToolMaxLen: 500})
	sessionKey := "capable:staging-failure"
	session := e.sessions.GetOrCreateActive(sessionKey)
	agentSession := newControllableSession("staging-failure-session")
	state := &interactiveState{agentSession: agentSession, platform: p, replyCtx: "ctx"}
	e.interactiveStates[sessionKey] = state

	done := make(chan struct{})
	go func() {
		e.processInteractiveEvents(state, session, e.sessions, sessionKey, "message", time.Now(), nil, nil, state.replyCtx)
		close(done)
	}()
	waitStaging(t, func() bool { starts, _, _ := p.snapshot(); return len(starts) == 1 }, "failure test preview")
	agentSession.events <- Event{Type: EventThinking, Content: "must not become standalone"}
	agentSession.events <- Event{Type: EventToolUse, ToolName: "Bash", ToolInput: "echo quiet"}
	agentSession.events <- Event{Type: EventResult, Content: "final answer", Done: true}
	<-done

	starts, _, sent := p.snapshot()
	if len(starts) != 1 {
		t.Fatalf("preview starts = %d, want one attempted staging message", len(starts))
	}
	if len(sent) != 1 || sent[0] != "final answer" {
		t.Fatalf("permanent failure flooded standalone progress: %#v", sent)
	}
}
