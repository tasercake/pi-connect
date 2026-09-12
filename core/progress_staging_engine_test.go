package core

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestProcessInteractiveEventsStagingUsesOneTimelineAndSeparateFinal(t *testing.T) {
	p := &blockingFinalStagingPlatform{
		stagingCapturePlatform: &stagingCapturePlatform{updated: make(chan struct{}, 4)},
		sendStarted:            make(chan struct{}, 1),
		releaseSend:            make(chan struct{}),
	}
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

	done := make(chan struct{})
	go func() {
		e.processInteractiveEvents(state, session, e.sessions, sessionKey, "message", time.Now(), nil, nil, state.replyCtx)
		close(done)
	}()
	waitStaging(t, func() bool { starts, _, _ := p.snapshot(); return len(starts) == 1 }, "engine staging preview")

	code, success := 0, true
	agentSession.events <- Event{Type: EventThinking, Content: "inspect"}
	agentSession.events <- Event{Type: EventToolUse, ToolName: "Bash", ToolInput: "echo hi", ToolInputRaw: map[string]any{"secret": "must-not-appear"}}
	agentSession.events <- Event{Type: EventToolResult, ToolName: "Bash", ToolResult: "hi", ToolStatus: "completed", ToolExitCode: &code, ToolSuccess: &success}
	agentSession.events <- Event{Type: EventText, Content: "draft "}
	agentSession.events <- Event{Type: EventText, Content: "text"}
	agentSession.events <- Event{Type: EventResult, Content: "final answer", Done: true}
	select {
	case <-p.sendStarted:
	case <-time.After(time.Second):
		t.Fatal("final answer send did not start")
	}
	select {
	case <-p.updated:
		renders := allStagingRenders(p.stagingCapturePlatform)
		t.Fatalf("staging updated before final answer send completed: %q", renders[len(renders)-1])
	case <-time.After(50 * time.Millisecond):
	}
	close(p.releaseSend)
	<-done
	waitStaging(t, func() bool {
		starts, updates, _ := p.snapshot()
		renders := append(starts, updates...)
		return len(renders) > 0 && strings.HasPrefix(renders[len(renders)-1], "✅ ")
	}, "engine terminal staging summary")
	starts, updates, sent := p.snapshot()
	if len(starts) != 1 {
		t.Fatalf("preview starts = %d, want exactly one", len(starts))
	}
	if len(sent) != 1 || sent[0] != "final answer" {
		t.Fatalf("standalone sends = %#v, want separate final answer only", sent)
	}
	renders := append(starts, updates...)
	finalSummary := renders[len(renders)-1]
	if !strings.HasPrefix(finalSummary, "✅ ") || !strings.Contains(finalSummary, "🔧 1 · 🪜 4") || strings.Contains(finalSummary, "\n") {
		t.Fatalf("terminal staging message is not a compact one-line summary: %q", finalSummary)
	}
	for _, forbidden := range []string{"inspect", "echo hi", "\nhi", "draft text", "must-not-appear", "```"} {
		if strings.Contains(finalSummary, forbidden) {
			t.Fatalf("terminal summary retained event content %q: %q", forbidden, finalSummary)
		}
	}
}

func TestProcessInteractiveEventsStagingSilentResultDiscardsPreview(t *testing.T) {
	p := &stagingCapturePlatform{}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.SetDisplayConfig(DisplayCfg{Mode: "staging", ThinkingMessages: true, ToolMessages: true})
	sessionKey := "capable:staging-silent"
	session := e.sessions.GetOrCreateActive(sessionKey)
	agentSession := newControllableSession("staging-silent-session")
	state := &interactiveState{agentSession: agentSession, platform: p, replyCtx: "ctx"}
	e.interactiveStates[sessionKey] = state

	done := make(chan struct{})
	go func() {
		e.processInteractiveEvents(state, session, e.sessions, sessionKey, "message", time.Now(), nil, nil, state.replyCtx)
		close(done)
	}()
	waitStaging(t, func() bool { starts, _, _ := p.snapshot(); return len(starts) == 1 }, "silent staging preview")
	agentSession.events <- Event{Type: EventThinking, Content: "private thought"}
	agentSession.events <- Event{Type: EventResult, Content: "NO_REPLY", Done: true}
	<-done
	waitStaging(t, func() bool { return len(p.deleteSnapshot()) == 1 }, "silent staging preview deletion")
	starts, updates, sent := p.snapshot()
	if len(starts) != 1 || len(updates) != 0 || len(sent) != 0 {
		t.Fatalf("silent staging deliveries: starts=%d updates=%d sent=%d", len(starts), len(updates), len(sent))
	}
}

