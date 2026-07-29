package core

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeWatchdogClock struct {
	mu      sync.Mutex
	now     time.Time
	timers  []*fakeWatchdogTimer
	created chan struct{}
}

type fakeWatchdogTimer struct {
	clock    *fakeWatchdogClock
	deadline time.Time
	active   bool
	ch       chan time.Time
}

func newFakeWatchdogClock(now time.Time) *fakeWatchdogClock {
	return &fakeWatchdogClock{now: now, created: make(chan struct{}, 16)}
}

func (c *fakeWatchdogClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeWatchdogClock) NewTimer(d time.Duration) watchdogTimer {
	c.mu.Lock()
	t := &fakeWatchdogTimer{clock: c, deadline: c.now.Add(d), active: true, ch: make(chan time.Time, 1)}
	c.timers = append(c.timers, t)
	c.mu.Unlock()
	c.created <- struct{}{}
	return t
}

func (t *fakeWatchdogTimer) Ch() <-chan time.Time { return t.ch }
func (t *fakeWatchdogTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	wasActive := t.active
	t.active = false
	return wasActive
}
func (t *fakeWatchdogTimer) Reset(d time.Duration) bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	wasActive := t.active
	t.deadline = t.clock.now.Add(d)
	t.active = true
	return wasActive
}
func (c *fakeWatchdogClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	for _, t := range c.timers {
		if t.active && !t.deadline.After(c.now) {
			t.active = false
			select {
			case t.ch <- c.now:
			default:
			}
		}
	}
	c.mu.Unlock()
}
func (c *fakeWatchdogClock) waitForInitialTimer(t *testing.T) {
	t.Helper()
	select {
	case <-c.created:
	case <-time.After(time.Second):
		t.Fatal("watchdog did not create timer")
	}
}

type watchdogProbeStep struct {
	snapshot StallProbeSnapshot
	err      error
	block    <-chan struct{}
}

type watchdogSession struct {
	events     chan Event
	alive      atomic.Bool
	closed     atomic.Int32
	interrupts atomic.Int32
	mu         sync.Mutex
	steps      []watchdogProbeStep
	probes     int
}

func newWatchdogSession(steps ...watchdogProbeStep) *watchdogSession {
	s := &watchdogSession{events: make(chan Event, 16), steps: steps}
	s.alive.Store(true)
	return s
}
func (s *watchdogSession) Send(string, []ImageAttachment, []FileAttachment) error { return nil }
func (s *watchdogSession) RespondPermission(string, PermissionResult) error       { return nil }
func (s *watchdogSession) Events() <-chan Event                                   { return s.events }
func (s *watchdogSession) CurrentSessionID() string                               { return "watchdog" }
func (s *watchdogSession) Alive() bool                                            { return s.alive.Load() }
func (s *watchdogSession) Close() error {
	s.closed.Add(1)
	s.alive.Store(false)
	return nil
}
func (s *watchdogSession) ProbeStall(ctx context.Context) (StallProbeSnapshot, error) {
	s.mu.Lock()
	i := s.probes
	s.probes++
	var step watchdogProbeStep
	if len(s.steps) > 0 {
		if i >= len(s.steps) {
			i = len(s.steps) - 1
		}
		step = s.steps[i]
	}
	s.mu.Unlock()
	if step.block != nil {
		select {
		case <-step.block:
		case <-ctx.Done():
			return StallProbeSnapshot{}, ctx.Err()
		}
	}
	return step.snapshot, step.err
}
func (s *watchdogSession) InterruptStall(context.Context) error {
	s.interrupts.Add(1)
	return nil
}

func healthyWatchdogSnapshot(cursor string) StallProbeSnapshot {
	return StallProbeSnapshot{Phase: StallPhaseRunning, ProcessAlive: true, TransportResponsive: true, Generation: 1, PID: 123, Cursor: cursor}
}

func runWatchdogLoop(t *testing.T, e *Engine, p Platform, s AgentSession, key string, pending []queuedMessage) (*interactiveState, chan struct{}) {
	t.Helper()
	state := &interactiveState{agentSession: s, platform: p, replyCtx: "active", pendingMessages: pending}
	e.interactiveMu.Lock()
	e.interactiveStates[key] = state
	e.interactiveMu.Unlock()
	session := e.sessions.GetOrCreateActive(key)
	if !session.TryLock() {
		t.Fatal("lock session")
	}
	done := make(chan struct{})
	go func() {
		e.processInteractiveEvents(state, session, e.sessions, key, "", time.Now(), nil, nil, nil)
		close(done)
	}()
	return state, done
}

