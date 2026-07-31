package pi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/tasercake/pi-connect/core"
)

var piJSONHeartbeatInterval = time.Minute

const maxCapturedStderrBytes = 16 * 1024

type boundedStderrBuffer struct {
	buf       bytes.Buffer
	truncated bool
}

func (b *boundedStderrBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := maxCapturedStderrBytes - b.buf.Len()
	if remaining > 0 {
		if remaining > len(p) {
			remaining = len(p)
		}
		_, _ = b.buf.Write(p[:remaining])
	}
	if remaining < len(p) {
		b.truncated = true
	}
	return n, nil
}

func (b *boundedStderrBuffer) String() string {
	if !b.truncated {
		return b.buf.String()
	}
	return b.buf.String() + "\n[stderr truncated]"
}

type stderrStringer interface {
	String() string
}

const (
	attachmentMaxTotalBytes = int64(1 << 30) // 1 GiB
	attachmentMaxAge        = 7 * 24 * time.Hour
)

// piSession manages a multi-turn pi coding agent conversation.
// Each Send() spawns `pi --mode json -p <prompt>`.
// Subsequent turns use `--session <sessionID>` to resume.
type piSession struct {
	cmd       string
	workDir   string
	model     string
	mode      string
	thinking  string // reasoning effort level for --thinking flag
	extraEnv  []string
	events    chan core.Event
	sessionID atomic.Value // stores string
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	alive     atomic.Bool
	// terminalError is set when Pi JSON reports an assistant-level error.
	// Such errors end the underlying one-shot JSON process for pi-connect's
	// purposes: keeping the session alive after the engine consumes EventError
	// can leave no goroutine draining stdout and wedge the child pipe.
	terminalError atomic.Bool

	thinkingBuf strings.Builder // accumulates thinking_delta chunks

	finalTextBuf     strings.Builder // complete final assistant text from message_end/turn_end
	emittedTextDelta bool            // true after a non-empty text_delta was emitted this turn
	inputTokens      int
	outputTokens     int

	usageMu                   sync.Mutex
	contextUsage              *core.ContextUsage
	pendingContextOverflowErr string
	recoveredAfterOverflow    bool
}

func newPiSession(ctx context.Context, cmd, workDir, model, mode, thinking, resumeID string, extraEnv []string) (*piSession, error) {
	sessionCtx, cancel := context.WithCancel(ctx)

	s := &piSession{
		cmd:      cmd,
		workDir:  workDir,
		model:    model,
		mode:     mode,
		thinking: thinking,
		extraEnv: extraEnv,
		events:   make(chan core.Event, 64),
		ctx:      sessionCtx,
		cancel:   cancel,
	}
	s.alive.Store(true)

	if resumeID != "" && resumeID != core.ContinueSession {
		s.sessionID.Store(resumeID)
	}

	return s, nil
}

func (s *piSession) Send(prompt string, images []core.ImageAttachment, files []core.FileAttachment) error {
	s.resetResponseState()

	// Clean up attachments from previous turns.
	cleanAttachments(s.workDir)

	// Images keep their existing inline behavior (Telegram already caps
	// photo sizes; the model can handle them as multimodal input).
	var atFiles []string
	if len(images) > 0 {
		atFiles = append(atFiles, saveImagesToDisk(s.workDir, images)...)
	}
	// File attachments are classified: inline-safe text/code keeps the
	// `@<path>` inline behavior, everything else (PDFs, audio, video,
	// docs, large logs) is surfaced as a path reference in the prompt so
	// the model uses its `read` tool / domain skills instead of bloating
	// the context window. See attachments.go for the classification rules.
	attached := classifyFileAttachments(s.workDir, files)
	atFiles = append(atFiles, attached.inlinePaths...)
	if prefix := buildAttachmentPrefix(attached.referencePaths, files); prefix != "" {
		prompt = prefix + prompt
	}
	if !s.alive.Load() {
		return fmt.Errorf("session is closed")
	}

	args := []string{"--mode", "json", "-p"}

	sid := s.CurrentSessionID()
	if sid != "" {
		args = append(args, "--session", sid)
	}

	if s.model != "" {
		args = append(args, "--model", s.model)
	}

	if s.mode == "yolo" {
		args = append(args, "--auto-approve")
	}

	if s.thinking != "" {
		args = append(args, "--thinking", s.thinking)
	}

	// Pass attachments as @file arguments
	for _, f := range atFiles {
		args = append(args, "@"+f)
	}

	// Append prompt as positional arg
	args = append(args, prompt)

	slog.Debug("piSession: launching", "resume", sid != "", "args", core.RedactArgs(args))

	cmd := exec.CommandContext(s.ctx, s.cmd, args...)
	cmd.Dir = s.workDir
	env := os.Environ()
	if len(s.extraEnv) > 0 {
		env = core.MergeEnv(env, s.extraEnv)
	}
	cmd.Env = env

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("piSession: stdout pipe: %w", err)
	}

	var stderrBuf boundedStderrBuffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("piSession: start: %w", err)
	}

	s.wg.Add(1)
	go s.readLoop(cmd, stdout, &stderrBuf)

	return nil
}

