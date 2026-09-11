package core

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	stagingProgressMaxRunes    = maxPlatformMessageLen - 200
	stagingProgressBodyRunes   = 900
	stagingProgressToolRunes   = 120
	stagingProgressStatusRunes = 80
	stagingProgressMaxEntries  = 128
	stagingProgressTick        = 5 * time.Second
)

type stagingProgressState string

const (
	stagingStateRunning   stagingProgressState = "running"
	stagingStateCompleted stagingProgressState = "completed"
	stagingStateFailed    stagingProgressState = "failed"
	stagingStateCancelled stagingProgressState = "cancelled"
)

type stagingTicker interface {
	Chan() <-chan time.Time
	Stop()
}

type realStagingTicker struct{ *time.Ticker }

func (t realStagingTicker) Chan() <-chan time.Time { return t.C }

type stagingProgressOptions struct {
	now          func() time.Time
	newTicker    func(time.Duration) stagingTicker
	waitOutbound func(context.Context) error
}

type stagingEntryKind int

const (
	stagingEntryThinking stagingEntryKind = iota
	stagingEntryToolUse
	stagingEntryToolResult
	stagingEntryText
	stagingEntryError
	stagingEntryPermission
)

type stagingEntry struct {
	kind     stagingEntryKind
	body     string
	tool     string
	toolNo   int
	status   string
	exitCode *int
	success  *bool
}

// stagingProgressWriter owns one editable progress message for one turn.
// Event methods only mutate the latest timeline snapshot. One worker performs
// coalesced API calls without holding mu, so platform backoff never blocks the
// agent event loop. If delivery fails permanently, the writer keeps ownership
// of progress events and degrades to no progress rather than flooding chat.
type stagingProgressWriter struct {
	mu sync.Mutex

	ctx             context.Context
	cancel          context.CancelFunc
	platform        Platform
	replyCtx        any
	starter         PreviewStarter
	updater         MessageUpdater
	scheduler       ProgressUpdateScheduler
	retryClassifier ProgressUpdateRetryClassifier
	waitOutbound    func(context.Context) error
	transform       func(string) string
	i18n            *I18n
	now             func() time.Time
	startTime       time.Time

	handle            any
	entries           []stagingEntry
	omittedEntries    int
	toolCount         int
	stepCount         int
	state             stagingProgressState
	enabled           bool
	degraded          bool
	terminal          bool
	stopped           bool
	lastSent          string
	lastUpdateAt      time.Time
	minUpdateInterval time.Duration

	newTicker  func(time.Duration) stagingTicker
	ticker     stagingTicker
	wake       chan struct{}
	stopWorker chan struct{}
	done       chan struct{}
	stopOnce   sync.Once
}

func newStagingProgressWriter(ctx context.Context, p Platform, replyCtx any, startTime time.Time, lang Language, transform func(string) string, options *stagingProgressOptions) *stagingProgressWriter {
	workerCtx, cancel := context.WithCancel(ctx)
	w := &stagingProgressWriter{
		ctx:        workerCtx,
		cancel:     cancel,
		platform:   p,
		replyCtx:   replyCtx,
		transform:  transform,
		i18n:       NewI18n(lang),
		startTime:  startTime,
		state:      stagingStateRunning,
		wake:       make(chan struct{}, 1),
		stopWorker: make(chan struct{}),
		done:       make(chan struct{}),
		now:        time.Now,
		newTicker:  func(d time.Duration) stagingTicker { return realStagingTicker{time.NewTicker(d)} },
	}
	if options != nil {
		if options.now != nil {
			w.now = options.now
		}
		if options.newTicker != nil {
			w.newTicker = options.newTicker
		}
		w.waitOutbound = options.waitOutbound
	}
	if w.startTime.IsZero() {
		w.startTime = w.now()
	}
	starter, starterOK := p.(PreviewStarter)
	updater, updaterOK := p.(MessageUpdater)
	if !starterOK || !updaterOK {
		return w
	}
	w.starter = starter
	w.updater = updater
	if throttler, ok := p.(ProgressUpdateThrottler); ok {
		w.minUpdateInterval = throttler.ProgressUpdateInterval()
	}
	w.scheduler, _ = p.(ProgressUpdateScheduler)
	w.retryClassifier, _ = p.(ProgressUpdateRetryClassifier)
	return w
}

// Start enables staging ownership and starts asynchronous delivery. Capability
// failures return false; API failures degrade silently while progress remains
// handled by this writer.
func (w *stagingProgressWriter) Start() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.enabled {
		return true
	}
	if w.starter == nil || w.updater == nil || w.stopped {
		w.stopWorkerLocked()
		return false
	}
	w.enabled = true
	w.ticker = w.newTicker(stagingProgressTick)
	go w.runWorker()
	w.signalLocked()
	return true
}

