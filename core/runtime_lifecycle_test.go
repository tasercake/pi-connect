package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type lifecycleTestPlatform struct {
	name        string
	handler     PlatformLifecycleHandler
	mu          sync.Mutex
	attempts    int
	failures    int
	sent        []string
	reconstruct []string
	attemptCh   chan int
	release     chan struct{}
}

func (p *lifecycleTestPlatform) Name() string               { return p.name }
func (p *lifecycleTestPlatform) Start(MessageHandler) error { return nil }
func (p *lifecycleTestPlatform) Reply(ctx context.Context, replyCtx any, content string) error {
	return p.Send(ctx, replyCtx, content)
}
func (p *lifecycleTestPlatform) Send(_ context.Context, _ any, content string) error {
	p.mu.Lock()
	p.attempts++
	attempt := p.attempts
	fail := p.failures > 0
	if fail {
		p.failures--
	} else {
		p.sent = append(p.sent, content)
	}
	p.mu.Unlock()
	if p.attemptCh != nil {
		p.attemptCh <- attempt
	}
	if fail {
		return errors.New("temporarily unavailable")
	}
	if p.release != nil {
		select {
		case <-p.release:
		default:
		}
	}
	return nil
}
func (p *lifecycleTestPlatform) Stop() error                                    { return nil }
func (p *lifecycleTestPlatform) SetLifecycleHandler(h PlatformLifecycleHandler) { p.handler = h }
func (p *lifecycleTestPlatform) ReconstructReplyCtx(key string) (any, error) {
	p.mu.Lock()
	p.reconstruct = append(p.reconstruct, key)
	p.mu.Unlock()
	return "ctx:" + key, nil
}
func (p *lifecycleTestPlatform) ready() { p.handler.OnPlatformReady(p) }

func TestRuntimeCrashLeaseRecoveryWaitsRetriesAcksAndDedupes(t *testing.T) {
	dataDir := t.TempDir()
	first, err := NewRuntimeLifecycleStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, identity := range []string{"worker-a", "worker-b"} {
		if err := first.UpsertLease(RuntimeLease{
			ID: first.NewLeaseID("project-a", identity), Project: "project-a", Platform: "async", SessionKey: "async:chat:user",
			AgentSessionID: "saved-session", OperationID: "turn-1", TurnInFlight: true,
		}); err != nil {
			t.Fatal(err)
		}
	}

	recovered, err := NewRuntimeLifecycleStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	alerts := recovered.pendingAlerts("project-a", "async")
	if len(alerts) != 1 || alerts[0].LostProcesses != 2 || alerts[0].ActiveTurns != 2 || !alerts[0].OutcomeUnknown {
		t.Fatalf("recovered alerts = %+v, want one coalesced alert", alerts)
	}

	oldInitial, oldMax := lifecycleRetryInitial, lifecycleRetryMax
	lifecycleRetryInitial, lifecycleRetryMax = 20*time.Millisecond, 40*time.Millisecond
	defer func() { lifecycleRetryInitial, lifecycleRetryMax = oldInitial, oldMax }()

	p := &lifecycleTestPlatform{name: "async", failures: 1, attemptCh: make(chan int, 4)}
	e := NewEngine("project-a", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.SetRuntimeLifecycleStore(recovered)
	defer e.Stop()
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case attempt := <-p.attemptCh:
		t.Fatalf("delivery attempted before readiness: %d", attempt)
	case <-time.After(50 * time.Millisecond):
	}

	p.ready()
	select {
	case <-p.attemptCh:
	case <-time.After(time.Second):
		t.Fatal("first delivery attempt missing")
	}
	if got := recovered.pendingAlerts("project-a", "async"); len(got) != 1 {
		t.Fatalf("failed delivery cleared alert: %+v", got)
	}
	select {
	case <-p.attemptCh:
	case <-time.After(time.Second):
		t.Fatal("retry delivery attempt missing")
	}
	deadline := time.Now().Add(time.Second)
	for len(recovered.pendingAlerts("project-a", "async")) != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := recovered.pendingAlerts("project-a", "async"); len(got) != 0 {
		t.Fatalf("successful delivery not acknowledged: %+v", got)
	}
	p.mu.Lock()
	attempts, sent, reconstructed := p.attempts, append([]string(nil), p.sent...), append([]string(nil), p.reconstruct...)
	p.mu.Unlock()
	if attempts != 2 || len(sent) != 1 || len(reconstructed) != 2 || reconstructed[0] != "async:chat:user" {
		t.Fatalf("attempts=%d sent=%v reconstructed=%v", attempts, sent, reconstructed)
	}
	if got := recovered.state.Leases; len(got) != 0 {
		t.Fatalf("stale leases not consumed: %+v", got)
	}
	third, err := NewRuntimeLifecycleStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := third.pendingAlerts("project-a", "async"); len(got) != 0 {
		t.Fatalf("acked alert duplicated after restart: %+v", got)
	}
}

