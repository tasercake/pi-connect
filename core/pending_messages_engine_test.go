package core

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type durableTestPlatform struct {
	stubPlatformEngine
	reconstructMu sync.Mutex
	reconstructed []string
	replyCtxs     []any
	onReply       func(string)
}

func (p *durableTestPlatform) ReconstructReplyCtx(sessionKey string) (any, error) {
	p.reconstructMu.Lock()
	p.reconstructed = append(p.reconstructed, sessionKey)
	p.reconstructMu.Unlock()
	return "reconstructed:" + sessionKey, nil
}

func (p *durableTestPlatform) Reply(ctx context.Context, replyCtx any, content string) error {
	if p.onReply != nil {
		p.onReply(content)
	}
	p.reconstructMu.Lock()
	p.replyCtxs = append(p.replyCtxs, replyCtx)
	p.reconstructMu.Unlock()
	return p.stubPlatformEngine.Reply(ctx, replyCtx, content)
}

func (p *durableTestPlatform) Send(ctx context.Context, replyCtx any, content string) error {
	p.reconstructMu.Lock()
	p.replyCtxs = append(p.replyCtxs, replyCtx)
	p.reconstructMu.Unlock()
	return p.stubPlatformEngine.Send(ctx, replyCtx, content)
}

type durableOutcomeUnknownError struct{}

func (durableOutcomeUnknownError) Error() string        { return "delivery uncertain" }
func (durableOutcomeUnknownError) OutcomeUnknown() bool { return true }

type durableErrorSession struct {
	*durableTestSession
	err error
}

func (s *durableErrorSession) Send(prompt string, images []ImageAttachment, files []FileAttachment) error {
	s.sends <- durableSendCall{prompt: prompt, images: images, files: files}
	return s.err
}

type durableSendCall struct {
	prompt string
	images []ImageAttachment
	files  []FileAttachment
}

type durableFactoryAgent struct {
	name    string
	session AgentSession
}

func (a *durableFactoryAgent) Name() string { return a.name }
func (a *durableFactoryAgent) StartSession(context.Context, string) (AgentSession, error) {
	return a.session, nil
}
func (a *durableFactoryAgent) ListSessions(context.Context) ([]AgentSessionInfo, error) {
	return nil, nil
}
func (a *durableFactoryAgent) Stop() error { return nil }

type durableTestSession struct {
	events     chan Event
	sends      chan durableSendCall
	alive      bool
	beforeSend func()
}

func newDurableTestSession() *durableTestSession {
	return &durableTestSession{events: make(chan Event, 16), sends: make(chan durableSendCall, 16), alive: true}
}

func (s *durableTestSession) Send(prompt string, images []ImageAttachment, files []FileAttachment) error {
	if s.beforeSend != nil {
		s.beforeSend()
	}
	s.sends <- durableSendCall{prompt: prompt, images: images, files: files}
	s.events <- Event{Type: EventResult, Content: "done", Done: true}
	return nil
}
func (s *durableTestSession) RespondPermission(string, PermissionResult) error { return nil }
func (s *durableTestSession) Events() <-chan Event                             { return s.events }
func (s *durableTestSession) CurrentSessionID() string                         { return "durable-session" }
func (s *durableTestSession) Alive() bool                                      { return s.alive }
func (s *durableTestSession) Close() error {
	s.alive = false
	return nil
}

func newDurableEngine(t *testing.T, p Platform, agent Agent, store *PendingMessageStore) *Engine {
	t.Helper()
	e := NewEngine("project", agent, []Platform{p}, t.TempDir()+"/sessions.json", LangEnglish)
	e.SetPendingMessageStore(store)
	return e
}

func TestDurableQueuePersistsBeforeAcknowledgement(t *testing.T) {
	store, err := NewPendingMessageStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := &durableTestPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	e := newDurableEngine(t, p, &stubAgent{}, store)
	state := &interactiveState{agentSession: &stubAgentSession{}, platform: p, replyCtx: "current"}
	e.interactiveStates["raw"] = state
	p.onReply = func(content string) {
		if content != e.i18n.T(MsgMessageQueued) {
			return
		}
		records, listErr := store.List("project", "test")
		if listErr != nil || len(records) != 1 {
			t.Errorf("ack before persistence: records=%d err=%v", len(records), listErr)
		}
	}

	if !e.queueMessageForBusySession(p, &Message{SessionKey: "raw", Platform: "test", MessageID: "m1", Content: "queued", ReplyCtx: "incoming"}, "raw") {
		t.Fatal("message not handled")
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.pendingMessages) != 1 || state.pendingMessages[0].durableID == "" {
		t.Fatalf("memory queue=%#v", state.pendingMessages)
	}
}

