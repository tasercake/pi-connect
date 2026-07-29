package core

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// watchdogClock is deliberately narrow so foreground watchdog tests can
// advance soft/grace deadlines without waiting in wall time. Production uses
// realWatchdogClock; probe RPC deadlines remain context-bound real deadlines.
type watchdogClock interface {
	Now() time.Time
	NewTimer(time.Duration) watchdogTimer
}

type watchdogTimer interface {
	Ch() <-chan time.Time
	Stop() bool
	Reset(time.Duration) bool
}

type realWatchdogClock struct{}

func (realWatchdogClock) Now() time.Time { return time.Now() }
func (realWatchdogClock) NewTimer(d time.Duration) watchdogTimer {
	return realWatchdogTimer{Timer: time.NewTimer(d)}
}

type realWatchdogTimer struct{ *time.Timer }

func (t realWatchdogTimer) Ch() <-chan time.Time { return t.C }

type stallControlKind uint8

const (
	stallControlProbe stallControlKind = iota
	stallControlInterrupt
)

type stallControlResult struct {
	kind     stallControlKind
	seq      uint64
	snapshot StallProbeSnapshot
	err      error
	latency  time.Duration
}

type stallDecision uint8

const (
	stallObserveSoft stallDecision = iota
	stallObserveSoon
	stallStartInterrupt
	stallTerminalTransport
	stallTerminalWedge
)

type foregroundStallDetector struct {
	hardTimeout  time.Duration
	maxFailures  int
	graceProbes  int
	progressAt   time.Time
	cursor       string
	generation   uint64
	activitySeq  uint64
	observed     bool
	failures     int
	inGrace      bool
	graceSeen    int
	interrupted  bool
	attempt      int
	lastSnapshot StallProbeSnapshot
}

func newForegroundStallDetector(now time.Time, hard time.Duration, maxFailures, graceProbes int) *foregroundStallDetector {
	return &foregroundStallDetector{
		hardTimeout: hard,
		maxFailures: maxFailures,
		graceProbes: graceProbes,
		progressAt:  now,
	}
}

func (d *foregroundStallDetector) eventProgress(now time.Time) {
	d.progressAt = now
	d.failures = 0
	d.inGrace = false
	d.graceSeen = 0
}

func (d *foregroundStallDetector) beginOperation(now time.Time) {
	d.eventProgress(now)
	d.interrupted = false
	d.observed = false
	d.lastSnapshot = StallProbeSnapshot{}
}

func (d *foregroundStallDetector) decide(now time.Time, session AgentSession, result stallControlResult) stallDecision {
	if session == nil || !session.Alive() {
		return stallTerminalTransport
	}
	if errors.Is(result.err, ErrNotSupported) {
		// Explicit fallback: a non-probing adapter has no responsive control
		// plane or durable cursor evidence. Alive is checked at every soft
		// threshold, but neither the optional hard ceiling nor silence may
		// authorize teardown; only an observed dead process/channel can do so.
		return stallObserveSoft
	}
	if result.snapshot.Phase == StallPhaseProcessDead || result.snapshot.Phase == StallPhaseTransportBroken {
		return stallTerminalTransport
	}
	if result.err != nil {
		if result.snapshot.PromptPending || result.snapshot.Phase == StallPhaseAwaitingAcceptance {
			// PR #39: a written prompt with no ACK has unknown acceptance and an
			// intentionally unbounded wait. Probe failure cannot reject, abort, or
			// close it; only verified process/transport loss is terminal.
			d.lastSnapshot = result.snapshot
			d.failures = 0
			d.inGrace = false
			d.graceSeen = 0
			return stallObserveSoon
		}
		if result.snapshot.Phase != "" {
			d.lastSnapshot = result.snapshot
		}
		// Failed control calls alone are not proof that a still-live operation
		// is wedged. Require one prior responsive durable observation before
		// a failure sequence can authorize interrupt/teardown; otherwise a
		// startup/network partition would recreate the old silence-only kill.
		if !d.observed {
			return stallObserveSoon
		}
		d.failures++
		if !d.inGrace && d.failures >= d.maxFailures {
			d.inGrace = true
			d.graceSeen = 0
			if d.safeToInterrupt() {
				d.interrupted = true
				return stallStartInterrupt
			}
		}
		if d.inGrace {
			d.graceSeen++
			if d.graceSeen >= d.graceProbes {
				return stallTerminalWedge
			}
		}
		return stallObserveSoon
	}

	s := result.snapshot
	if !s.ProcessAlive || s.Phase == StallPhaseProcessDead || s.Phase == StallPhaseTransportBroken {
		return stallTerminalTransport
	}
	if !s.TransportResponsive {
		result.err = errors.New("stall control plane unresponsive")
		return d.decide(now, session, result)
	}

	progress := false
	if d.observed {
		progress = (s.Generation != 0 && d.generation != 0 && s.Generation != d.generation) ||
			(s.Cursor != "" && d.cursor != "" && s.Cursor != d.cursor) ||
			(s.ActivitySeq != d.activitySeq) ||
			(s.Phase != d.lastSnapshot.Phase) ||
			(s.IsStreaming != d.lastSnapshot.IsStreaming) ||
			(s.IsCompacting != d.lastSnapshot.IsCompacting) ||
			(s.PendingMessageCount != d.lastSnapshot.PendingMessageCount)
	}
	d.observed = true
	d.lastSnapshot = s
	if s.Generation != 0 {
		d.generation = s.Generation
	}
	d.activitySeq = s.ActivitySeq
	if s.Cursor != "" {
		d.cursor = s.Cursor
	}
	if progress {
		d.progressAt = now
		d.failures = 0
		d.inGrace = false
		d.graceSeen = 0
		return stallObserveSoft
	}

	d.failures = 0
	if s.PromptPending || s.Phase == StallPhaseAwaitingAcceptance {
		d.inGrace = false
		d.graceSeen = 0
		return stallObserveSoon
	}
	if d.inGrace {
		d.graceSeen++
		if d.graceSeen >= d.graceProbes {
			return stallTerminalWedge
		}
		return stallObserveSoon
	}
	if d.hardTimeout > 0 && now.Sub(d.progressAt) >= d.hardTimeout {
		d.inGrace = true
		d.graceSeen = 0
		if d.safeToInterrupt() {
			d.interrupted = true
			return stallStartInterrupt
		}
		return stallObserveSoon
	}
	if s.Phase == StallPhaseSettled {
		// agent_settled/EventResult can race this result. Observe soon so core's
		// event path remains the sole settlement owner.
		return stallObserveSoon
	}
	return stallObserveSoft
}