func TestForegroundWatchdog_HealthySilentProbeKeepsParentAndQueue(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	s := newWatchdogSession(watchdogProbeStep{snapshot: healthyWatchdogSnapshot("leaf")})
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	clock := newFakeWatchdogClock(time.Unix(1_000, 0))
	e.setStallWatchdogClock(clock)
	e.SetEventIdleTimeout(4 * time.Hour)
	e.setStallWatchdogTimings(time.Second, time.Minute, 2, 2)
	state, done := runWatchdogLoop(t, e, p, s, "test:healthy", []queuedMessage{{platform: p, replyCtx: "queued", content: "later"}})
	clock.waitForInitialTimer(t)
	clock.Advance(4 * time.Hour)
	deadline := time.Now().Add(time.Second)
	for {
		s.mu.Lock()
		probes := s.probes
		s.mu.Unlock()
		if probes > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("healthy silent operation was not probed")
		}
		time.Sleep(time.Millisecond)
	}
	if s.closed.Load() != 0 || s.interrupts.Load() != 0 {
		t.Fatalf("healthy session changed: closed=%d interrupts=%d", s.closed.Load(), s.interrupts.Load())
	}
	state.mu.Lock()
	queued := len(state.pendingMessages)
	state.mu.Unlock()
	if queued != 1 {
		t.Fatalf("queue drained during soft observation: %d", queued)
	}
	// Settlement must drain preserved queue through normal dispatch exactly once.
	s.events <- Event{Type: EventResult, Content: "done", Done: true}
	deadline = time.Now().Add(time.Second)
	for {
		state.mu.Lock()
		queued = len(state.pendingMessages)
		state.mu.Unlock()
		if queued == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("queued message was not dispatched after recovery")
		}
		time.Sleep(time.Millisecond)
	}
	s.events <- Event{Type: EventResult, Content: "queued done", Done: true}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("did not settle queued turn")
	}
}

func TestForegroundWatchdog_SlowPromptAcceptanceIgnoresHardCeiling(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	awaiting := healthyWatchdogSnapshot("same")
	awaiting.Phase = StallPhaseAwaitingAcceptance
	awaiting.PromptPending = true
	s := newWatchdogSession(watchdogProbeStep{snapshot: awaiting})
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.SetEventIdleTimeout(5 * time.Millisecond)
	e.SetHardStallTimeout(5 * time.Millisecond)
	e.setStallWatchdogTimings(10*time.Millisecond, 2*time.Millisecond, 2, 2)
	_, done := runWatchdogLoop(t, e, p, s, "test:slow-ack", nil)
	time.Sleep(30 * time.Millisecond)
	if s.closed.Load() != 0 || s.interrupts.Load() != 0 {
		t.Fatalf("slow ACK altered: close=%d interrupt=%d", s.closed.Load(), s.interrupts.Load())
	}
	s.events <- Event{Type: EventResult, Content: "accepted and done", Done: true}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("slow ACK session did not settle")
	}
}

func TestForegroundWatchdog_TemporaryProbeFailureRecoversWithoutMutation(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	s := newWatchdogSession(
		watchdogProbeStep{err: errors.New("temporary timeout")},
		watchdogProbeStep{snapshot: healthyWatchdogSnapshot("advanced")},
	)
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.SetEventIdleTimeout(5 * time.Millisecond)
	e.setStallWatchdogTimings(10*time.Millisecond, 2*time.Millisecond, 3, 2)
	_, done := runWatchdogLoop(t, e, p, s, "test:probe-recovery", nil)
	deadline := time.Now().Add(time.Second)
	for {
		s.mu.Lock()
		probes := s.probes
		s.mu.Unlock()
		if probes >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("recovery probe did not run")
		}
		time.Sleep(time.Millisecond)
	}
	s.events <- Event{Type: EventResult, Content: "done", Done: true}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("recovered session did not settle")
	}
	if s.closed.Load() != 0 || s.interrupts.Load() != 0 {
		t.Fatalf("temporary failure caused mutation: close=%d interrupt=%d", s.closed.Load(), s.interrupts.Load())
	}
}