func TestProcessInteractiveEventsStagingFinalSendFailureMarksTimelineFailed(t *testing.T) {
	p := &stagingCapturePlatform{sendErr: errors.New("send failed")}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.SetDisplayConfig(DisplayCfg{Mode: "staging", ThinkingMessages: true, ToolMessages: true})
	sessionKey := "capable:staging-send-failure"
	session := e.sessions.GetOrCreateActive(sessionKey)
	agentSession := newControllableSession("staging-send-failure-session")
	state := &interactiveState{agentSession: agentSession, platform: p, replyCtx: "ctx"}
	e.interactiveStates[sessionKey] = state

	done := make(chan struct{})
	go func() {
		e.processInteractiveEvents(state, session, e.sessions, sessionKey, "message", time.Now(), nil, nil, state.replyCtx)
		close(done)
	}()
	waitStaging(t, func() bool { starts, _, _ := p.snapshot(); return len(starts) == 1 }, "send-failure staging preview")
	agentSession.events <- Event{Type: EventThinking, Content: "retained failure context"}
	agentSession.events <- Event{Type: EventResult, Content: "unsent final answer", Done: true}
	<-done
	waitStaging(t, func() bool {
		_, updates, _ := p.snapshot()
		return len(updates) > 0 && strings.HasPrefix(updates[len(updates)-1], "❌ ")
	}, "failed terminal staging update")
	_, updates, sent := p.snapshot()
	last := updates[len(updates)-1]
	if len(sent) != 1 || sent[0] != "unsent final answer" || !strings.Contains(last, "retained failure context") || strings.HasPrefix(last, "✅ ") {
		t.Fatalf("send-failure lifecycle: sent=%#v terminal=%q", sent, last)
	}
}

type blockingFinalStagingPlatform struct {
	*stagingCapturePlatform
	sendStarted chan struct{}
	releaseSend chan struct{}
}

func (p *blockingFinalStagingPlatform) Send(ctx context.Context, replyCtx any, content string) error {
	select {
	case p.sendStarted <- struct{}{}:
	default:
	}
	select {
	case <-p.releaseSend:
		return p.stagingCapturePlatform.Send(ctx, replyCtx, content)
	case <-ctx.Done():
		return ctx.Err()
	}
}

type blockingStagingSchedulerPlatform struct {
	*stagingCapturePlatform
	release chan struct{}
}

func (p *blockingStagingSchedulerPlatform) AcquireProgressUpdate(ctx context.Context) (func(bool), error) {
	select {
	case <-p.release:
		return func(bool) {}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (p *blockingStagingSchedulerPlatform) DeferProgressUpdates(time.Duration) {}
func (p *blockingStagingSchedulerPlatform) SendWithButtons(_ context.Context, _ any, content string, buttons [][]ButtonOption) error {
	if len(buttons) == 0 {
		return errors.New("missing permission buttons")
	}
	return p.Send(context.Background(), nil, content)
}

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

	done := make(chan struct{})
	go func() {
		e.processInteractiveEvents(state, session, e.sessions, sessionKey, "message-1", time.Now(), nil, nil, state.replyCtx)
		close(done)
	}()
	waitStaging(t, func() bool { starts, _, _ := p.snapshot(); return len(starts) == 1 }, "first queued-turn preview")
	agentSession.events <- Event{Type: EventThinking, Content: "first thought"}
	agentSession.events <- Event{Type: EventResult, Content: "first answer", Done: true}
	<-agentSession.sent
	waitStaging(t, func() bool { starts, _, _ := p.snapshot(); return len(starts) == 2 }, "second queued-turn preview")
	agentSession.events <- Event{Type: EventThinking, Content: "second thought"}
	agentSession.events <- Event{Type: EventResult, Content: "second answer", Done: true}
	<-done
	waitStaging(t, func() bool {
		starts, updates, _ := p.snapshot()
		return len(starts) == 2 && len(updates) >= 2
	}, "queued staging terminal summaries")
	starts, updates, sent := p.snapshot()
	if len(starts) != 2 {
		t.Fatalf("preview starts = %d, want one per queued turn", len(starts))
	}
	if len(sent) != 2 || sent[0] != "first answer" || sent[1] != "second answer" {
		t.Fatalf("queued final answers = %#v", sent)
	}
	last := updates[len(updates)-1]
	if !strings.HasPrefix(last, "✅ ") || !strings.Contains(last, "🔧 0 · 🪜 1") || strings.Contains(last, "second thought") || strings.Contains(last, "\n") {
		t.Fatalf("second turn terminal summary = %q", last)
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
	waitStaging(t, func() bool { calls, _ := p.attempts(); return calls == 2 }, "429 terminal-summary retry")
	_, updates, _ := p.snapshot()
	if len(updates) != 1 || !strings.HasPrefix(updates[0], "✅ ") || !strings.Contains(updates[0], "🔧 1 · 🪜 2") || strings.Contains(updates[0], "latest state") || strings.Contains(updates[0], "\n") {
		t.Fatalf("429 retry did not send compact terminal summary: %#v", updates)
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
	waitStaging(t, func() bool { calls, _ := p.attempts(); return calls == 1 }, "permanent update failure")

	starts, updates, sent := p.snapshot()
	if len(starts) != 1 {
		t.Fatalf("preview starts = %d, want one attempted staging message", len(starts))
	}
	if len(updates) != 0 {
		t.Fatalf("permanent failure unexpectedly edited timeline: %#v", updates)
	}
	if len(sent) != 1 || sent[0] != "final answer" {
		t.Fatalf("permanent failure flooded standalone progress: %#v", sent)
	}
}
