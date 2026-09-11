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
	now       func() time.Time
	newTicker func(time.Duration) stagingTicker
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
// Its mutex serializes event-driven edits with elapsed-time ticker edits.
type stagingProgressWriter struct {
	mu sync.Mutex

	ctx       context.Context
	platform  Platform
	replyCtx  any
	starter   PreviewStarter
	updater   MessageUpdater
	transform func(string) string
	i18n      *I18n
	now       func() time.Time
	startTime time.Time

	handle            any
	entries           []stagingEntry
	omittedEntries    int
	toolCount         int
	stepCount         int
	state             stagingProgressState
	enabled           bool
	failed            bool
	closed            bool
	lastSent          string
	lastUpdateAt      time.Time
	minUpdateInterval time.Duration

	newTicker  func(time.Duration) stagingTicker
	ticker     stagingTicker
	stopTicker chan struct{}
	stopOnce   sync.Once
}

func newStagingProgressWriter(ctx context.Context, p Platform, replyCtx any, startTime time.Time, lang Language, transform func(string) string, options *stagingProgressOptions) *stagingProgressWriter {
	w := &stagingProgressWriter{
		ctx:        ctx,
		platform:   p,
		replyCtx:   replyCtx,
		transform:  transform,
		i18n:       NewI18n(lang),
		startTime:  startTime,
		state:      stagingStateRunning,
		stopTicker: make(chan struct{}),
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
	return w
}

// Start promptly creates the sole staging message with a zero-count header.
func (w *stagingProgressWriter) Start() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.enabled && !w.failed && !w.closed {
		return true
	}
	if w.starter == nil || w.updater == nil || w.closed || w.failed {
		w.stopTimerLocked()
		return false
	}
	content := w.renderLocked()
	callCtx, cancel := w.withAPITimeoutLocked()
	handle, err := w.starter.SendPreviewStart(callCtx, w.replyCtx, content)
	cancel()
	if err != nil || handle == nil {
		slog.Warn("staging progress: SendPreviewStart failed", "platform", w.platform.Name(), "error", err, "handle_nil", handle == nil)
		w.failed = true
		w.stopTimerLocked()
		return false
	}
	w.handle = handle
	w.enabled = true
	w.lastSent = content
	w.lastUpdateAt = w.now()
	w.ticker = w.newTicker(stagingProgressTick)
	go w.runTicker()
	return true
}

func (w *stagingProgressWriter) Active() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.enabled && !w.failed && !w.closed && w.state == stagingStateRunning
}

func (w *stagingProgressWriter) AppendThinking(text string) bool {
	return w.append(stagingEntry{kind: stagingEntryThinking, body: text}, false, false)
}

func (w *stagingProgressWriter) AppendToolUse(number int, name, input string) bool {
	return w.append(stagingEntry{kind: stagingEntryToolUse, toolNo: number, tool: name, body: input}, false, false)
}

func (w *stagingProgressWriter) AppendToolResult(name, result, status string, exitCode *int, success *bool) bool {
	return w.append(stagingEntry{kind: stagingEntryToolResult, tool: name, body: result, status: status, exitCode: exitCode, success: success}, false, false)
}

func (w *stagingProgressWriter) AppendText(text string) bool {
	return w.append(stagingEntry{kind: stagingEntryText, body: text}, true, false)
}

func (w *stagingProgressWriter) AppendPermission(name, input string) bool {
	// Permission prompt is sent immediately after this returns, so force this
	// security-relevant timeline update through any normal progress throttle.
	return w.append(stagingEntry{kind: stagingEntryPermission, tool: name, body: input}, false, true)
}

func (w *stagingProgressWriter) AppendError(text string) bool {
	return w.append(stagingEntry{kind: stagingEntryError, body: text}, false, false)
}

func (w *stagingProgressWriter) append(entry stagingEntry, coalesceText, force bool) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.enabled || w.failed || w.closed || w.state != stagingStateRunning {
		return false
	}
	if entry.kind != stagingEntryText {
		entry.body = strings.TrimSpace(entry.body)
	}
	entry.tool = middleTruncateStaging(strings.TrimSpace(entry.tool), stagingProgressToolRunes, w.bodyOmission)
	entry.status = middleTruncateStaging(strings.TrimSpace(entry.status), stagingProgressStatusRunes, w.bodyOmission)
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
	return w.updateLocked(force)
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
	if w.closed {
		return w.enabled && !w.failed && w.state == state
	}
	if !w.enabled || w.failed {
		w.stopTimerLocked()
		return false
	}
	w.state = state
	ok := w.updateLocked(true)
	w.closed = true
	w.stopTimerLocked()
	return ok
}

// Stop only releases timer resources. Terminal paths call Finalize explicitly.
func (w *stagingProgressWriter) Stop() {
	w.mu.Lock()
	w.closed = true
	w.stopTimerLocked()
	w.mu.Unlock()
}

func (w *stagingProgressWriter) runTicker() {
	for {
		select {
		case <-w.ticker.Chan():
			w.mu.Lock()
			if w.enabled && !w.failed && !w.closed && w.state == stagingStateRunning {
				w.updateLocked(false)
			}
			w.mu.Unlock()
		case <-w.stopTicker:
			return
		case <-w.ctx.Done():
			w.mu.Lock()
			w.stopTimerLocked()
			w.mu.Unlock()
			return
		}
	}
}

func (w *stagingProgressWriter) updateLocked(force bool) bool {
	content := w.renderLocked()
	if content == w.lastSent {
		return true
	}
	if !force && w.minUpdateInterval > 0 && w.now().Sub(w.lastUpdateAt) < w.minUpdateInterval {
		return true
	}
	callCtx, cancel := w.withAPITimeoutLocked()
	err := w.updater.UpdateMessage(callCtx, w.handle, content)
	cancel()
	if err != nil {
		slog.Warn("staging progress: UpdateMessage failed", "platform", w.platform.Name(), "error", err)
		w.failed = true
		w.stopTimerLocked()
		return false
	}
	w.lastSent = content
	w.lastUpdateAt = w.now()
	return true
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

func (w *stagingProgressWriter) stopTimerLocked() {
	w.stopOnce.Do(func() {
		if w.ticker != nil {
			w.ticker.Stop()
		}
		close(w.stopTicker)
	})
}

func (w *stagingProgressWriter) withAPITimeoutLocked() (context.Context, context.CancelFunc) {
	if _, hasDeadline := w.ctx.Deadline(); hasDeadline {
		return w.ctx, func() {}
	}
	return context.WithTimeout(w.ctx, compactProgressAPITimeout)
}