func (w *stagingProgressWriter) Active() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.enabled && !w.terminal && w.state == stagingStateRunning && (!w.stopped || w.degraded)
}

func (w *stagingProgressWriter) AppendThinking(text string) bool {
	return w.append(stagingEntry{kind: stagingEntryThinking, body: text}, false)
}

func (w *stagingProgressWriter) AppendToolUse(number int, name, input string) bool {
	return w.append(stagingEntry{kind: stagingEntryToolUse, toolNo: number, tool: name, body: input}, false)
}

func (w *stagingProgressWriter) AppendToolResult(name, result, status string, exitCode *int, success *bool) bool {
	return w.append(stagingEntry{kind: stagingEntryToolResult, tool: name, body: result, status: status, exitCode: exitCode, success: success}, false)
}

func (w *stagingProgressWriter) AppendText(text string) bool {
	return w.append(stagingEntry{kind: stagingEntryText, body: text}, true)
}

func (w *stagingProgressWriter) AppendPermission(name, input string) bool {
	// Permission UI is sent separately by the engine. Do not bypass progress
	// pacing here; security UX must not wait for a timeline edit.
	return w.append(stagingEntry{kind: stagingEntryPermission, tool: name, body: input}, false)
}

func (w *stagingProgressWriter) AppendError(text string) bool {
	return w.append(stagingEntry{kind: stagingEntryError, body: text}, false)
}

func (w *stagingProgressWriter) append(entry stagingEntry, coalesceText bool) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.enabled || w.terminal || w.state != stagingStateRunning {
		return false
	}
	if w.degraded {
		return true
	}
	if w.stopped {
		return false
	}
	if entry.kind != stagingEntryText {
		entry.body = strings.TrimSpace(entry.body)
	}
	entry.tool = middleTruncateStaging(strings.TrimSpace(entry.tool), stagingProgressToolRunes, w.bodyOmission)
	entry.status = middleTruncateStaging(strings.TrimSpace(entry.status), stagingProgressStatusRunes, w.bodyOmission)
	if entry.exitCode != nil {
		exitCode := *entry.exitCode
		entry.exitCode = &exitCode
	}
	if entry.success != nil {
		success := *entry.success
		entry.success = &success
	}
	if w.transform != nil && entry.body != "" {
		entry.body = w.transform(entry.body)
	}
	// Timeline is plain compact text. Remove fence delimiters so truncation can
	// never leave malformed markdown around multiline tool bodies.
	entry.body = strings.ReplaceAll(entry.body, "```", "'''")
	entry.body = middleTruncateStaging(entry.body, stagingProgressBodyRunes, w.bodyOmission)

	if coalesceText && len(w.entries) > 0 && w.entries[len(w.entries)-1].kind == stagingEntryText {
		last := &w.entries[len(w.entries)-1]
		last.body = middleTruncateStaging(last.body+entry.body, stagingProgressBodyRunes, w.bodyOmission)
	} else {
		if entry.kind == stagingEntryToolUse {
			w.toolCount++
			if entry.toolNo <= 0 {
				entry.toolNo = w.toolCount
			}
		} else if entry.kind == stagingEntryToolResult {
			entry.toolNo = w.latestToolNumberLocked(entry.tool)
		}
		w.entries = append(w.entries, entry)
		w.stepCount++
		if len(w.entries) > stagingProgressMaxEntries {
			drop := len(w.entries) - stagingProgressMaxEntries
			w.entries = append([]stagingEntry(nil), w.entries[drop:]...)
			w.omittedEntries += drop
		}
	}
	// Periodic ticks batch all events since the previous edit. The terminal
	// transition wakes the worker immediately and still observes platform pacing.
	return true
}

func (w *stagingProgressWriter) latestToolNumberLocked(name string) int {
	for i := len(w.entries) - 1; i >= 0; i-- {
		if w.entries[i].kind == stagingEntryToolUse && (name == "" || strings.EqualFold(w.entries[i].tool, name)) {
			return w.entries[i].toolNo
		}
	}
	return w.toolCount
}

func (w *stagingProgressWriter) Finalize(state stagingProgressState) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if state == "" {
		state = stagingStateCompleted
	}
	if !w.enabled {
		w.stopWorkerLocked()
		return false
	}
	if w.terminal {
		return w.state == state
	}
	w.state = state
	w.terminal = true
	if w.degraded {
		w.stopWorkerLocked()
		return true
	}
	w.signalLocked()
	return true
}