func (d *foregroundStallDetector) safeToInterrupt() bool {
	if d.interrupted {
		return false
	}
	switch d.lastSnapshot.Phase {
	case StallPhaseRunning, StallPhaseStreaming, StallPhaseTool, StallPhaseRetrying:
		return true
	default:
		// Never interrupt an awaiting-acceptance command: its outcome can be
		// unknown and watchdog recovery must not alter acceptance semantics.
		return false
	}
}

func startStallProbe(parent context.Context, session AgentSession, timeout time.Duration, seq uint64, out chan<- stallControlResult) context.CancelFunc {
	ctx, cancel := context.WithTimeout(parent, timeout)
	probe, supported := session.(StallProbeSession)
	go func() {
		start := time.Now()
		result := stallControlResult{kind: stallControlProbe, seq: seq}
		if !supported {
			result.err = ErrNotSupported
		} else {
			result.snapshot, result.err = probe.ProbeStall(ctx)
		}
		result.latency = time.Since(start)
		select {
		case out <- result:
		case <-parent.Done():
		}
	}()
	return cancel
}

func startStallInterrupt(parent context.Context, session AgentSession, timeout time.Duration, seq uint64, out chan<- stallControlResult) context.CancelFunc {
	ctx, cancel := context.WithTimeout(parent, timeout)
	interrupt, supported := session.(StallInterruptSession)
	go func() {
		start := time.Now()
		result := stallControlResult{kind: stallControlInterrupt, seq: seq}
		if !supported {
			result.err = ErrNotSupported
		} else {
			result.err = interrupt.InterruptStall(ctx)
		}
		result.latency = time.Since(start)
		select {
		case out <- result:
		case <-parent.Done():
		}
	}()
	return cancel
}

func stallControlErrorClass(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline_exceeded"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, ErrNotSupported) {
		return "unsupported"
	}
	return "operation_failed"
}

func logStallProbe(sessionKey, operationID string, detector *foregroundStallDetector, result stallControlResult, lastEventType string, lastEventAt, now time.Time, queued int) {
	outcome := "responsive"
	if result.err != nil {
		outcome = "failed"
		if errors.Is(result.err, ErrNotSupported) {
			outcome = "unsupported"
		}
	}
	slog.Warn("foreground stall control observation",
		"session_key", sessionKey,
		"operation_id", operationID,
		"phase", result.snapshot.Phase,
		"pid", result.snapshot.PID,
		"generation", result.snapshot.Generation,
		"activity_seq", result.snapshot.ActivitySeq,
		"last_event_age", now.Sub(lastEventAt),
		"last_event_type", lastEventType,
		"adapter_event_type", result.snapshot.LastEventType,
		"probe_attempt", detector.attempt,
		"probe_outcome", outcome,
		"probe_latency", result.latency,
		"is_streaming", result.snapshot.IsStreaming,
		"is_compacting", result.snapshot.IsCompacting,
		"pending_messages", result.snapshot.PendingMessageCount,
		"prompt_pending", result.snapshot.PromptPending,
		"cursor_moved", result.snapshot.Cursor != "" && detector.cursor != "" && result.snapshot.Cursor != detector.cursor,
		"hard_grace", detector.inGrace,
		"interrupt_issued", detector.interrupted,
		"queued_count", queued,
	)
}