func (s *piSession) CompactContext() error {
	return s.CompactContextWithContext(s.ctx)
}

func (s *piSession) CompactContextWithContext(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if !s.alive.Load() {
		return fmt.Errorf("session is closed")
	}
	sid := s.CurrentSessionID()
	if sid == "" {
		return fmt.Errorf("piSession: cannot compact before session id is known")
	}

	rpc, err := newPiRPCSession(ctx, s.cmd, s.workDir, s.model, s.mode, s.thinking, sid, s.extraEnv)
	if err != nil {
		return err
	}
	defer func() { _ = rpc.Close() }()
	return rpc.CompactContextWithContext(ctx)
}

func (s *piSession) readLoop(cmd *exec.Cmd, stdout io.ReadCloser, stderrBuf stderrStringer) {
	defer s.wg.Done()
	stopHeartbeat := s.startJSONHeartbeat()
	defer stopHeartbeat()

	// Pi's JSON events are small (typically <1KB each). A 10MB Scanner buffer
	// is more than sufficient — no need for the bufio.Reader approach used by
	// adapters that may receive very large single-line responses.
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var raw map[string]any
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			slog.Debug("piSession: non-JSON line", "line", truncStr(line, 100))
			continue
		}

		s.handleEvent(raw)
	}

	scanErr := scanner.Err()
	// Wait only after stdout is fully drained. This makes process status the
	// authoritative terminal boundary and prevents a late deferred Wait error
	// from following an already-emitted successful result.
	waitErr := cmd.Wait()

	// Stop synthetic liveness events before emitting a terminal event, so no
	// stale heartbeat can arrive after the result/error for this turn.
	stopHeartbeat()

	if s.terminalError.Load() || !s.alive.Load() {
		s.resetResponseState()
		return
	}
	if scanErr != nil {
		terminalErr := fmt.Errorf("read stdout: %w", scanErr)
		if waitErr != nil {
			terminalErr = fmt.Errorf("%w; stdout read error: %v", newUnexpectedProcessExitError("pi JSON", cmd, waitErr, stderrBuf.String()), scanErr)
		}
		slog.Error("piSession: scanner error", "error", terminalErr)
		s.emitTerminalError(terminalErr)
		s.resetResponseState()
		return
	}
	if waitErr != nil {
		processErr := newUnexpectedProcessExitError("pi JSON", cmd, waitErr, stderrBuf.String())
		slog.Error("piSession: process failed", "error", processErr)
		s.emitTerminalError(processErr)
		s.resetResponseState()
		return
	}

	if errMsg := s.pendingOverflowErrorForEOF(); errMsg != "" {
		s.emitTerminalError(fmt.Errorf("%s", errMsg))
		s.resetResponseState()
		return
	}

	// Emit EventResult only after a successful process exit.
	sid := s.CurrentSessionID()
	evt := core.Event{
		Type:         core.EventResult,
		SessionID:    sid,
		Done:         true,
		InputTokens:  s.inputTokens,
		OutputTokens: s.outputTokens,
	}
	if !s.emittedTextDelta {
		evt.Content = s.finalTextBuf.String()
	}
	s.resetResponseState()
	select {
	case s.events <- evt:
	case <-s.ctx.Done():
	}
}