func TestRuntimeLifecycleAlertsRemainProjectScoped(t *testing.T) {
	dataDir := t.TempDir()
	first, err := NewRuntimeLifecycleStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.UpsertLease(RuntimeLease{ID: first.NewLeaseID("project-a", "worker"), Project: "project-a", Platform: "shared", SessionKey: "shared:chat:user"}); err != nil {
		t.Fatal(err)
	}
	recovered, err := NewRuntimeLifecycleStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	wrong := &lifecycleTestPlatform{name: "shared", attemptCh: make(chan int, 1)}
	wrongEngine := NewEngine("project-b", &stubAgent{}, []Platform{wrong}, "", LangEnglish)
	wrongEngine.SetRuntimeLifecycleStore(recovered)
	defer wrongEngine.Stop()
	if err := wrongEngine.Start(); err != nil {
		t.Fatal(err)
	}
	wrong.ready()
	select {
	case attempt := <-wrong.attemptCh:
		t.Fatalf("alert crossed project boundary on attempt %d", attempt)
	case <-time.After(50 * time.Millisecond):
	}
	if alerts := recovered.pendingAlerts("project-a", "shared"); len(alerts) != 1 {
		t.Fatalf("project-a alert lost: %+v", alerts)
	}
}

func TestRuntimeImportsLegacyRestartAsReady(t *testing.T) {
	dataDir := t.TempDir()
	runDir := filepath.Join(dataDir, "run")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := []byte(`{"project":"p","platform":"async","session_key":"async:one"}`)
	if err := os.WriteFile(filepath.Join(runDir, "restart_notify"), legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewRuntimeLifecycleStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	alert, ok := store.claimNextAlert("p", "async")
	if !ok || alert.Kind != "restart" {
		t.Fatalf("legacy alert not ready in importing run: alert=%+v ok=%v", alert, ok)
	}
	store.releaseAlert(alert.ID)
	if _, err := os.Stat(filepath.Join(runDir, "restart_notify")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy marker not removed after durable import: %v", err)
	}
}

func TestRuntimeExplicitRestartAlertSurvivesUntilReadyAndSuccess(t *testing.T) {
	dataDir := t.TempDir()
	beforeRestart, err := NewRuntimeLifecycleStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := beforeRestart.EnqueueRestart(RestartRequest{Project: "p", Platform: "async", SessionKey: "async:one"}); err != nil {
		t.Fatal(err)
	}
	if alert, ok := beforeRestart.claimNextAlert("p", "async"); ok {
		t.Fatalf("restart success became deliverable before restart: %+v", alert)
	}
	store, err := NewRuntimeLifecycleStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	p := &lifecycleTestPlatform{name: "async", attemptCh: make(chan int, 1)}
	e := NewEngine("p", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.SetRuntimeLifecycleStore(store)
	defer e.Stop()
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	if len(store.pendingAlerts("p", "async")) != 1 {
		t.Fatal("restart alert missing before readiness")
	}
	p.ready()
	select {
	case <-p.attemptCh:
	case <-time.After(time.Second):
		t.Fatal("restart alert not delivered after readiness")
	}
	deadline := time.Now().Add(time.Second)
	for len(store.pendingAlerts("p", "async")) != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(store.pendingAlerts("p", "async")) != 0 {
		t.Fatal("restart alert not acknowledged")
	}
}

type blockingCloseSession struct {
	*controllableAgentSession
	entered chan struct{}
	release chan struct{}
}

func (s *blockingCloseSession) Close() error {
	close(s.entered)
	<-s.release
	return s.controllableAgentSession.Close()
}

func TestRuntimeLifecyclePersistFailuresKeepInMemoryEvidence(t *testing.T) {
	dataDir := t.TempDir()
	store, err := NewRuntimeLifecycleStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	lease := RuntimeLease{ID: store.NewLeaseID("p", "s"), Project: "p", Platform: "test", SessionKey: "test:s"}
	if err := store.UpsertLease(lease); err != nil {
		t.Fatal(err)
	}
	store.path = filepath.Join(dataDir, "missing", "runtime_lifecycle.json")
	if err := store.RemoveLease(lease.ID); err == nil {
		t.Fatal("RemoveLease succeeded with unavailable persistence path")
	}
	if _, ok := store.state.Leases[lease.ID]; !ok {
		t.Fatal("failed durable lease removal discarded in-memory evidence")
	}

	alert := lifecycleAlert{ID: "alert", Kind: "crash", Project: "p", Platform: "test", SessionKey: "test:s"}
	store.state.Alerts[alert.ID] = alert
	store.claims[alert.ID] = true
	if err := store.ackAlert(alert.ID); err == nil {
		t.Fatal("ackAlert succeeded with unavailable persistence path")
	}
	if _, ok := store.state.Alerts[alert.ID]; !ok || !store.claims[alert.ID] {
		t.Fatal("failed durable acknowledgement discarded pending alert or claim")
	}
}

func TestRuntimeCleanEngineStopRemovesLeaseBeforeSlowProcessClose(t *testing.T) {
	dataDir := t.TempDir()
	store, err := NewRuntimeLifecycleStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("project", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.SetRuntimeLifecycleStore(store)
	sess := &blockingCloseSession{controllableAgentSession: newControllableSession("saved"), entered: make(chan struct{}), release: make(chan struct{})}
	state := &interactiveState{
		agentSession: sess, platform: p, deliverySessionKey: "test:chat:user", leaseStartedAt: time.Now().UTC(),
		runtimeLeaseID: store.NewLeaseID("project", "interactive"), turnInFlight: true, operationID: "turn",
	}
	e.interactiveStates["interactive"] = state
	e.persistRuntimeLease(state)
	stopped := make(chan error, 1)
	go func() { stopped <- e.Stop() }()
	select {
	case <-sess.entered:
	case <-time.After(time.Second):
		t.Fatal("agent close did not start")
	}
	store.mu.Lock()
	leaseCount := len(store.state.Leases)
	store.mu.Unlock()
	if leaseCount != 0 {
		t.Fatalf("lease retained during intentional slow close: %d", leaseCount)
	}
	close(sess.release)
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeCleanEngineStopRemovesLeaseWithoutCrashAlert(t *testing.T) {
	dataDir := t.TempDir()
	store, err := NewRuntimeLifecycleStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("project", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.SetRuntimeLifecycleStore(store)
	sess := newControllableSession("saved")
	state := &interactiveState{
		agentSession: sess, platform: p, deliverySessionKey: "test:chat:user", leaseStartedAt: time.Now().UTC(),
		runtimeLeaseID: store.NewLeaseID("project", "interactive"), turnInFlight: true, operationID: "turn",
	}
	e.interactiveStates["interactive"] = state
	e.persistRuntimeLease(state)
	if err := e.Stop(); err != nil {
		t.Fatal(err)
	}
	next, err := NewRuntimeLifecycleStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if alerts := next.pendingAlerts("project", "test"); len(alerts) != 0 {
		t.Fatalf("clean shutdown produced crash alert: %+v", alerts)
	}
}

type countingStartAgent struct{ starts atomic.Int32 }

func (a *countingStartAgent) Name() string { return "counting" }
func (a *countingStartAgent) StartSession(context.Context, string) (AgentSession, error) {
	a.starts.Add(1)
	return nil, errors.New("must not replay")
}
func (a *countingStartAgent) ListSessions(context.Context) ([]AgentSessionInfo, error) {
	return nil, nil
}
func (a *countingStartAgent) Stop() error { return nil }

func TestRuntimeLifecycleCorruptFilesDoNotBlockStartup(t *testing.T) {
	dataDir := t.TempDir()
	runDir := filepath.Join(dataDir, "run")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "runtime_lifecycle.json"), []byte("{truncated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "restart_notify"), []byte("{truncated"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewRuntimeLifecycleStore(dataDir)
	if err != nil {
		t.Fatalf("corrupt lifecycle migration blocked startup: %v", err)
	}
	if len(store.state.Leases) != 0 || len(store.state.Alerts) != 0 {
		t.Fatalf("corrupt state was treated as valid: %+v", store.state)
	}
	matches, err := filepath.Glob(filepath.Join(runDir, "*.corrupt.*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 {
		t.Fatalf("quarantined files = %v, want 2", matches)
	}
}

func TestRuntimeRecoveryNeverReplaysOutcomeUnknownPrompt(t *testing.T) {
	dataDir := t.TempDir()
	first, _ := NewRuntimeLifecycleStore(dataDir)
	_ = first.UpsertLease(RuntimeLease{ID: first.NewLeaseID("p", "s"), Project: "p", Platform: "async", SessionKey: "async:s", TurnInFlight: true, OutcomeUnknown: true, OperationID: "op"})
	recovered, err := NewRuntimeLifecycleStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	agent := &countingStartAgent{}
	p := &lifecycleTestPlatform{name: "async", attemptCh: make(chan int, 1)}
	e := NewEngine("p", agent, []Platform{p}, "", LangEnglish)
	e.SetRuntimeLifecycleStore(recovered)
	defer e.Stop()
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	p.ready()
	select {
	case <-p.attemptCh:
	case <-time.After(time.Second):
		t.Fatal("recovery alert not delivered")
	}
	if got := agent.starts.Load(); got != 0 {
		t.Fatalf("recovery replayed prompt via %d agent starts", got)
	}
}
