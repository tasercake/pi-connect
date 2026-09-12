package core

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

type fakeStagingClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeStagingClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *fakeStagingClock) Add(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return c.now
}

type fakeStagingTicker struct {
	ch      chan time.Time
	mu      sync.Mutex
	stopped bool
}

func (t *fakeStagingTicker) Chan() <-chan time.Time { return t.ch }
func (t *fakeStagingTicker) Stop() {
	t.mu.Lock()
	t.stopped = true
	t.mu.Unlock()
}
func (t *fakeStagingTicker) isStopped() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stopped
}

type stagingCapturePlatform struct {
	mu              sync.Mutex
	starts          []string
	updates         []string
	sent            []string
	replies         []string
	deleted         []any
	deleteCalls     int
	deleteErrAt     int
	deleteErr       error
	updateErrAt     int
	updateErrAlways bool
	updateErr       error
	sendErr         error
	retryDelay      time.Duration
	updateCalls     int
	attemptedAt     []time.Time
	updated         chan struct{}
}

func (p *stagingCapturePlatform) Name() string               { return "capable" }
func (p *stagingCapturePlatform) Start(MessageHandler) error { return nil }
func (p *stagingCapturePlatform) Stop() error                { return nil }
func (p *stagingCapturePlatform) StagingProgressStyle() StagingProgressStyle {
	return StagingProgressStyle{ToolBodiesAsCode: true, RepeatLiveHeader: true, CompactOnComplete: true}
}
func (p *stagingCapturePlatform) Reply(_ context.Context, _ any, content string) error {
	p.mu.Lock()
	p.replies = append(p.replies, content)
	p.mu.Unlock()
	return nil
}
func (p *stagingCapturePlatform) Send(_ context.Context, _ any, content string) error {
	p.mu.Lock()
	p.sent = append(p.sent, content)
	err := p.sendErr
	p.mu.Unlock()
	return err
}
func (p *stagingCapturePlatform) SendPreviewStart(_ context.Context, _ any, content string) (any, error) {
	p.mu.Lock()
	p.starts = append(p.starts, content)
	p.mu.Unlock()
	return "one-handle", nil
}
func (p *stagingCapturePlatform) UpdateMessage(_ context.Context, handle any, content string) error {
	if handle != "one-handle" {
		return errors.New("wrong preview handle")
	}
	p.mu.Lock()
	p.updateCalls++
	p.attemptedAt = append(p.attemptedAt, time.Now())
	call := p.updateCalls
	shouldFail := p.updateErrAlways || (p.updateErrAt > 0 && call == p.updateErrAt)
	if !shouldFail {
		p.updates = append(p.updates, content)
	}
	updated := p.updated
	p.mu.Unlock()
	if updated != nil {
		updated <- struct{}{}
	}
	if shouldFail {
		if p.updateErr != nil {
			return p.updateErr
		}
		return errors.New("edit failed")
	}
	return nil
}
func (p *stagingCapturePlatform) ProgressUpdateRetryAfter(err error) (time.Duration, bool) {
	return p.retryDelay, p.retryDelay > 0 && errors.Is(err, p.updateErr)
}
func (p *stagingCapturePlatform) DeletePreviewMessage(_ context.Context, handle any) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deleteCalls++
	if p.deleteErrAt > 0 && p.deleteCalls == p.deleteErrAt {
		return p.deleteErr
	}
	p.deleted = append(p.deleted, handle)
	return nil
}
func (p *stagingCapturePlatform) snapshot() (starts, updates, sent []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.starts...), append([]string(nil), p.updates...), append([]string(nil), p.sent...)
}
func (p *stagingCapturePlatform) replySnapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.replies...)
}
func (p *stagingCapturePlatform) deleteSnapshot() []any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]any(nil), p.deleted...)
}
func (p *stagingCapturePlatform) deleteAttempts() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.deleteCalls
}
func (p *stagingCapturePlatform) attempts() (int, []time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.updateCalls, append([]time.Time(nil), p.attemptedAt...)
}

// plainStagingPlatform intentionally omits StagingProgressStyleProvider to
// verify that non-Telegram platforms retain their original rendering.
type plainStagingPlatform struct{ capture *stagingCapturePlatform }