func newUnexpectedProcessExitError(label string, cmd *exec.Cmd, waitErr error, stderr string) error {
	parts := make([]string, 0, 4)
	if cmd != nil && cmd.Process != nil {
		parts = append(parts, fmt.Sprintf("pid %d", cmd.Process.Pid))
	}
	if cmd != nil && cmd.ProcessState != nil {
		if code := cmd.ProcessState.ExitCode(); code >= 0 {
			parts = append(parts, fmt.Sprintf("exit code %d", code))
		}
	}
	waitText := strings.TrimSpace(waitErr.Error())
	if waitText != "" && (cmd == nil || cmd.ProcessState == nil || cmd.ProcessState.ExitCode() < 0) {
		parts = append(parts, waitText)
	}
	lower := strings.ToLower(waitText)
	if strings.Contains(lower, "signal: killed") || strings.Contains(lower, "signal 9") || strings.Contains(lower, "sigkill") {
		parts = append(parts, "SIGKILL may indicate OOM or an external kill")
	}
	stderr = strings.TrimSpace(stderr)
	if stderr != "" {
		parts = append(parts, "stderr: "+truncStr(stderr, 2048))
	}
	if len(parts) == 0 {
		parts = append(parts, "unknown process failure")
	}
	return fmt.Errorf("%s process exited unexpectedly (%s)", label, strings.Join(parts, "; "))
}

func (s *piSession) startJSONHeartbeat() func() {
	interval := piJSONHeartbeatInterval
	if interval <= 0 {
		return func() {}
	}

	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				evt := core.Event{
					Type:      core.EventHeartbeat,
					Metadata:  map[string]any{"source": "pi_json_process_alive"},
					Synthetic: true,
				}
				select {
				case s.events <- evt:
				case <-s.ctx.Done():
					return
				case <-done:
					return
				}
			case <-s.ctx.Done():
				return
			case <-done:
				return
			}
		}
	}()

	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			<-stopped
		})
	}
}

// Pi NDJSON event types:
//
//	session           — session metadata with id
//	agent_start/end   — agent lifecycle
//	turn_start/end    — turn boundaries
//	message_start     — beginning of user/assistant/toolResult message
//	message_update    — streaming deltas (assistantMessageEvent sub-events)
//	message_end       — complete message
//	custom_message    — visible extension message (e.g. subagent completion)
func (s *piSession) handleEvent(raw map[string]any) {
	s.updateUsageFromEvent(raw)
	eventType, _ := raw["type"].(string)

	switch eventType {
	case "session":
		if id, ok := raw["id"].(string); ok && id != "" {
			s.sessionID.Store(id)
			slog.Debug("piSession: session started", "session_id", id)
		}

	case "message_update":
		s.handleMessageUpdate(raw)

	case "message":
		s.handleMessageRecord(raw)

	case "message_end":
		s.handleMessageEnd(raw)

	case "custom_message":
		s.handleCustomMessage(raw)

	case "turn_end":
		s.handleTurnEnd(raw)

	case "compaction_start", "compaction_end":
		s.handleCompactionEvent(eventType, raw)

	case "agent_start", "agent_end", "turn_start", "message_start":
		// Logged for debugging but no action needed.
		slog.Debug("piSession: lifecycle event", "type", eventType)

	default:
		slog.Debug("piSession: unhandled event", "type", eventType)
	}
}

func (s *piSession) handleCustomMessage(raw map[string]any) {
	content := customMessageContent(raw)
	if content == "" {
		return
	}
	// Subagent completion notifications are often unsolicited background
	// messages. Emit a complete result so pi-connect's unsolicited reader sends
	// the notification immediately instead of buffering text until a later turn.
	evt := core.Event{Type: core.EventResult, Content: content, Done: true, SessionID: s.CurrentSessionID()}
	select {
	case s.events <- evt:
	case <-s.ctx.Done():
		return
	}
}

