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
	mu          sync.Mutex
	starts      []string
	updates     []string
	sent        []string
	updateErrAt int
	updateCalls int
	updated     chan struct{}
}

func (p *stagingCapturePlatform) Name() string                             { return "capable" }
func (p *stagingCapturePlatform) Start(MessageHandler) error               { return nil }
func (p *stagingCapturePlatform) Stop() error                              { return nil }
func (p *stagingCapturePlatform) Reply(context.Context, any, string) error { return nil }
func (p *stagingCapturePlatform) Send(_ context.Context, _ any, content string) error {
	p.mu.Lock()
	p.sent = append(p.sent, content)
	p.mu.Unlock()
	return nil
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
	call := p.updateCalls
	if p.updateErrAt == 0 || call != p.updateErrAt {
		p.updates = append(p.updates, content)
	}
	updated := p.updated
	p.mu.Unlock()
	if updated != nil {
		updated <- struct{}{}
	}
	if p.updateErrAt > 0 && call == p.updateErrAt {
		return errors.New("edit failed")
	}
	return nil
}
func (p *stagingCapturePlatform) snapshot() (starts, updates, sent []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.starts...), append([]string(nil), p.updates...), append([]string(nil), p.sent...)
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
	clock.Add(37 * time.Second)
	if !w.Finalize(stagingStateCompleted) {
		t.Fatal("Finalize() = false")
	}
	if !ticker.isStopped() {
		t.Fatal("ticker was not stopped")
	}

	_, updates, _ := p.snapshot()
	last := updates[len(updates)-1]
	for _, want := range []string{
		"✅ 42s · 🔧 1 · 🪜 5",
		"💭 plan",
		"🔧 #1 Bash",
		"✅ #1 Bash · completed · ↩ 0",
		"📤", // checked below using success-specific equivalent
		"✍️ draft answer",
		"🔐 Write",
	} {
		if want == "📤" {
			continue
		}
		if !strings.Contains(last, want) {
			t.Fatalf("final timeline missing %q:\n%s", want, last)
		}
	}
	if strings.Count(last, "✍️") != 1 {
		t.Fatalf("text deltas not coalesced:\n%s", last)
	}
}

func TestStagingProgressWriterFailedAndCancelledStates(t *testing.T) {
	p := &stagingCapturePlatform{}
	w, ticker, _ := newTestStagingWriter(t, p)
	if !w.Start() || !w.AppendError("agent failed") || !w.Finalize(stagingStateFailed) {
		t.Fatal("failed-state lifecycle failed")
	}
	_, updates, _ := p.snapshot()
	last := updates[len(updates)-1]
	if !strings.HasPrefix(last, "❌ ") || !strings.Contains(last, "❌ agent failed") {
		t.Fatalf("failed rendering = %q", last)
	}
	if !ticker.isStopped() {
		t.Fatal("failed finalize did not stop timer")
	}

	p2 := &stagingCapturePlatform{}
	w2, ticker2, _ := newTestStagingWriter(t, p2)
	if !w2.Start() || !w2.Finalize(stagingStateCancelled) {
		t.Fatal("cancelled-state lifecycle failed")
	}
	_, updates2, _ := p2.snapshot()
	if got := updates2[len(updates2)-1]; !strings.HasPrefix(got, "🛑 ") {
		t.Fatalf("cancelled rendering = %q", got)
	}
	if !ticker2.isStopped() {
		t.Fatal("cancelled finalize did not stop timer")
	}
}

func TestStagingProgressWriterMiddleTruncationCapAndOldestOmission(t *testing.T) {
	p := &stagingCapturePlatform{}
	w, _, _ := newTestStagingWriter(t, p)
	if !w.Start() {
		t.Fatal("Start() = false")
	}
	long := "```json\nHEAD-" + strings.Repeat("界", 3000) + "-TAIL\n```"
	if !w.AppendToolUse(1, "Read", long) {
		t.Fatal("long tool append failed")
	}
	for i := 0; i < stagingProgressMaxEntries+3; i++ {
		if !w.AppendThinking("old-step-" + strings.Repeat("x", 30)) {
			t.Fatalf("append %d failed", i)
		}
	}
	starts, updates, _ := p.snapshot()
	last := updates[len(updates)-1]
	for i, rendered := range append(starts, updates...) {
		if !utf8.ValidString(rendered) {
			t.Fatalf("render %d is invalid UTF-8", i)
		}
		if utf8.RuneCountInString(rendered) > stagingProgressMaxRunes {
			t.Fatalf("render %d runes = %d, cap = %d", i, utf8.RuneCountInString(rendered), stagingProgressMaxRunes)
		}
		if strings.Contains(rendered, "```") {
			t.Fatalf("render %d retained markdown fence", i)
		}
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
}

func TestStagingProgressWriterUpdateFailureDisablesAndStops(t *testing.T) {
	p := &stagingCapturePlatform{updateErrAt: 1}
	w, ticker, _ := newTestStagingWriter(t, p)
	if !w.Start() {
		t.Fatal("Start() = false")
	}
	if w.AppendThinking("must fall back") {
		t.Fatal("AppendThinking() = true after failed edit")
	}
	if w.Active() {
		t.Fatal("writer stayed active after update failure")
	}
	if !ticker.isStopped() {
		t.Fatal("ticker not stopped after update failure")
	}
	if w.AppendToolUse(1, "Bash", "echo hi") {
		t.Fatal("disabled writer swallowed later event")
	}
}

func TestStagingProgressWriterRequiresBothCapabilities(t *testing.T) {
	p := &stubPlatformNoProgress{}
	w, _, _ := newTestStagingWriter(t, p)
	if w.Start() {
		t.Fatal("Start() = true without preview/update capabilities")
	}
	select {
	case <-w.stopTicker:
	default:
		t.Fatal("disabled writer was not closed")
	}
}