func TestDurableQueuePersistenceFailureDoesNotAccept(t *testing.T) {
	store, err := NewPendingMessageStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(store.dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.dir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &durableTestPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	e := newDurableEngine(t, p, &stubAgent{}, store)
	state := &interactiveState{agentSession: &stubAgentSession{}, platform: p}
	e.interactiveStates["raw"] = state

	if !e.queueMessageForBusySession(p, &Message{SessionKey: "raw", Platform: "test", MessageID: "m1", Content: "queued", ReplyCtx: "ctx"}, "raw") {
		t.Fatal("failure was not handled")
	}
	state.mu.Lock()
	queued := len(state.pendingMessages)
	state.mu.Unlock()
	if queued != 0 {
		t.Fatalf("queued after persistence failure: %d", queued)
	}
	sent := p.getSent()
	if len(sent) != 1 || sent[0] != e.i18n.T(MsgQueuePersistenceFailed) {
		t.Fatalf("replies=%v", sent)
	}
}

func TestDurableQueueDispatchTransitionFailureDefersWithoutSend(t *testing.T) {
	store, err := NewPendingMessageStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := &durableTestPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	sess := newDurableTestSession()
	e := newDurableEngine(t, p, &resultAgent{session: sess}, store)
	state := &interactiveState{agentSession: sess, platform: p}
	e.interactiveStates["raw"] = state
	msg := &Message{SessionKey: "raw", Platform: "test", MessageID: "transition", Content: "queued", ReplyCtx: "ctx"}
	if !e.queueMessageForBusySession(p, msg, "raw") {
		t.Fatal("queue failed")
	}
	state.mu.Lock()
	id := state.pendingMessages[0].durableID
	state.mu.Unlock()
	if err := os.Mkdir(store.path(id, PendingMessageStateDispatching), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.takeNextQueuedMessage(state); ok {
		t.Fatal("message dispatched after transition failure")
	}
	select {
	case call := <-sess.sends:
		t.Fatalf("agent Send called: %#v", call)
	default:
	}
	state.mu.Lock()
	remaining := len(state.pendingMessages)
	state.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("failed recovery attempt remains in memory: %d", remaining)
	}
	records, listErr := store.List("project", "test")
	if listErr != nil || len(records) != 1 || records[0].State != PendingMessageStatePending {
		t.Fatalf("pending record not deferred: %#v err=%v", records, listErr)
	}
	sent := p.getSent()
	if len(sent) < 2 || sent[len(sent)-1] != e.i18n.T(MsgQueueDispatchDeferred) {
		t.Fatalf("deferral notice missing: %v", sent)
	}
}

func TestDurableQueueMarkDispatchingPrecedesSendAndCompletionRemoves(t *testing.T) {
	store, err := NewPendingMessageStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := &durableTestPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	sess := newDurableTestSession()
	agent := &resultAgent{session: sess}
	e := newDurableEngine(t, p, agent, store)
	state := &interactiveState{agentSession: sess, platform: p, replyCtx: "current", agent: agent}
	e.interactiveStates["raw"] = state
	msg := &Message{SessionKey: "raw", Platform: "test", MessageID: "m1", Content: "queued", ReplyCtx: "ctx", Files: []FileAttachment{{FileName: "queued.txt", Data: []byte("payload")}}}
	workDir := t.TempDir()
	e.SetBaseWorkDir(workDir)
	if !e.queueMessageForBusySession(p, msg, "raw") {
		t.Fatal("queue failed")
	}
	if _, err := os.Stat(workDir + "/.pi-connect/attachments"); !os.IsNotExist(err) {
		t.Fatalf("queue wrote workspace attachments before dispatch: %v", err)
	}
	sess.beforeSend = func() {
		records, listErr := store.List("project", "test")
		if listErr != nil || len(records) != 1 || records[0].State != PendingMessageStateDispatching {
			t.Errorf("send before dispatch transition: records=%#v err=%v", records, listErr)
		}
	}
	session := e.sessions.GetOrCreateActive("raw")
	if !session.TryLock() {
		t.Fatal("session lock")
	}
	done := make(chan bool, 1)
	go func() { done <- e.drainPendingMessages(state, session, e.sessions, "raw") }()
	select {
	case call := <-sess.sends:
		if call.prompt != "queued" || !reflect.DeepEqual(call.files, msg.Files) {
			t.Fatalf("queued call=%#v", call)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("send timeout")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("drain timeout")
	}
	records, err := store.List("project", "test")
	if err != nil || len(records) != 0 {
		t.Fatalf("record leaked after result: %#v err=%v", records, err)
	}
}

func TestDurableQueueSendFailureState(t *testing.T) {
	for _, tc := range []struct {
		name      string
		sendErr   error
		wantState PendingMessageState
		wantCount int
	}{
		{name: "known removes", sendErr: errors.New("rejected"), wantCount: 0},
		{name: "unknown retains dispatching", sendErr: durableOutcomeUnknownError{}, wantState: PendingMessageStateDispatching, wantCount: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := NewPendingMessageStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			p := &durableTestPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
			base := newDurableTestSession()
			sess := &durableErrorSession{durableTestSession: base, err: tc.sendErr}
			agent := &resultAgent{session: sess}
			e := newDurableEngine(t, p, agent, store)
			state := &interactiveState{agentSession: sess, platform: p, replyCtx: "current", agent: agent}
			e.interactiveStates["raw"] = state
			msg := &Message{SessionKey: "raw", Platform: "test", MessageID: "m1", Content: "queued", ReplyCtx: "ctx"}
			if !e.queueMessageForBusySession(p, msg, "raw") {
				t.Fatal("queue failed")
			}
			session := e.sessions.GetOrCreateActive("raw")
			if !session.TryLock() {
				t.Fatal("session lock")
			}
			done := make(chan bool, 1)
			go func() { done <- e.drainPendingMessages(state, session, e.sessions, "raw") }()
			select {
			case <-base.sends:
			case <-time.After(3 * time.Second):
				t.Fatal("send timeout")
			}
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("drain timeout")
			}
			records, listErr := store.List("project", "test")
			if listErr != nil || len(records) != tc.wantCount {
				t.Fatalf("records=%#v err=%v", records, listErr)
			}
			if tc.wantCount == 1 && records[0].State != tc.wantState {
				t.Fatalf("state=%s, want %s", records[0].State, tc.wantState)
			}
		})
	}
}