// Stop cancels an unterminated writer. A finalized writer remains detached long
// enough to deliver its terminal snapshot without delaying the final answer.
func (w *stagingProgressWriter) Stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.terminal {
		return
	}
	w.stopped = true
	w.stopWorkerLocked()
}

func (w *stagingProgressWriter) runWorker() {
	defer func() {
		w.mu.Lock()
		w.stopped = true
		w.stopWorkerLocked()
		w.mu.Unlock()
		close(w.done)
	}()
	for {
		select {
		case <-w.wake:
		case <-w.ticker.Chan():
		case <-w.stopWorker:
			return
		case <-w.ctx.Done():
			return
		}
		if !w.deliverLatest() {
			return
		}
	}
}

// deliverLatest sends one current snapshot, retrying transient failures with
// latest-state-wins semantics. All waits and API calls happen without mu held.
func (w *stagingProgressWriter) deliverLatest() bool {
	for {
		w.mu.Lock()
		if w.stopped || w.degraded {
			w.mu.Unlock()
			return false
		}
		content := w.renderLocked()
		handle := w.handle
		terminal := w.terminal
		lastSent := w.lastSent
		lastUpdateAt := w.lastUpdateAt
		w.mu.Unlock()

		if content == lastSent {
			if terminal {
				w.mu.Lock()
				w.stopped = true
				w.stopWorkerLocked()
				w.mu.Unlock()
				return false
			}
			return true
		}

		if handle != nil && w.minUpdateInterval > 0 {
			if delay := w.minUpdateInterval - w.now().Sub(lastUpdateAt); delay > 0 {
				if !w.wait(delay) {
					return false
				}
			}
		}
		if w.scheduler != nil {
			if err := w.scheduler.WaitProgressUpdate(w.ctx); err != nil {
				return false
			}
		}
		if w.waitOutbound != nil {
			if err := w.waitOutbound(w.ctx); err != nil {
				return false
			}
		}

		// Re-render after pacing waits so bursts and retry windows send only the
		// newest state.
		w.mu.Lock()
		if w.stopped || w.degraded {
			w.mu.Unlock()
			return false
		}
		content = w.renderLocked()
		handle = w.handle
		w.mu.Unlock()

		callCtx, cancel := w.withAPITimeout()
		var err error
		var newHandle any
		if handle == nil {
			newHandle, err = w.starter.SendPreviewStart(callCtx, w.replyCtx, content)
			if err == nil && newHandle == nil {
				err = fmt.Errorf("nil preview handle")
			}
		} else {
			err = w.updater.UpdateMessage(callCtx, handle, content)
		}
		cancel()
		if err != nil {
			if delay, transient := w.retryAfter(err); transient {
				if w.scheduler != nil {
					w.scheduler.DeferProgressUpdates(delay)
				}
				slog.Warn("staging progress: transient delivery failure; retrying latest snapshot", "platform", w.platform.Name(), "retry_after", delay, "error", err)
				if !w.wait(delay) {
					return false
				}
				continue
			}
			slog.Warn("staging progress: delivery failed permanently; suppressing further progress", "platform", w.platform.Name(), "error", err)
			w.mu.Lock()
			w.degraded = true
			w.stopped = true
			w.stopWorkerLocked()
			w.mu.Unlock()
			return false
		}

		w.mu.Lock()
		if handle == nil {
			w.handle = newHandle
		}
		w.lastSent = content
		w.lastUpdateAt = w.now()
		latest := w.renderLocked()
		terminal = w.terminal
		if terminal && latest == content {
			w.stopped = true
			w.stopWorkerLocked()
		}
		w.mu.Unlock()
		if terminal && latest == content {
			return false
		}
		if terminal && latest != content {
			continue
		}
		return true
	}
}

func (w *stagingProgressWriter) retryAfter(err error) (time.Duration, bool) {
	if w.retryClassifier == nil {
		return 0, false
	}
	delay, ok := w.retryClassifier.ProgressUpdateRetryAfter(err)
	if !ok {
		return 0, false
	}
	if delay < w.minUpdateInterval {
		delay = w.minUpdateInterval
	}
	if delay < 0 {
		delay = 0
	}
	return delay, true
}

func (w *stagingProgressWriter) wait(delay time.Duration) bool {
	if delay <= 0 {
		return true
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-w.stopWorker:
		return false
	case <-w.ctx.Done():
		return false
	}
}

