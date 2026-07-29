package core

import (
	"errors"
	"testing"
	"time"
)

type stallTestSession struct{ alive bool }

func (s *stallTestSession) Send(string, []ImageAttachment, []FileAttachment) error { return nil }
func (s *stallTestSession) RespondPermission(string, PermissionResult) error       { return nil }
func (s *stallTestSession) Events() <-chan Event                                   { return nil }
func (s *stallTestSession) CurrentSessionID() string                               { return "stall-test" }
func (s *stallTestSession) Alive() bool                                            { return s.alive }
func (s *stallTestSession) Close() error                                           { s.alive = false; return nil }

func responsiveStallResult(phase StallPhase, cursor string) stallControlResult {
	return stallControlResult{snapshot: StallProbeSnapshot{
		Phase:               phase,
		ProcessAlive:        true,
		TransportResponsive: true,
		Generation:          1,
		Cursor:              cursor,
	}}
}

func TestForegroundStallDetector_HealthySilentAcceptedRunSurvives(t *testing.T) {
	now := time.Unix(100, 0)
	d := newForegroundStallDetector(now, 0, 3, 2)
	s := &stallTestSession{alive: true}
	for i := 0; i < 10; i++ {
		if got := d.decide(now.Add(time.Duration(i+1)*time.Hour), s, responsiveStallResult(StallPhaseRunning, "leaf-1")); got != stallObserveSoft {
			t.Fatalf("probe %d decision=%v, want observe", i, got)
		}
	}
	if !s.Alive() || d.interrupted {
		t.Fatalf("healthy silent run damaged: alive=%v interrupted=%v", s.Alive(), d.interrupted)
	}
}

func TestForegroundStallDetector_DurableCursorProgressResetsRecovery(t *testing.T) {
	now := time.Unix(200, 0)
	d := newForegroundStallDetector(now, time.Minute, 2, 2)
	s := &stallTestSession{alive: true}
	_ = d.decide(now.Add(30*time.Second), s, responsiveStallResult(StallPhaseTool, "leaf-1"))
	if got := d.decide(now.Add(2*time.Minute), s, responsiveStallResult(StallPhaseTool, "leaf-2")); got != stallObserveSoft {
		t.Fatalf("cursor progress decision=%v", got)
	}
	if d.inGrace || !d.progressAt.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("progress did not reset detector: grace=%v progress=%v", d.inGrace, d.progressAt)
	}
}

func TestForegroundStallDetector_DeadProcessTerminalImmediately(t *testing.T) {
	d := newForegroundStallDetector(time.Now(), 0, 3, 2)
	s := &stallTestSession{alive: false}
	if got := d.decide(time.Now(), s, stallControlResult{}); got != stallTerminalTransport {
		t.Fatalf("decision=%v, want terminal transport", got)
	}
}

func TestForegroundStallDetector_FailedProbesGraceInterruptOnceThenClose(t *testing.T) {
	now := time.Unix(300, 0)
	d := newForegroundStallDetector(now, 0, 2, 2)
	s := &stallTestSession{alive: true}
	_ = d.decide(now, s, responsiveStallResult(StallPhaseRunning, "leaf"))
	failed := stallControlResult{err: errors.New("probe timeout")}
	if got := d.decide(now.Add(time.Second), s, failed); got != stallObserveSoon {
		t.Fatalf("first failure=%v", got)
	}
	if got := d.decide(now.Add(2*time.Second), s, failed); got != stallStartInterrupt {
		t.Fatalf("grace entry=%v, want interrupt", got)
	}
	if got := d.decide(now.Add(3*time.Second), s, failed); got != stallObserveSoon {
		t.Fatalf("first grace probe=%v, want observe", got)
	}
	if got := d.decide(now.Add(4*time.Second), s, failed); got != stallTerminalWedge {
		t.Fatalf("grace exhaustion=%v, want terminal", got)
	}
	if !d.interrupted {
		t.Fatal("interrupt not recorded")
	}
	if d.safeToInterrupt() {
		t.Fatal("second interrupt permitted")
	}
}

func TestForegroundStallDetector_TemporaryProbeFailureRecovers(t *testing.T) {
	now := time.Unix(400, 0)
	d := newForegroundStallDetector(now, 0, 3, 2)
	s := &stallTestSession{alive: true}
	if got := d.decide(now, s, stallControlResult{err: errors.New("temporary")}); got != stallObserveSoon {
		t.Fatalf("failure=%v", got)
	}
	if got := d.decide(now.Add(time.Second), s, responsiveStallResult(StallPhaseStreaming, "advanced")); got != stallObserveSoft {
		t.Fatalf("recovery=%v", got)
	}
	if d.failures != 0 || d.inGrace || d.interrupted {
		t.Fatalf("recovery state failures=%d grace=%v interrupted=%v", d.failures, d.inGrace, d.interrupted)
	}
}