func customMessageContent(raw map[string]any) string {
	content, _ := raw["content"].(string)
	if strings.TrimSpace(content) == "" {
		return ""
	}
	customType, _ := raw["customType"].(string)
	display, _ := raw["display"].(bool)
	if customType != "subagent-notify" || !display {
		return ""
	}
	return content
}

func (s *piSession) handleCompactionEvent(eventType string, raw map[string]any) {
	switch eventType {
	case "compaction_start":
		slog.Info("piSession: compaction started", "session_id", s.CurrentSessionID())
		select {
		case s.events <- core.Event{Type: core.EventThinking, Content: "Context compaction started"}:
		case <-s.ctx.Done():
		}
	case "compaction_end":
		if errMsg, _ := raw["errorMessage"].(string); errMsg != "" {
			slog.Warn("piSession: compaction failed", "session_id", s.CurrentSessionID(), "error", errMsg)
			select {
			case s.events <- core.Event{Type: core.EventError, Error: fmt.Errorf("context compaction failed: %s", errMsg)}:
			case <-s.ctx.Done():
			}
			return
		}
		slog.Info("piSession: compaction completed", "session_id", s.CurrentSessionID())
		select {
		case s.events <- core.Event{Type: core.EventThinking, Content: "Context compaction completed"}:
		case <-s.ctx.Done():
		}
	}
}

// handleMessageUpdate processes streaming deltas from pi's assistantMessageEvent.
func (s *piSession) handleMessageUpdate(raw map[string]any) {
	ame, _ := raw["assistantMessageEvent"].(map[string]any)
	if ame == nil {
		return
	}

	subType, _ := ame["type"].(string)

	switch subType {
	case "text_delta":
		delta, _ := ame["delta"].(string)
		if delta != "" {
			s.emittedTextDelta = true
			evt := core.Event{Type: core.EventText, Content: delta}
			select {
			case s.events <- evt:
			case <-s.ctx.Done():
				return
			}
		}

	case "thinking_delta":
		delta, _ := ame["delta"].(string)
		if delta != "" {
			s.thinkingBuf.WriteString(delta)
		}

	case "thinking_end":
		if s.thinkingBuf.Len() > 0 {
			evt := core.Event{Type: core.EventThinking, Content: s.thinkingBuf.String()}
			s.thinkingBuf.Reset()
			select {
			case s.events <- evt:
			case <-s.ctx.Done():
				return
			}
		}

	case "toolcall_end":
		// Extract tool name and input from the accumulated message content.
		s.emitToolFromMessage(ame)
	}
}

// emitToolFromMessage extracts tool call info from a toolcall_end event.
func (s *piSession) emitToolFromMessage(ame map[string]any) {
	msg, _ := ame["message"].(map[string]any)
	if msg == nil {
		msg, _ = ame["partial"].(map[string]any)
	}
	if msg == nil {
		return
	}

	content, _ := msg["content"].([]any)
	idx := int(0)
	if ci, ok := ame["contentIndex"].(float64); ok {
		idx = int(ci)
	}

	if idx >= 0 && idx < len(content) {
		item, _ := content[idx].(map[string]any)
		if item != nil {
			itemType, _ := item["type"].(string)
			if itemType == "toolCall" {
				name, _ := item["name"].(string)
				input := extractToolInput(item)
				evt := core.Event{Type: core.EventToolUse, ToolName: name, ToolInput: input}
				select {
				case s.events <- evt:
				case <-s.ctx.Done():
					return
				}
			}
		}
	}
}