func (w *stagingProgressWriter) renderLocked() string {
	header := fmt.Sprintf("%s %ds · 🔧 %d · 🪜 %d", stagingStateIcon(w.state), max(0, int(w.now().Sub(w.startTime)/time.Second)), w.toolCount, w.stepCount)
	lines := make([]string, len(w.entries))
	for i := range w.entries {
		lines[i] = w.renderEntry(w.entries[i])
	}

	omitted := w.omittedEntries
	for {
		parts := []string{header}
		if omitted > 0 {
			parts = append(parts, w.i18n.Tf(MsgStagingStepsOmitted, omitted))
		}
		parts = append(parts, lines...)
		content := strings.Join(parts, "\n\n")
		if utf8.RuneCountInString(content) <= stagingProgressMaxRunes {
			return content
		}
		if len(lines) > 1 {
			lines = lines[1:]
			omitted++
			continue
		}
		available := stagingProgressMaxRunes - utf8.RuneCountInString(strings.Join(parts[:len(parts)-len(lines)], "\n\n")) - 2
		if available < 1 || len(lines) == 0 {
			return runePrefix(header, stagingProgressMaxRunes)
		}
		lines[0] = middleTruncateStaging(lines[0], available, w.bodyOmission)
	}
}

func (w *stagingProgressWriter) renderEntry(entry stagingEntry) string {
	body := indentStagingBody(entry.body)
	switch entry.kind {
	case stagingEntryThinking:
		return appendStagingBody("💭", body)
	case stagingEntryToolUse:
		label := fmt.Sprintf("🔧 #%d", entry.toolNo)
		if entry.tool != "" {
			label += " " + entry.tool
		}
		return appendStagingBody(label, body)
	case stagingEntryToolResult:
		emoji := "📤"
		if entry.success != nil {
			if *entry.success {
				emoji = "✅"
			} else {
				emoji = "❌"
			}
		}
		label := emoji
		if entry.toolNo > 0 {
			label += fmt.Sprintf(" #%d", entry.toolNo)
		}
		if entry.tool != "" {
			label += " " + entry.tool
		}
		var metadata []string
		if entry.status != "" {
			metadata = append(metadata, entry.status)
		}
		if entry.exitCode != nil {
			metadata = append(metadata, fmt.Sprintf("↩ %d", *entry.exitCode))
		}
		if len(metadata) > 0 {
			label += " · " + strings.Join(metadata, " · ")
		}
		return appendStagingBody(label, body)
	case stagingEntryText:
		return appendStagingBody("✍️", body)
	case stagingEntryError:
		return appendStagingBody("❌", body)
	case stagingEntryPermission:
		label := "🔐"
		if entry.tool != "" {
			label += " " + entry.tool
		}
		return appendStagingBody(label, body)
	default:
		return appendStagingBody("✍️", body)
	}
}

func stagingStateIcon(state stagingProgressState) string {
	switch state {
	case stagingStateCompleted:
		return "✅"
	case stagingStateFailed:
		return "❌"
	case stagingStateCancelled:
		return "🛑"
	default:
		return "⏳"
	}
}

func appendStagingBody(label, body string) string {
	if body == "" {
		return label
	}
	return label + " " + body
}

func indentStagingBody(body string) string {
	return strings.ReplaceAll(body, "\n", "\n  ")
}

func middleTruncateStaging(text string, maxRunes int, omission func(int) string) string {
	if maxRunes <= 0 || utf8.RuneCountInString(text) <= maxRunes {
		return text
	}
	rs := []rune(text)
	omitted := len(rs) - maxRunes
	var marker string
	var keep int
	for {
		marker = omission(omitted)
		markerRunes := utf8.RuneCountInString(marker)
		if markerRunes >= maxRunes {
			return runePrefix(marker, maxRunes)
		}
		keep = maxRunes - markerRunes
		actualOmitted := len(rs) - keep
		if actualOmitted == omitted {
			break
		}
		omitted = actualOmitted
	}
	head := (keep + 1) / 2
	tail := keep - head
	return string(rs[:head]) + marker + string(rs[len(rs)-tail:])
}

func (w *stagingProgressWriter) bodyOmission(count int) string {
	return w.i18n.Tf(MsgStagingBodyOmitted, count)
}

func runePrefix(text string, maxRunes int) string {
	rs := []rune(text)
	if len(rs) <= maxRunes {
		return text
	}
	return string(rs[:maxRunes])
}

func (w *stagingProgressWriter) signalLocked() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *stagingProgressWriter) stopWorkerLocked() {
	w.stopOnce.Do(func() {
		if w.ticker != nil {
			w.ticker.Stop()
		}
		close(w.stopWorker)
		w.cancel()
	})
}

func (w *stagingProgressWriter) withAPITimeout() (context.Context, context.CancelFunc) {
	if _, hasDeadline := w.ctx.Deadline(); hasDeadline {
		return w.ctx, func() {}
	}
	return context.WithTimeout(w.ctx, compactProgressAPITimeout)
}