func TestForegroundStallDetector_HardCeilingPolicy(t *testing.T) {
	now := time.Unix(500, 0)
	s := &stallTestSession{alive: true}
	disabled := newForegroundStallDetector(now, 0, 3, 2)
	if got := disabled.decide(now.Add(24*time.Hour), s, responsiveStallResult(StallPhaseRunning, "same")); got != stallObserveSoft {
		t.Fatalf("disabled ceiling decision=%v", got)
	}
	enabled := newForegroundStallDetector(now, time.Hour, 3, 2)
	_ = enabled.decide(now, s, responsiveStallResult(StallPhaseRunning, "same"))
	if got := enabled.decide(now.Add(time.Hour), s, responsiveStallResult(StallPhaseRunning, "same")); got != stallStartInterrupt {
		t.Fatalf("enabled ceiling decision=%v, want interrupt", got)
	}
}

func TestForegroundStallDetector_AwaitingAcceptanceNeverInterruptedOrClosed(t *testing.T) {
	now := time.Unix(600, 0)
	d := newForegroundStallDetector(now, time.Minute, 2, 2)
	s := &stallTestSession{alive: true}
	awaiting := responsiveStallResult(StallPhaseAwaitingAcceptance, "same")
	awaiting.snapshot.PromptPending = true
	for i := 0; i < 5; i++ {
		if got := d.decide(now.Add(time.Duration(i)*time.Hour), s, awaiting); got != stallObserveSoon {
			t.Fatalf("responsive awaiting decision=%v", got)
		}
		failed := stallControlResult{snapshot: StallProbeSnapshot{Phase: StallPhaseAwaitingAcceptance, ProcessAlive: true, PromptPending: true}, err: errors.New("probe timeout")}
		if got := d.decide(now.Add(time.Duration(i)*time.Hour), s, failed); got != stallObserveSoon {
			t.Fatalf("failed awaiting decision=%v", got)
		}
	}
	if d.interrupted || d.inGrace {
		t.Fatalf("unknown prompt acceptance changed: interrupted=%v grace=%v", d.interrupted, d.inGrace)
	}
}

func TestForegroundStallDetector_ProtocolActivityIsProgress(t *testing.T) {
	now := time.Unix(650, 0)
	d := newForegroundStallDetector(now, time.Minute, 2, 2)
	s := &stallTestSession{alive: true}
	first := responsiveStallResult(StallPhaseTool, "same")
	first.snapshot.ActivitySeq = 10
	_ = d.decide(now, s, first)
	second := first
	second.snapshot.ActivitySeq = 11
	if got := d.decide(now.Add(2*time.Minute), s, second); got != stallObserveSoft {
		t.Fatalf("activity progress decision=%v", got)
	}
	if !d.progressAt.Equal(now.Add(2 * time.Minute)) {
		t.Fatalf("activity did not reset hard clock: %v", d.progressAt)
	}
}

func TestForegroundStallDetector_UnresponsiveWithoutCursorDoesNotAuthorizeRecovery(t *testing.T) {
	now := time.Unix(675, 0)
	d := newForegroundStallDetector(now, 0, 2, 2)
	s := &stallTestSession{alive: true}
	result := stallControlResult{snapshot: StallProbeSnapshot{Phase: StallPhaseRunning, ProcessAlive: true}}
	for i := 0; i < 4; i++ {
		if got := d.decide(now.Add(time.Duration(i)*time.Second), s, result); got != stallObserveSoon {
			t.Fatalf("probe %d decision=%v", i, got)
		}
	}
	if d.interrupted || d.inGrace {
		t.Fatalf("unobserved failure authorized recovery: interrupted=%v grace=%v", d.interrupted, d.inGrace)
	}
}

func TestForegroundStallDetector_UnsupportedFallbackExplicit(t *testing.T) {
	now := time.Unix(700, 0)
	s := &stallTestSession{alive: true}
	d := newForegroundStallDetector(now, 0, 2, 2)
	for i := 0; i < 5; i++ {
		if got := d.decide(now.Add(time.Duration(i)*time.Hour), s, stallControlResult{err: ErrNotSupported}); got != stallObserveSoft {
			t.Fatalf("unsupported decision=%v", got)
		}
	}
	d = newForegroundStallDetector(now, time.Hour, 2, 2)
	if got := d.decide(now.Add(24*time.Hour), s, stallControlResult{err: ErrNotSupported}); got != stallObserveSoft {
		t.Fatalf("unsupported fallback decision=%v", got)
	}
}