func TestForegroundWatchdog_ProcessDeadAtThresholdCleansOnce(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	s := newWatchdogSession()
	s.alive.Store(false)
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.SetEventIdleTimeout(10 * time.Millisecond)
	_, done := runWatchdogLoop(t, e, p, s, "test:dead", nil)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("dead process not cleaned")
	}
	if s.closed.Load() != 1 {
		t.Fatalf("close count=%d", s.closed.Load())
	}
}

func TestForegroundWatchdog_ProbeGraceInterruptAndQueuedCleanupExactlyOnce(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	s := newWatchdogSession(
		watchdogProbeStep{snapshot: healthyWatchdogSnapshot("same")},
		watchdogProbeStep{err: errors.New("probe timeout")},
	)
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.SetEventIdleTimeout(10 * time.Millisecond)
	e.setStallWatchdogTimings(10*time.Millisecond, 3*time.Millisecond, 2, 2)
	pending := []queuedMessage{{platform: p, replyCtx: "q1", content: "one"}, {platform: p, replyCtx: "q2", content: "two"}}
	_, done := runWatchdogLoop(t, e, p, s, "test:wedge", pending)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("wedge recovery did not terminate")
	}
	if s.interrupts.Load() != 1 || s.closed.Load() != 1 {
		t.Fatalf("interrupts=%d closes=%d", s.interrupts.Load(), s.closed.Load())
	}
	sent := p.getSent()
	resets := 0
	verified := 0
	for _, msg := range sent {
		if strings.Contains(msg, "session reset") {
			resets++
		}
		if strings.Contains(msg, "recoverable progress") {
			verified++
		}
	}
	if resets != 2 || verified != 1 {
		t.Fatalf("sent=%v; resets=%d verified=%d", sent, resets, verified)
	}
}

func TestForegroundWatchdog_BufferedSettlementWinsTerminalProbe(t *testing.T) {
	for i := 0; i < 30; i++ {
		p := &stubPlatformEngine{n: "test"}
		block := make(chan struct{})
		s := newWatchdogSession(watchdogProbeStep{block: block, snapshot: StallProbeSnapshot{Phase: StallPhaseProcessDead}})
		e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
		e.SetEventIdleTimeout(2 * time.Millisecond)
		e.setStallWatchdogTimings(50*time.Millisecond, time.Millisecond, 2, 2)
		_, done := runWatchdogLoop(t, e, p, s, "test:terminal-race", nil)
		deadline := time.Now().Add(time.Second)
		for {
			s.mu.Lock()
			probes := s.probes
			s.mu.Unlock()
			if probes > 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("probe did not start")
			}
			time.Sleep(time.Millisecond)
		}
		s.events <- Event{Type: EventResult, Content: "settled", Done: true}
		close(block)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("settlement/terminal race did not finish")
		}
		if s.closed.Load() != 0 {
			t.Fatalf("iteration %d terminal probe beat buffered settlement", i)
		}
	}
}

func TestForegroundWatchdog_CallerCancellationDuringProbeDoesNotDeadlock(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	block := make(chan struct{})
	s := newWatchdogSession(watchdogProbeStep{block: block, snapshot: healthyWatchdogSnapshot("leaf")})
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.SetEventIdleTimeout(5 * time.Millisecond)
	e.setStallWatchdogTimings(time.Second, 5*time.Millisecond, 2, 2)
	_, done := runWatchdogLoop(t, e, p, s, "test:cancel-probe", nil)
	time.Sleep(15 * time.Millisecond)
	e.cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("engine cancellation deadlocked behind probe")
	}
	if s.closed.Load() != 0 || s.interrupts.Load() != 0 {
		t.Fatalf("cancellation caused recovery mutation: close=%d interrupt=%d", s.closed.Load(), s.interrupts.Load())
	}
}

func TestForegroundWatchdog_ResultRacesBlockedProbeWithoutCleanup(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	block := make(chan struct{})
	s := newWatchdogSession(watchdogProbeStep{block: block, snapshot: healthyWatchdogSnapshot("leaf")})
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.SetEventIdleTimeout(10 * time.Millisecond)
	e.setStallWatchdogTimings(100*time.Millisecond, 5*time.Millisecond, 2, 2)
	_, done := runWatchdogLoop(t, e, p, s, "test:race", nil)
	time.Sleep(20 * time.Millisecond)
	s.events <- Event{Type: EventResult, Content: "settled", Done: true}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("result lost behind probe")
	}
	if s.closed.Load() != 0 || s.interrupts.Load() != 0 {
		t.Fatalf("settlement raced cleanup: close=%d interrupt=%d", s.closed.Load(), s.interrupts.Load())
	}
}