func (p *plainStagingPlatform) Name() string                 { return "plain" }
func (p *plainStagingPlatform) Start(h MessageHandler) error { return p.capture.Start(h) }
func (p *plainStagingPlatform) Stop() error                  { return p.capture.Stop() }
func (p *plainStagingPlatform) Reply(ctx context.Context, replyCtx any, content string) error {
	return p.capture.Reply(ctx, replyCtx, content)
}
func (p *plainStagingPlatform) Send(ctx context.Context, replyCtx any, content string) error {
	return p.capture.Send(ctx, replyCtx, content)
}
func (p *plainStagingPlatform) SendPreviewStart(ctx context.Context, replyCtx any, content string) (any, error) {
	return p.capture.SendPreviewStart(ctx, replyCtx, content)
}
func (p *plainStagingPlatform) UpdateMessage(ctx context.Context, handle any, content string) error {
	return p.capture.UpdateMessage(ctx, handle, content)
}

type blockingUpdateStagingPlatform struct {
	*stagingCapturePlatform
	updateStarted chan struct{}
	releaseUpdate chan struct{}
}

func (p *blockingUpdateStagingPlatform) UpdateMessage(context.Context, any, string) error {
	select {
	case p.updateStarted <- struct{}{}:
	default:
	}
	<-p.releaseUpdate
	return errors.New("permanent edit failure")
}

func waitStaging(t *testing.T, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func allStagingRenders(p *stagingCapturePlatform) []string {
	starts, updates, _ := p.snapshot()
	return append(starts, updates...)
}

func newTestStagingWriter(t *testing.T, p Platform) (*stagingProgressWriter, *fakeStagingTicker, *fakeStagingClock) {
	t.Helper()
	start := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)
	clock := &fakeStagingClock{now: start}
	ticker := &fakeStagingTicker{ch: make(chan time.Time, 1)}
	w := newStagingProgressWriter(context.Background(), p, "reply", start, LangEnglish, nil, &stagingProgressOptions{
		now: clock.Now,
		newTicker: func(time.Duration) stagingTicker {
			return ticker
		},
	})
	return w, ticker, clock
}

func TestStagingProgressWriterTimelineCountsCoalescingAndFinalState(t *testing.T) {
	p := &stagingCapturePlatform{updated: make(chan struct{}, 20)}
	w, ticker, clock := newTestStagingWriter(t, p)
	if !w.Start() {
		t.Fatal("Start() = false")
	}
	waitStaging(t, func() bool { starts, _, _ := p.snapshot(); return len(starts) == 1 }, "initial preview")
	starts, _, _ := p.snapshot()
	if len(starts) != 1 || starts[0] != "⏳ 0s · 🔧 0 · 🪜 0" {
		t.Fatalf("initial staging message = %#v", starts)
	}

	if !w.AppendThinking("plan") || !w.AppendToolUse(1, "Bash", "printf hi") {
		t.Fatal("thinking/tool append failed")
	}
	code, success := 0, true
	if !w.AppendToolResult("Bash", "hi", "completed", &code, &success) {
		t.Fatal("tool result append failed")
	}
	if !w.AppendText("draft ") || !w.AppendText("answer") {
		t.Fatal("text append failed")
	}
	if !w.AppendPermission("Write", "safe preview") {
		t.Fatal("permission append failed")
	}

	for len(p.updated) > 0 {
		<-p.updated
	}
	ticker.ch <- clock.Add(5 * time.Second)
	select {
	case <-p.updated:
	case <-time.After(time.Second):
		t.Fatal("ticker did not update elapsed header")
	}
	_, liveUpdates, _ := p.snapshot()
	live := liveUpdates[len(liveUpdates)-1]
	liveHeader := "⏳ 5s · 🔧 1 · 🪜 5"
	if !strings.HasPrefix(live, liveHeader+"\n\n") || !strings.HasSuffix(live, "\n\n"+liveHeader) {
		t.Fatalf("live status header not repeated at bottom:\n%s", live)
	}
	for _, want := range []string{
		"💭 plan",
		"🔧 #1 Bash\n```\nprintf hi\n```",
		"✅ #1 Bash · completed · ↩ 0\n```\nhi\n```",
		"✍️ draft answer",
		"🔐 Write",
	} {
		if !strings.Contains(live, want) {
			t.Fatalf("live timeline missing %q:\n%s", want, live)
		}
	}
	if strings.Count(live, "✍️") != 1 {
		t.Fatalf("text deltas not coalesced:\n%s", live)
	}

	clock.Add(37 * time.Second)
	if !w.Finalize(stagingStateCompleted) {
		t.Fatal("Finalize() = false")
	}
	waitStaging(t, ticker.isStopped, "final timeline")

	_, updates, _ := p.snapshot()
	if len(updates) != 2 {
		t.Fatalf("event burst produced %d edits, want one batched edit plus final", len(updates))
	}
	if got, want := updates[len(updates)-1], "✅ 42s · 🔧 1 · 🪜 5"; got != want {
		t.Fatalf("terminal summary = %q, want %q", got, want)
	}
}