// handleMessageEnd processes completed messages — particularly toolResult messages.
func (s *piSession) handleMessageEnd(raw map[string]any) {
	msg, _ := raw["message"].(map[string]any)
	if msg == nil {
		return
	}

	role, _ := msg["role"].(string)

	switch role {
	case "toolResult":
		toolName, _ := msg["toolName"].(string)
		content, _ := msg["content"].([]any)
		var output string
		for _, c := range content {
			if item, ok := c.(map[string]any); ok {
				if text, ok := item["text"].(string); ok {
					output = text
					break
				}
			}
		}
		evt := core.Event{Type: core.EventToolResult, ToolName: toolName, Content: truncStr(output, 500)}
		select {
		case s.events <- evt:
		case <-s.ctx.Done():
			return
		}

	case "assistant":
		// Check for errors
		if errMsg, _ := msg["errorMessage"].(string); errMsg != "" {
			if isRecoverablePiOverflow(errMsg) {
				s.storePendingOverflowError(errMsg)
				return
			}
			var diagDetails []string
			isTransportFailure := false

			if stopReason, ok := msg["stopReason"].(string); ok && stopReason != "" {
				diagDetails = append(diagDetails, fmt.Sprintf("stopReason=%s", stopReason))
			}

			if det, ok := msg["details"].(map[string]any); ok {
				if phase, ok := det["phase"].(string); ok && phase != "" {
					diagDetails = append(diagDetails, fmt.Sprintf("phase=%s", phase))
				}
				if bytes, ok := det["requestBytes"].(float64); ok && bytes != 0 {
					diagDetails = append(diagDetails, fmt.Sprintf("bytes=%.0f", bytes))
				}
			}

			if diagAny, ok := msg["diagnostics"]; ok {
				switch d := diagAny.(type) {
				case map[string]any:
					if dType, ok := d["type"].(string); ok && dType != "" {
						diagDetails = append(diagDetails, fmt.Sprintf("diag=%s", dType))
						if dType == "provider_transport_failure" {
							isTransportFailure = true
						}
					}
				case []any:
					for _, item := range d {
						if diag, ok := item.(map[string]any); ok {
							if dType, ok := diag["type"].(string); ok && dType != "" {
								diagDetails = append(diagDetails, fmt.Sprintf("diag=%s", dType))
								if dType == "provider_transport_failure" {
									isTransportFailure = true
								}
							}
						}
					}
				}
			}

			fullErr := errMsg
			if len(diagDetails) > 0 {
				fullErr = fmt.Sprintf("%s [%s]", errMsg, strings.Join(diagDetails, ", "))
			}

			if strings.Contains(errMsg, "WebSocket closed 1000") && isTransportFailure {
				fullErr = "transient provider transport failure: " + fullErr + ". Please try again."
			}

			s.emitTerminalError(fmt.Errorf("%s", fullErr))
			return
		}
		s.captureFinalAssistantMessage(msg)
		if assistantMessageSucceeded(msg) {
			s.clearPendingOverflowError()
		}
	}
}

func (s *piSession) handleTurnEnd(raw map[string]any) {
	if msg, ok := raw["message"].(map[string]any); ok && msg != nil {
		s.recordUsageFromMessage(msg)
		s.captureFinalAssistantMessage(msg)
		if assistantMessageSucceeded(msg) {
			s.clearPendingOverflowError()
		}
	}
	slog.Debug("piSession: lifecycle event", "type", "turn_end")
}

func (s *piSession) resetResponseState() {
	s.finalTextBuf.Reset()
	s.emittedTextDelta = false
	s.inputTokens = 0
	s.outputTokens = 0
}

func (s *piSession) captureFinalAssistantMessage(msg map[string]any) {
	s.recordUsageFromMessage(msg)
	if !isFinalAssistantMessage(msg) {
		return
	}
	text := extractTextContent(msg)
	if text != "" {
		s.finalTextBuf.Reset()
		s.finalTextBuf.WriteString(text)
	}
}

func isFinalAssistantMessage(msg map[string]any) bool {
	role, _ := msg["role"].(string)
	if role != "assistant" {
		return false
	}
	if errMsg, _ := msg["errorMessage"].(string); errMsg != "" {
		return false
	}
	stopReason, _ := msg["stopReason"].(string)
	if stopReason == "toolUse" {
		return false
	}
	if stopReason == "stop" {
		return true
	}
	return hasFinalAnswerSignature(msg)
}

func extractTextContent(msg map[string]any) string {
	content, _ := msg["content"].([]any)
	var b strings.Builder
	for _, c := range content {
		item, _ := c.(map[string]any)
		if item == nil {
			continue
		}
		if itemType, _ := item["type"].(string); itemType != "text" {
			continue
		}
		if text, ok := item["text"].(string); ok {
			b.WriteString(text)
		}
	}
	return b.String()
}