func TestDurableRecoveryPreservesMultiMessageFIFO(t *testing.T) {
	store, err := NewPendingMessageStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first := pendingTestRecord("fifo-1", "first")
	second := pendingTestRecord("fifo-2", "second")
	second.CreatedAt = first.CreatedAt.Add(time.Second)
	if _, _, err := store.Enqueue(second); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Enqueue(first); err != nil {
		t.Fatal(err)
	}
	p := &durableTestPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	sess := newDurableTestSession()
	e := newDurableEngine(t, p, &resultAgent{session: sess}, store)
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	defer e.Stop()
	var got []string
	for len(got) < 2 {
		select {
		case call := <-sess.sends:
			got = append(got, call.prompt)
		case <-time.After(3 * time.Second):
			t.Fatalf("recovery sends=%v", got)
		}
	}
	if !reflect.DeepEqual(got, []string{"first", "second"}) {
		t.Fatalf("recovery order=%v", got)
	}
	if sent := waitForPlatformSend(&p.stubPlatformEngine, 2, 3*time.Second); len(sent) < 2 {
		t.Fatalf("recovery responses=%v", sent)
	}
}

func TestDurablePendingSurvivesEngineStopAndRecoversAttachments(t *testing.T) {
	dataDir := t.TempDir()
	store, err := NewPendingMessageStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	p1 := &durableTestPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	e1 := newDurableEngine(t, p1, &stubAgent{}, store)
	state := &interactiveState{agentSession: &stubAgentSession{}, platform: p1}
	e1.interactiveStates["raw"] = state
	images := []ImageAttachment{{MimeType: "image/png", Data: []byte{1, 2}, FileName: "a.png"}}
	files := []FileAttachment{{MimeType: "text/plain", Data: []byte("x"), FileName: ""}}
	msg := &Message{SessionKey: "raw", Platform: "test", MessageID: "m1", Content: "queued", Images: images, Files: files, ReplyCtx: "ctx"}
	if !e1.queueMessageForBusySession(p1, msg, "raw") {
		t.Fatal("queue failed")
	}
	if err := e1.Stop(); err != nil {
		t.Fatal(err)
	}
	if records, _ := store.List("project", "test"); len(records) != 1 || records[0].State != PendingMessageStatePending {
		t.Fatalf("pending did not survive stop: %#v", records)
	}

	p2 := &durableTestPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	sess := newDurableTestSession()
	e2 := newDurableEngine(t, p2, &resultAgent{session: sess}, store)
	if err := e2.Start(); err != nil {
		t.Fatal(err)
	}
	defer e2.Stop()
	select {
	case call := <-sess.sends:
		if call.prompt != "queued" || !reflect.DeepEqual(call.images, images) || !reflect.DeepEqual(call.files, files) {
			t.Fatalf("recovered call=%#v", call)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("recovery send timeout")
	}
	if sent := waitForPlatformSend(&p2.stubPlatformEngine, 1, 3*time.Second); len(sent) == 0 {
		t.Fatal("recovered response timeout")
	}
	if records, _ := store.List("project", "test"); len(records) != 0 {
		t.Fatalf("record not removed after result: %#v", records)
	}
	p2.reconstructMu.Lock()
	gotKeys := append([]string(nil), p2.reconstructed...)
	gotCtx := append([]any(nil), p2.replyCtxs...)
	p2.reconstructMu.Unlock()
	if len(gotKeys) == 0 || gotKeys[0] != "raw" {
		t.Fatalf("reconstructed keys=%v", gotKeys)
	}
	foundCtx := false
	for _, ctx := range gotCtx {
		if ctx == "reconstructed:raw" {
			foundCtx = true
		}
	}
	if !foundCtx {
		t.Fatalf("recovered reply context not used: %v", gotCtx)
	}
}

func TestDurableDispatchingWarnsWithoutReplay(t *testing.T) {
	store, err := NewPendingMessageStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := store.Enqueue(pendingTestRecord("m1", "must not replay"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDispatching(id); err != nil {
		t.Fatal(err)
	}
	p := &durableTestPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	sess := newDurableTestSession()
	e := newDurableEngine(t, p, &resultAgent{session: sess}, store)
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	defer e.Stop()
	sent := waitForPlatformSend(&p.stubPlatformEngine, 1, 3*time.Second)
	if len(sent) != 1 || sent[0] != e.i18n.T(MsgAgentOutcomeUnknown) {
		t.Fatalf("notices=%v", sent)
	}
	select {
	case call := <-sess.sends:
		t.Fatalf("stale prompt replayed: %#v", call)
	default:
	}
	e.lifecycleWorkersWG.Wait()
	if records, _ := store.List("project", "test"); len(records) != 0 {
		t.Fatalf("stale dispatching record remains after notice: %#v", records)
	}
}

func TestDurableRecallAfterDispatchTransitionCancelsRecord(t *testing.T) {
	store, err := NewPendingMessageStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := &durableTestPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	sess := newDurableTestSession()
	e := newDurableEngine(t, p, &resultAgent{session: sess}, store)
	state := &interactiveState{agentSession: sess, platform: p}
	e.interactiveStates["raw"] = state
	msg := &Message{SessionKey: "raw", Platform: "test", MessageID: "recall-transition", Content: "queued", ReplyCtx: "ctx"}
	if !e.queueMessageForBusySession(p, msg, "raw") {
		t.Fatal("queue failed")
	}
	if queued, ok := e.takeNextQueuedMessage(state); !ok || queued.messageID != msg.MessageID {
		t.Fatalf("transition failed: %#v %v", queued, ok)
	}
	e.handleMessageRecall(p, &Message{Platform: "test", MessageID: msg.MessageID, Recalled: true})
	if records, _ := store.List("project", "test"); len(records) != 0 {
		t.Fatalf("recall left dispatching record: %#v", records)
	}
	select {
	case call := <-sess.sends:
		t.Fatalf("recalled prompt sent: %#v", call)
	default:
	}
}

func TestDurableQueueStopDiskOnlyAndRecall(t *testing.T) {
	store, err := NewPendingMessageStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := &durableTestPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	e := newDurableEngine(t, p, &stubAgent{}, store)
	record := pendingTestRecord("stop-me", "queued")
	if _, _, err := store.Enqueue(record); err != nil {
		t.Fatal(err)
	}
	e.cmdStop(p, &Message{SessionKey: record.SessionKey, ReplyCtx: "stop"})
	if sent := p.getSent(); len(sent) != 1 || sent[0] != e.i18n.T(MsgExecutionStopped) {
		t.Fatalf("stop replies=%v", sent)
	}
	if records, _ := store.List("project", "test"); len(records) != 0 {
		t.Fatalf("stop left records: %#v", records)
	}

	p.clearSent()
	if _, _, err := store.Enqueue(pendingTestRecord("recall-me", "queued")); err != nil {
		t.Fatal(err)
	}
	e.handleMessageRecall(p, &Message{Platform: "test", MessageID: "recall-me", Recalled: true})
	if records, _ := store.List("project", "test"); len(records) != 0 {
		t.Fatalf("disk-only recall left records: %#v", records)
	}

	state := &interactiveState{agentSession: &stubAgentSession{}, platform: p}
	e.interactiveStates["memory"] = state
	memoryMsg := &Message{SessionKey: "memory", Platform: "test", MessageID: "memory-recall", Content: "queued", ReplyCtx: "ctx"}
	if !e.queueMessageForBusySession(p, memoryMsg, "memory") {
		t.Fatal("memory recall queue failed")
	}
	e.handleMessageRecall(p, &Message{Platform: "test", MessageID: "memory-recall", Recalled: true})
	state.mu.Lock()
	remaining := len(state.pendingMessages)
	state.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("memory recall left %d queued messages", remaining)
	}
	if records, _ := store.List("project", "test"); len(records) != 0 {
		t.Fatalf("memory recall left records: %#v", records)
	}
}

func TestDurableQueueConcurrentEngineStopAndSendFailurePreservesSpool(t *testing.T) {
	store, err := NewPendingMessageStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	currentID, _, err := store.Enqueue(pendingTestRecord("current-stop", "current"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDispatching(currentID); err != nil {
		t.Fatal(err)
	}
	nextRecord := pendingTestRecord("next-stop", "next")
	nextID, _, err := store.Enqueue(nextRecord)
	if err != nil {
		t.Fatal(err)
	}
	p := &durableTestPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	sess := newDurableTestSession()
	e := newDurableEngine(t, p, &resultAgent{session: sess}, store)
	state := &interactiveState{
		agentSession: sess, platform: p, replyCtx: "ctx", agent: &stubAgent{},
		currentMessageID: "current-stop", currentDurableQueueID: currentID,
		pendingMessages: []queuedMessage{{messageID: "next-stop", platform: p, replyCtx: "next-ctx", content: "next", durableID: nextID}},
	}
	e.interactiveStates["raw"] = state
	sendDone := make(chan error, 1)
	processed := make(chan struct{})
	go func() {
		e.processInteractiveEvents(state, e.sessions.GetOrCreateActive("raw"), e.sessions, "raw", "current-stop", time.Now(), nil, sendDone, "ctx")
		close(processed)
	}()
	if err := e.Stop(); err != nil {
		t.Fatal(err)
	}
	sendDone <- context.Canceled
	select {
	case <-processed:
	case <-time.After(3 * time.Second):
		t.Fatal("event processor did not stop")
	}
	records, listErr := store.List("project", "test")
	if listErr != nil || len(records) != 2 {
		t.Fatalf("shutdown spool=%#v err=%v", records, listErr)
	}
}

func TestDurableQueueShutdownEventResultRacePreservesSpool(t *testing.T) {
	store, err := NewPendingMessageStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	currentID, _, err := store.Enqueue(pendingTestRecord("current", "current"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDispatching(currentID); err != nil {
		t.Fatal(err)
	}
	nextRecord := pendingTestRecord("next", "next")
	nextID, _, err := store.Enqueue(nextRecord)
	if err != nil {
		t.Fatal(err)
	}
	p := &durableTestPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	sess := newDurableTestSession()
	sess.events <- Event{Type: EventResult, Content: "done", Done: true}
	e := newDurableEngine(t, p, &resultAgent{session: sess}, store)
	state := &interactiveState{
		agentSession: sess, platform: p, replyCtx: "ctx", agent: &stubAgent{},
		currentMessageID: "current", currentDurableQueueID: currentID,
		pendingMessages: []queuedMessage{{messageID: "next", platform: p, replyCtx: "next-ctx", content: "next", durableID: nextID}},
	}
	e.interactiveStates["raw"] = state
	e.cancel()
	sendDone := make(chan error, 1)
	sendDone <- nil
	e.processInteractiveEvents(state, e.sessions.GetOrCreateActive("raw"), e.sessions, "raw", "current", time.Now(), nil, sendDone, "ctx")

	state.mu.Lock()
	remaining := len(state.pendingMessages)
	state.mu.Unlock()
	if remaining != 1 {
		t.Fatalf("EventResult popped pending after shutdown: %d", remaining)
	}
	records, listErr := store.List("project", "test")
	if listErr != nil || len(records) != 2 {
		t.Fatalf("shutdown spool=%#v err=%v", records, listErr)
	}
	states := map[string]PendingMessageState{}
	for _, record := range records {
		states[record.ID] = record.State
	}
	if states[currentID] != PendingMessageStateDispatching || states[nextID] != PendingMessageStatePending {
		t.Fatalf("shutdown states=%v", states)
	}
}

func TestDurableQueueDuplicateAndShutdownDoNotPop(t *testing.T) {
	store, err := NewPendingMessageStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := &durableTestPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	e := newDurableEngine(t, p, &stubAgent{}, store)
	state := &interactiveState{agentSession: &stubAgentSession{}, platform: p}
	e.interactiveStates["raw"] = state
	msg := &Message{SessionKey: "raw", Platform: "test", MessageID: "same", Content: "queued", ReplyCtx: "ctx"}
	if !e.queueMessageForBusySession(p, msg, "raw") || !e.queueMessageForBusySession(p, msg, "raw") {
		t.Fatal("queue failed")
	}
	state.mu.Lock()
	if len(state.pendingMessages) != 1 {
		state.mu.Unlock()
		t.Fatalf("duplicate memory queue length=%d", len(state.pendingMessages))
	}
	state.mu.Unlock()
	e.cancel()
	if _, ok := e.takeNextQueuedMessage(state); ok {
		t.Fatal("queue popped after engine cancellation")
	}
	state.mu.Lock()
	remaining := len(state.pendingMessages)
	state.mu.Unlock()
	if remaining != 1 {
		t.Fatalf("pending after cancellation=%d", remaining)
	}
	records, listErr := store.List("project", "test")
	if listErr != nil || len(records) != 1 || records[0].State != PendingMessageStatePending {
		t.Fatalf("disk pending after cancellation=%#v err=%v", records, listErr)
	}
}

func TestDurableQueueCancellationFailureIsReported(t *testing.T) {
	store, err := NewPendingMessageStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Enqueue(pendingTestRecord("cancel-failure", "queued")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(store.dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.dir, []byte("unavailable"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &durableTestPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	e := newDurableEngine(t, p, &stubAgent{}, store)
	e.cmdStop(p, &Message{SessionKey: "test:room:user", ReplyCtx: "ctx"})
	if sent := p.getSent(); len(sent) != 1 || sent[0] != e.i18n.T(MsgQueueCancellationFailed) {
		t.Fatalf("cancellation failure replies=%v", sent)
	}
	card := e.handleCardNav("act:/stop", "test:room:user")
	if card == nil || !strings.Contains(card.RenderText(), e.i18n.T(MsgQueueCancellationFailed)) {
		t.Fatalf("card cancellation failure not rendered: %#v", card)
	}
}

func TestDurableQueueRequiresReplyContextReconstructor(t *testing.T) {
	store, err := NewPendingMessageStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := &stubPlatformEngine{n: "test"}
	e := newDurableEngine(t, p, &stubAgent{}, store)
	state := &interactiveState{agentSession: &stubAgentSession{}, platform: p}
	e.interactiveStates["raw"] = state
	accepted := e.queueMessageForBusySession(p, &Message{SessionKey: "raw", Platform: "test", MessageID: "m", Content: "x", ReplyCtx: "ctx"}, "raw")
	if !accepted {
		t.Fatal("unsupported durable queue not handled")
	}
	state.mu.Lock()
	queued := len(state.pendingMessages)
	state.mu.Unlock()
	if queued != 0 || len(p.getSent()) != 1 || !strings.Contains(p.getSent()[0], "could not be saved") {
		t.Fatalf("queued=%d replies=%v", queued, p.getSent())
	}
}

func TestDurableRecoveryUsesSavedWorkspace(t *testing.T) {
	store, err := NewPendingMessageStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	record := pendingTestRecord("workspace-message", "workspace prompt")
	record.WorkspaceDir = workspace
	if _, _, err := store.Enqueue(record); err != nil {
		t.Fatal(err)
	}

	sess := newDurableTestSession()
	agentName := "durable-workspace-recovery-agent"
	var factoryMu sync.Mutex
	var factoryWorkDir string
	RegisterAgent(agentName, func(opts map[string]any) (Agent, error) {
		factoryMu.Lock()
		factoryWorkDir, _ = opts["work_dir"].(string)
		factoryMu.Unlock()
		return &durableFactoryAgent{name: agentName, session: sess}, nil
	})
	parent := &durableFactoryAgent{name: agentName, session: &stubAgentSession{}}
	p := &durableTestPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	e := newDurableEngine(t, p, parent, store)
	e.SetMultiWorkspace(t.TempDir(), t.TempDir()+"/bindings.json")
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	defer e.Stop()
	select {
	case call := <-sess.sends:
		if call.prompt != record.Content {
			t.Fatalf("prompt=%q", call.prompt)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("workspace recovery timeout")
	}
	factoryMu.Lock()
	gotWorkDir := factoryWorkDir
	factoryMu.Unlock()
	if gotWorkDir != workspace {
		t.Fatalf("factory work_dir=%q, want saved %q", gotWorkDir, workspace)
	}
	e.interactiveMu.Lock()
	state := e.interactiveStates[workspace+":"+record.SessionKey]
	e.interactiveMu.Unlock()
	if state == nil {
		t.Fatalf("saved workspace interactive key missing")
	}
	state.mu.Lock()
	gotDeliveryKey := state.deliverySessionKey
	state.mu.Unlock()
	if gotDeliveryKey != record.SessionKey {
		t.Fatalf("delivery session key=%q, want %q", gotDeliveryKey, record.SessionKey)
	}
	if sent := waitForPlatformSend(&p.stubPlatformEngine, 1, 3*time.Second); len(sent) == 0 {
		t.Fatal("workspace response timeout")
	}
	if records, _ := store.List("project", "test"); len(records) != 0 {
		t.Fatalf("workspace durable record not completed: %#v", records)
	}
}

func TestDurableIntentionalQueueDropAndResetRemoveRecords(t *testing.T) {
	store, err := NewPendingMessageStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := &durableTestPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	e := newDurableEngine(t, p, &stubAgent{}, store)
	state := &interactiveState{agentSession: &stubAgentSession{}, platform: p}
	e.interactiveStates["raw"] = state
	msg := &Message{SessionKey: "raw", Platform: "test", MessageID: "drop", Content: "drop", ReplyCtx: "ctx", Images: []ImageAttachment{{Data: []byte{1, 2, 3}}}, Files: []FileAttachment{{FileName: "secret.bin", Data: []byte{4, 5, 6}}}}
	if !e.queueMessageForBusySession(p, msg, "raw") {
		t.Fatal("queue failed")
	}
	e.notifyDroppedQueuedMessages(state, errors.New("cancelled"))
	if records, _ := store.List("project", "test"); len(records) != 0 {
		t.Fatalf("drop left records: %#v", records)
	}
	if _, _, err := store.Enqueue(pendingTestRecord("reset", "reset")); err != nil {
		t.Fatal(err)
	}
	e.resetAllSessions()
	if records, _ := store.List("project", "test"); len(records) != 0 {
		t.Fatalf("reset left records: %#v", records)
	}
	newRecord := pendingTestRecord("disk-new", "new")
	newRecord.SessionKey = "disk-only"
	if _, _, err := store.Enqueue(newRecord); err != nil {
		t.Fatal(err)
	}
	e.cmdNew(p, &Message{SessionKey: newRecord.SessionKey, Platform: "test", ReplyCtx: "new-ctx"}, nil)
	if records, _ := store.List("project", "test"); len(records) != 0 {
		t.Fatalf("/new left disk-only records: %#v", records)
	}
}

// Compile-time guard that test helpers do not accidentally classify ordinary
// errors as outcome-unknown failures.
func TestDurableQueueOrdinaryErrorIsKnown(t *testing.T) {
	var outcomeUnknown OutcomeUnknownError
	if errors.As(errors.New("known"), &outcomeUnknown) {
		t.Fatal("ordinary error unexpectedly implements OutcomeUnknownError")
	}
}