func TestStagingProgressWriterDiscardWinsInFlightPermanentEditFailure(t *testing.T) {
	capture := &stagingCapturePlatform{}
	p := &blockingUpdateStagingPlatform{
		stagingCapturePlatform: capture,
		updateStarted:          make(chan struct{}, 1),
		releaseUpdate:          make(chan struct{}),
	}
	w, ticker, _ := newTestStagingWriter(t, p)
	if !w.Start() {
		t.Fatal("Start() = false")
	}
	waitStaging(t, func() bool { starts, _, _ := capture.snapshot(); return len(starts) == 1 }, "discard-race initial preview")
	if !w.AppendThinking("must be deleted") {
		t.Fatal("AppendThinking() = false")
	}
	ticker.ch <- time.Now()
	select {
	case <-p.updateStarted:
	case <-time.After(time.Second):
		t.Fatal("live update did not start")
	}
	if !w.Discard() {
		t.Fatal("Discard() = false")
	}
	close(p.releaseUpdate)
	waitStaging(t, func() bool { return len(capture.deleteSnapshot()) == 1 }, "discard after failed in-flight edit")
	waitStaging(t, ticker.isStopped, "discard-race worker stop")
}

func TestStagingProgressWriterDiscardRetriesTransientDeletion(t *testing.T) {
	transientErr := errors.New("telegram 429")
	p := &stagingCapturePlatform{
		deleteErrAt: 1,
		deleteErr:   transientErr,
		updateErr:   transientErr,
		retryDelay:  10 * time.Millisecond,
	}
	w, ticker, _ := newTestStagingWriter(t, p)
	if !w.Start() {
		t.Fatal("Start() = false")
	}
	waitStaging(t, func() bool { starts, _, _ := p.snapshot(); return len(starts) == 1 }, "delete-retry initial preview")
	if !w.Discard() {
		t.Fatal("Discard() = false")
	}
	waitStaging(t, func() bool { return p.deleteAttempts() == 2 && len(p.deleteSnapshot()) == 1 }, "transient deletion retry")
	waitStaging(t, ticker.isStopped, "delete-retry worker stop")
}

func TestStagingProgressWriterDefaultStyleRemainsPlainAndDetailed(t *testing.T) {
	capture := &stagingCapturePlatform{}
	p := &plainStagingPlatform{capture: capture}
	w, ticker, _ := newTestStagingWriter(t, p)
	if !w.Start() {
		t.Fatal("Start() = false")
	}
	waitStaging(t, func() bool { starts, _, _ := capture.snapshot(); return len(starts) == 1 }, "plain initial preview")
	if !w.AppendToolUse(1, "Bash", "echo plain") {
		t.Fatal("AppendToolUse() = false")
	}
	ticker.ch <- time.Now()
	waitStaging(t, func() bool { _, updates, _ := capture.snapshot(); return len(updates) == 1 }, "plain live update")
	_, updates, _ := capture.snapshot()
	if got := updates[0]; strings.Contains(got, "```") || strings.Count(got, "⏳ ") != 1 || !strings.Contains(got, "🔧 #1 Bash echo plain") {
		t.Fatalf("default live staging style changed: %q", got)
	}
	if !w.Finalize(stagingStateCompleted) {
		t.Fatal("Finalize() = false")
	}
	waitStaging(t, ticker.isStopped, "plain terminal update")
	_, updates, _ = capture.snapshot()
	if got := updates[len(updates)-1]; !strings.Contains(got, "🔧 #1 Bash echo plain") || strings.Contains(got, "```") || strings.Count(got, "✅ ") != 1 {
		t.Fatalf("default terminal staging style changed: %q", got)
	}
}