func hasFinalAnswerSignature(msg map[string]any) bool {
	content, _ := msg["content"].([]any)
	for _, c := range content {
		item, _ := c.(map[string]any)
		if item == nil {
			continue
		}
		if itemType, _ := item["type"].(string); itemType != "text" {
			continue
		}
		sig, _ := item["textSignature"].(map[string]any)
		if sig == nil {
			continue
		}
		if phase, _ := sig["phase"].(string); phase == "final_answer" {
			return true
		}
	}
	return false
}

func (s *piSession) updateUsageFromEvent(raw map[string]any) {
	if msg, _ := raw["message"].(map[string]any); msg != nil {
		s.recordUsageFromMessage(msg)
	}
	if messages, _ := raw["messages"].([]any); messages != nil {
		for _, item := range messages {
			if msg, _ := item.(map[string]any); msg != nil {
				s.recordUsageFromMessage(msg)
			}
		}
	}
}

func (s *piSession) recordUsageFromMessage(msg map[string]any) {
	u, ok := parsePiUsage(msg)
	if !ok {
		return
	}
	cu := u.contextUsage()
	if cu == nil {
		return
	}
	s.usageMu.Lock()
	s.contextUsage = cu
	s.usageMu.Unlock()
	s.inputTokens = cu.UsedTokens
	s.outputTokens = cu.OutputTokens
}

func (s *piSession) GetContextUsage() *core.ContextUsage {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	return cloneContextUsage(s.contextUsage)
}

func (s *piSession) handleMessageRecord(raw map[string]any) {
	msg, _ := raw["message"].(map[string]any)
	if msg == nil {
		return
	}
	role, _ := msg["role"].(string)
	if role != "assistant" {
		return
	}
	if errMsg, _ := msg["errorMessage"].(string); errMsg != "" {
		if isRecoverablePiOverflow(errMsg) {
			s.storePendingOverflowError(errMsg)
			return
		}
		s.emitTerminalError(fmt.Errorf("%s", errMsg))
		return
	}
	if assistantMessageSucceeded(msg) {
		s.clearPendingOverflowError()
	}
}

func (s *piSession) storePendingOverflowError(errMsg string) {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	s.pendingContextOverflowErr = errMsg
	s.recoveredAfterOverflow = false
}

func (s *piSession) clearPendingOverflowError() {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	if s.pendingContextOverflowErr != "" {
		s.pendingContextOverflowErr = ""
		s.recoveredAfterOverflow = true
	}
}

func (s *piSession) pendingOverflowErrorForEOF() string {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	if s.recoveredAfterOverflow {
		return ""
	}
	return s.pendingContextOverflowErr
}

// extractToolInput pulls a concise summary from a tool call content item.
func extractToolInput(item map[string]any) string {
	args, _ := item["arguments"].(map[string]any)
	if args == nil {
		return ""
	}
	// Prefer description or command fields.
	if desc, ok := args["description"].(string); ok && desc != "" {
		return desc
	}
	if cmd, ok := args["command"].(string); ok && cmd != "" {
		return cmd
	}
	if fp, ok := args["file_path"].(string); ok && fp != "" {
		return fp
	}
	if pattern, ok := args["pattern"].(string); ok && pattern != "" {
		return pattern
	}
	if query, ok := args["query"].(string); ok && query != "" {
		return query
	}
	b, _ := json.Marshal(args)
	return truncStr(string(b), 200)
}