func TestStagingProgressWriterFailedAndCancelledStates(t *testing.T) {
	p := &stagingCapturePlatform{}
	w, ticker, _ := newTestStagingWriter(t, p)
	if !w.Start() {
		t.Fatal("failed-state Start() = false")
	}
	waitStaging(t, func() bool { starts, _, _ := p.snapshot(); return len(starts) == 1 }, "failed-state preview")
	if !w.AppendError("agent failed") || !w.Finalize(stagingStateFailed) {
		t.Fatal("failed-state lifecycle failed")
	}
	waitStaging(t, ticker.isStopped, "failed final timeline")
	renders := allStagingRenders(p)
	last := renders[len(renders)-1]
	if !strings.HasPrefix(last, "❌ ") || !strings.Contains(last, "❌ agent failed") {
		t.Fatalf("failed rendering = %q", last)
	}

	p2 := &stagingCapturePlatform{}
	w2, ticker2, _ := newTestStagingWriter(t, p2)
	if !w2.Start() {
		t.Fatal("cancelled-state Start() = false")
	}
	waitStaging(t, func() bool { starts, _, _ := p2.snapshot(); return len(starts) == 1 }, "cancelled-state preview")
	if !w2.Finalize(stagingStateCancelled) {
		t.Fatal("cancelled-state lifecycle failed")
	}
	waitStaging(t, ticker2.isStopped, "cancelled final timeline")
	renders2 := allStagingRenders(p2)
	if got := renders2[len(renders2)-1]; !strings.HasPrefix(got, "🛑 ") {
		t.Fatalf("cancelled rendering = %q", got)
	}
}

func TestStagingProgressWriterMiddleTruncationCapAndOldestOmission(t *testing.T) {
	p := &stagingCapturePlatform{}
	w, ticker, _ := newTestStagingWriter(t, p)
	if !w.Start() {
		t.Fatal("Start() = false")
	}
	waitStaging(t, func() bool { starts, _, _ := p.snapshot(); return len(starts) == 1 }, "initial preview")
	long := "```json\nHEAD-" + strings.Repeat("界", 3000) + "-TAIL\n```"
	if !w.AppendToolUse(1, "Read", long) {
		t.Fatal("long tool append failed")
	}
	ticker.ch <- time.Now()
	waitStaging(t, func() bool { _, updates, _ := p.snapshot(); return len(updates) == 1 }, "long tool update")
	for i := 0; i < stagingProgressMaxEntries+3; i++ {
		if !w.AppendThinking("old-step-" + strings.Repeat("x", 30)) {
			t.Fatalf("append %d failed", i)
		}
	}
	ticker.ch <- time.Now()
	waitStaging(t, func() bool { _, updates, _ := p.snapshot(); return len(updates) == 2 }, "omission update")
	starts, updates, _ := p.snapshot()
	last := updates[len(updates)-1]
	for i, rendered := range append(starts, updates...) {
		if !utf8.ValidString(rendered) {
			t.Fatalf("render %d is invalid UTF-8", i)
		}
		if utf8.RuneCountInString(rendered) > stagingProgressMaxRunes {
			t.Fatalf("render %d runes = %d, cap = %d", i, utf8.RuneCountInString(rendered), stagingProgressMaxRunes)
		}
	}
	if strings.Count(updates[0], "```") != 2 || strings.Contains(updates[0], "```json") {
		t.Fatalf("tool body fence was malformed or nested fence was not neutralized:\n%s", updates[0])
	}
	if !strings.Contains(updates[0], "HEAD-") || !strings.Contains(updates[0], "-TAIL") || !strings.Contains(updates[0], "characters omitted") {
		t.Fatalf("middle truncation did not retain head/tail/marker:\n%s", updates[0])
	}
	if !strings.Contains(last, "older steps omitted") {
		t.Fatalf("oldest-entry omission marker missing:\n%s", last)
	}
	if !strings.Contains(last, "old-step-") {
		t.Fatal("newest entries missing")
	}
	w.Finalize(stagingStateCompleted)
	waitStaging(t, ticker.isStopped, "truncation final timeline")
}

func TestStagingProgressWriterPermanentFailureSuppressesFurtherProgress(t *testing.T) {
	p := &stagingCapturePlatform{updateErrAt: 2}
	w, ticker, _ := newTestStagingWriter(t, p)
	if !w.Start() {
		t.Fatal("Start() = false")
	}
	waitStaging(t, func() bool { starts, _, _ := p.snapshot(); return len(starts) == 1 }, "initial preview")
	if !w.AppendThinking("must stay quiet") {
		t.Fatal("AppendThinking() = false before failed edit")
	}
	ticker.ch <- time.Now()
	waitStaging(t, func() bool { calls, _ := p.attempts(); return calls == 1 }, "successful detailed update")
	if !w.AppendThinking("newer detail") {
		t.Fatal("second AppendThinking() = false")
	}
	ticker.ch <- time.Now()
	waitStaging(t, func() bool { calls, _ := p.attempts(); return calls == 2 }, "permanent live failure")
	if ticker.isStopped() || !w.Active() {
		t.Fatal("writer gave up terminal-cleanup ownership after permanent live failure")
	}
	if !w.AppendToolUse(1, "Bash", "echo hi") {
		t.Fatal("degraded writer did not suppress later progress")
	}
	if !w.Finalize(stagingStateCompleted) {
		t.Fatal("Finalize() = false after permanent failure")
	}
	waitStaging(t, ticker.isStopped, "terminal cleanup after permanent live failure")
	calls, _ := p.attempts()
	starts, updates, sent := p.snapshot()
	if calls != 3 || len(starts) != 1 || len(updates) != 2 || len(sent) != 0 {
		t.Fatalf("terminal cleanup delivery: calls=%d starts=%d updates=%d sent=%d", calls, len(starts), len(updates), len(sent))
	}
	if !strings.Contains(updates[0], "must stay quiet") {
		t.Fatalf("successful detailed update missing before degradation: %q", updates[0])
	}
	if got := updates[1]; !strings.HasPrefix(got, "✅ ") || strings.Contains(got, "must stay quiet") || strings.Contains(got, "echo hi") || strings.Contains(got, "\n") {
		t.Fatalf("terminal cleanup retained detailed content: %q", got)
	}
}

func TestStagingProgressWriterTransientFailureRetriesLatestSnapshot(t *testing.T) {
	transientErr := errors.New("retry later")
	p := &stagingCapturePlatform{
		updateErrAt: 1,
		updateErr:   transientErr,
		retryDelay:  30 * time.Millisecond,
		updated:     make(chan struct{}, 4),
	}
	w, ticker, _ := newTestStagingWriter(t, p)
	if !w.Start() {
		t.Fatal("Start() = false")
	}
	waitStaging(t, func() bool { starts, _, _ := p.snapshot(); return len(starts) == 1 }, "initial preview")
	if !w.AppendThinking("old snapshot") {
		t.Fatal("first append failed")
	}
	ticker.ch <- time.Now()
	select {
	case <-p.updated:
	case <-time.After(time.Second):
		t.Fatal("first update attempt missing")
	}
	if !w.AppendToolUse(1, "Bash", "latest snapshot") {
		t.Fatal("append during retry failed")
	}
	waitStaging(t, func() bool { calls, _ := p.attempts(); return calls == 2 }, "transient retry")
	calls, attemptedAt := p.attempts()
	if calls != 2 || attemptedAt[1].Sub(attemptedAt[0]) < p.retryDelay {
		t.Fatalf("retry timing calls=%d delay=%v, want >=%v", calls, attemptedAt[1].Sub(attemptedAt[0]), p.retryDelay)
	}
	_, updates, sent := p.snapshot()
	if len(updates) != 1 || !strings.Contains(updates[0], "latest snapshot") {
		t.Fatalf("retry did not send latest snapshot: %#v", updates)
	}
	if len(sent) != 0 {
		t.Fatalf("transient failure emitted standalone progress: %#v", sent)
	}
	w.Finalize(stagingStateCompleted)
	waitStaging(t, ticker.isStopped, "transient final timeline")
}