func (s *piSession) emitTerminalError(err error) {
	s.terminalError.Store(true)
	s.alive.Store(false)
	// Cancel first so the heartbeat goroutine stops competing for space while we
	// make the terminal error observable to the engine.
	s.cancel()

	evt := core.Event{Type: core.EventError, Error: err}

	// The terminal error must be observable by the engine: after EventError,
	// core intentionally stops consuming this turn's event stream when the
	// session is dead. If the bounded channel is already full, evict stale
	// buffered events to make room rather than silently dropping the only terminal
	// signal and leaving the foreground loop to wait for the idle timeout.
	for i := 0; i <= cap(s.events); i++ {
		select {
		case s.events <- evt:
			return
		default:
		}
		select {
		case dropped := <-s.events:
			slog.Warn("piSession: evicting buffered event to deliver terminal error", "dropped_type", dropped.Type, "error", err)
		default:
		}
	}

	// This should be unreachable while readLoop owns the still-open events
	// channel, but keep a non-blocking fallback so a terminal path never wedges
	// stdout draining if future code adds another concurrent producer.
	slog.Warn("piSession: terminal error could not be delivered after eviction", "error", err)
}

func (s *piSession) RespondPermission(_ string, _ core.PermissionResult) error {
	return nil
}

func (s *piSession) Events() <-chan core.Event {
	return s.events
}

func (s *piSession) CurrentSessionID() string {
	v, _ := s.sessionID.Load().(string)
	return v
}

func (s *piSession) Alive() bool {
	return s.alive.Load()
}

func (s *piSession) Close() error {
	s.alive.Store(false)
	s.cancel()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		slog.Warn("piSession: close timed out, abandoning wg.Wait")
	}
	close(s.events)
	return nil
}

// cleanAttachments performs bounded GC on the attachments directory to avoid
// unbounded growth while keeping recent files available across turns.
func cleanAttachments(workDir string) {
	cleanAttachmentsWithLimits(workDir, attachmentMaxTotalBytes, attachmentMaxAge, time.Now())
}

func cleanAttachmentsWithLimits(workDir string, maxTotalBytes int64, maxAge time.Duration, now time.Time) {
	attachDir := filepath.Join(workDir, ".pi-connect", "attachments")
	entries, err := os.ReadDir(attachDir)
	if err != nil {
		return // directory may not exist yet
	}
	type attachmentFile struct {
		path    string
		size    int64
		modTime time.Time
	}
	files := make([]attachmentFile, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		path := filepath.Join(attachDir, e.Name())
		if maxAge >= 0 && now.Sub(info.ModTime()) > maxAge {
			if err := os.Remove(path); err != nil {
				slog.Warn("piSession: failed to remove old attachment", "path", path, "error", err)
			}
			continue
		}
		files = append(files, attachmentFile{path: path, size: info.Size(), modTime: info.ModTime()})
	}

	if maxTotalBytes <= 0 {
		return
	}
	var total int64
	for _, f := range files {
		total += f.size
	}
	if total <= maxTotalBytes {
		return
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].modTime.Before(files[j].modTime)
	})
	for _, f := range files {
		if total <= maxTotalBytes {
			break
		}
		if err := os.Remove(f.path); err != nil {
			slog.Warn("piSession: failed to remove attachment over size cap", "path", f.path, "error", err)
			continue
		}
		total -= f.size
	}
}

// saveImagesToDisk saves image attachments to workDir/.pi-connect/attachments/
// and returns the list of absolute file paths.
func saveImagesToDisk(workDir string, images []core.ImageAttachment) []string {
	attachDir := filepath.Join(workDir, ".pi-connect", "attachments")
	if err := os.MkdirAll(attachDir, 0o755); err != nil {
		slog.Error("piSession: failed to create attachments dir", "error", err)
		return nil
	}

	var paths []string
	for i, img := range images {
		ext := ".png"
		switch img.MimeType {
		case "image/jpeg":
			ext = ".jpg"
		case "image/gif":
			ext = ".gif"
		case "image/webp":
			ext = ".webp"
		}
		fname := img.FileName
		if fname == "" {
			fname = fmt.Sprintf("image_%d_%d%s", time.Now().UnixMilli(), i, ext)
		}
		fpath := filepath.Join(attachDir, fname)
		if err := os.WriteFile(fpath, img.Data, 0o644); err != nil {
			slog.Error("piSession: save image failed", "error", err)
			continue
		}
		paths = append(paths, fpath)
	}
	return paths
}

func truncStr(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	return string([]rune(s)[:maxRunes]) + "..."
}