func TestStagingProgressWriterTerminalBeforePreviewSuppressesLateTimeline(t *testing.T) {
	capture := &stagingCapturePlatform{}
	p := &blockingStagingSchedulerPlatform{stagingCapturePlatform: capture, release: make(chan struct{})}
	w, _, _ := newTestStagingWriter(t, p)
	if !w.Start() || !w.AppendThinking("fast turn") || !w.Finalize(stagingStateCompleted) {
		t.Fatal("staging lifecycle failed")
	}
	close(p.release)
	waitStaging(t, func() bool {
		select {
		case <-w.done:
			return true
		default:
			return false
		}
	}, "terminal writer without preview")
	starts, updates, _ := capture.snapshot()
	if len(starts) != 0 || len(updates) != 0 {
		t.Fatalf("terminal writer created late timeline: starts=%d updates=%d", len(starts), len(updates))
	}
}

func TestStagingProgressWriterRepeated429StopsAtTerminalDeadline(t *testing.T) {
	transientErr := errors.New("repeated 429")
	p := &stagingCapturePlatform{
		updateErrAlways: true,
		updateErr:       transientErr,
		retryDelay:      time.Second,
	}
	ticker := &fakeStagingTicker{ch: make(chan time.Time, 1)}
	w := newStagingProgressWriter(context.Background(), p, "reply", time.Now(), LangEnglish, nil, &stagingProgressOptions{
		newTicker:        func(time.Duration) stagingTicker { return ticker },
		terminalLifetime: 65 * time.Millisecond,
	})
	if !w.Start() {
		t.Fatal("Start() = false")
	}
	waitStaging(t, func() bool { starts, _, _ := p.snapshot(); return len(starts) == 1 }, "repeated-429 preview")
	if !w.AppendThinking("retain") {
		t.Fatal("append failed")
	}
	ticker.ch <- time.Now()
	waitStaging(t, func() bool { calls, _ := p.attempts(); return calls == 1 }, "pre-terminal 429 retry wait")

	started := time.Now()
	if !w.Finalize(stagingStateCompleted) {
		t.Fatal("Finalize() = false")
	}
	waitStaging(t, func() bool {
		select {
		case <-w.done:
			return true
		default:
			return false
		}
	}, "terminal retry deadline")
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("pre-terminal retry outlived terminal deadline: %v", elapsed)
	}
	calls, _ := p.attempts()
	if calls < 2 {
		t.Fatalf("update attempts = %d, want terminal retry after pre-terminal 429", calls)
	}
	_, updates, sent := p.snapshot()
	if len(updates) != 0 || len(sent) != 0 {
		t.Fatalf("repeated 429 produced noise: updates=%d sent=%d", len(updates), len(sent))
	}
}

func TestStagingProgressWriterUsesOutboundGateForPreviewAndEdit(t *testing.T) {
	p := &stagingCapturePlatform{}
	var gateMu sync.Mutex
	gateCalls := 0
	w := newStagingProgressWriter(context.Background(), p, "reply", time.Now(), LangEnglish, nil, &stagingProgressOptions{
		waitOutbound: func(context.Context) error {
			gateMu.Lock()
			gateCalls++
			gateMu.Unlock()
			return nil
		},
	})
	if !w.Start() {
		t.Fatal("Start() = false")
	}
	waitStaging(t, func() bool { starts, _, _ := p.snapshot(); return len(starts) == 1 }, "outbound-gated preview")
	if !w.AppendThinking("batched") || !w.Finalize(stagingStateCompleted) {
		t.Fatal("staging lifecycle failed")
	}
	waitStaging(t, func() bool {
		select {
		case <-w.done:
			return true
		default:
			return false
		}
	}, "outbound-gated final edit")
	gateMu.Lock()
	calls := gateCalls
	gateMu.Unlock()
	if calls != 2 {
		t.Fatalf("outbound gate calls = %d, want preview plus final edit", calls)
	}
}

func TestStagingProgressWriterRequiresBothCapabilities(t *testing.T) {
	p := &stubPlatformNoProgress{}
	w, _, _ := newTestStagingWriter(t, p)
	if w.Start() {
		t.Fatal("Start() = true without preview/update capabilities")
	}
	select {
	case <-w.stopWorker:
	default:
		t.Fatal("disabled writer was not closed")
	}
}
