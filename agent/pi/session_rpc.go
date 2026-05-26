package pi

// piRPCSession drives `pi --mode rpc` as a long-lived subprocess.
//
// Why this exists:
//   - JSON mode (`pi --mode json -p <prompt>`) spawns a fresh process per turn.
//     That works for plain prompts but breaks extension commands (e.g. /goals,
//     /sisyphus from @capyup/pi-goal) because they assume a persistent extension
//     runtime and a UI surface that JSON mode doesn't provide.
//   - RPC mode keeps one pi process alive for the whole session, dispatches
//     extension commands properly, and exposes an extension UI sub-protocol we
//     can bridge.
//
// Wire shape (NOT JSON-RPC 2.0 — pi's own JSONL protocol):
//   Commands (stdin):  {"id": "...", "type": "prompt", "message": "..."}
//   Responses (stdout): {"id": "...", "type": "response", "command": "prompt", "success": true}
//   Events (stdout):    {"type": "message_update", ...}  (no id)
//   UI requests (stdout): {"type": "extension_ui_request", "id": "...", "method": "..."}
//   UI replies (stdin):   {"type": "extension_ui_response", "id": "...", "value": "..."}
//
// Framing rules per pi docs: strict JSONL, LF-only delimiter. Do not use a
// generic Unicode line reader. bufio.Scanner with ScanLines is safe.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// piRPCSession is the long-lived analogue of piSession.
// It implements the same core.AgentSession surface.
type piRPCSession struct {
	cmd      string
	workDir  string
	model    string
	mode     string
	thinking string
	extraEnv []string

	events    chan core.Event
	sessionID atomic.Value // string
	ctx       context.Context
	cancel    context.CancelFunc
	alive     atomic.Bool

	startOnce sync.Once
	startErr  error

	// subprocess state — guarded by procMu.
	procMu sync.Mutex
	proc   *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr *bytes.Buffer

	// transport state.
	writeMu sync.Mutex // serializes stdin writes
	nextID  atomic.Int64

	pendingMu sync.Mutex
	pending   map[string]chan rpcResponse

	// streaming state (when isStreaming, prompts must specify steer/follow_up).
	streamMu    sync.Mutex
	isStreaming bool

	// thinking accumulation buffer.
	thinkingBuf strings.Builder

	wg sync.WaitGroup
}

type rpcResponse struct {
	success bool
	data    json.RawMessage
	errMsg  string
}

// extensionUIBridge handles extension_ui_request events. Implementations decide
// how to surface dialog requests to the human (auto-cancel, chat-text fallback,
// Telegram inline keyboards, ...).
type extensionUIBridge interface {
	// handle is invoked from the read loop. The bridge must call `reply` exactly
	// once for dialog methods; for fire-and-forget methods, no reply.
	handle(req extensionUIRequest, reply func(payload map[string]any) error)
}

type extensionUIRequest struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Raw    json.RawMessage `json:"-"`
}

func newPiRPCSession(ctx context.Context, cmd, workDir, model, mode, thinking, resumeID string, extraEnv []string) (*piRPCSession, error) {
	sessCtx, cancel := context.WithCancel(ctx)
	s := &piRPCSession{
		cmd:      cmd,
		workDir:  workDir,
		model:    model,
		mode:     mode,
		thinking: thinking,
		extraEnv: extraEnv,
		events:   make(chan core.Event, 64),
		ctx:      sessCtx,
		cancel:   cancel,
		pending:  make(map[string]chan rpcResponse),
	}
	s.alive.Store(true)
	if resumeID != "" && resumeID != core.ContinueSession {
		s.sessionID.Store(resumeID)
	}
	return s, nil
}

// startProcess spawns `pi --mode rpc` and starts the read loop. Idempotent.
func (s *piRPCSession) startProcess() error {
	s.startOnce.Do(func() {
		s.startErr = s.doStart()
	})
	return s.startErr
}

func (s *piRPCSession) doStart() error {
	args := []string{"--mode", "rpc"}
	if sid, _ := s.sessionID.Load().(string); sid != "" {
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

	slog.Debug("piRPCSession: launching", "args", core.RedactArgs(args))

	cmd := exec.CommandContext(s.ctx, s.cmd, args...)
	cmd.Dir = s.workDir
	env := os.Environ()
	if len(s.extraEnv) > 0 {
		env = core.MergeEnv(env, s.extraEnv)
	}
	cmd.Env = env

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("piRPCSession: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("piRPCSession: stdout pipe: %w", err)
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("piRPCSession: start: %w", err)
	}

	s.procMu.Lock()
	s.proc = cmd
	s.stdin = stdin
	s.stdout = stdout
	s.stderr = &stderrBuf
	s.procMu.Unlock()

	s.wg.Add(1)
	go s.readLoop()

	// Reaper goroutine: when the process exits, mark closed and emit final event.
	go func() {
		err := cmd.Wait()
		s.alive.Store(false)
		stderrMsg := strings.TrimSpace(stderrBuf.String())
		if err != nil && stderrMsg != "" {
			slog.Error("piRPCSession: process exited", "error", err, "stderr", truncStr(stderrMsg, 300))
			s.tryEmit(core.Event{Type: core.EventError, Error: fmt.Errorf("%s", stderrMsg)})
		} else if err != nil {
			slog.Warn("piRPCSession: process exited", "error", err)
		}
		// Cancel any pending RPC calls so callers unblock.
		s.cancelPending(fmt.Errorf("pi process exited"))
	}()

	return nil
}

// readLoop drains stdout, demuxing responses, events, and UI requests.
func (s *piRPCSession) readLoop() {
	defer s.wg.Done()
	s.procMu.Lock()
	stdout := s.stdout
	s.procMu.Unlock()
	if stdout == nil {
		return
	}

	// Pi events can include large blobs (e.g. base64 image content in user
	// messages echoed back). Match JSON-mode buffer size.
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	// Default ScanLines splits on \n and strips trailing \r. That matches
	// pi's LF-only framing while tolerating CRLF if a wrapper introduces it.

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		s.dispatchLine(line)
	}
	if err := scanner.Err(); err != nil {
		slog.Debug("piRPCSession: stdout scanner closed", "error", err)
	}
	// Emit final EventResult when readLoop returns.
	s.tryEmit(core.Event{Type: core.EventResult, SessionID: s.CurrentSessionID(), Done: true})
}

func (s *piRPCSession) dispatchLine(line []byte) {
	var env struct {
		Type string          `json:"type"`
		ID   json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(line, &env); err != nil {
		slog.Debug("piRPCSession: non-JSON line", "line", truncBytes(line, 120))
		return
	}

	switch env.Type {
	case "response":
		s.handleResponse(line)
	case "extension_ui_request":
		s.handleUIRequest(line)
	case "session":
		var sess struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(line, &sess)
		if sess.ID != "" {
			s.sessionID.Store(sess.ID)
		}
	default:
		// Generic agent event (message_update, message_end, tool_*, etc.).
		// Reuse JSON-mode event handling.
		var raw map[string]any
		if err := json.Unmarshal(line, &raw); err == nil {
			s.handleAgentEvent(raw)
		}
	}
}

// handleAgentEvent reuses the JSON-mode event semantics. It's intentionally a
// thin wrapper around the existing piSession event handlers so we don't drift.
func (s *piRPCSession) handleAgentEvent(raw map[string]any) {
	eventType, _ := raw["type"].(string)
	switch eventType {
	case "message_update":
		s.rpcHandleMessageUpdate(raw)
	case "message_end":
		s.rpcHandleMessageEnd(raw)
	case "agent_start":
		s.streamMu.Lock()
		s.isStreaming = true
		s.streamMu.Unlock()
	case "agent_end":
		s.streamMu.Lock()
		s.isStreaming = false
		s.streamMu.Unlock()
	case "extension_error":
		errMsg, _ := raw["error"].(string)
		extPath, _ := raw["extensionPath"].(string)
		s.tryEmit(core.Event{Type: core.EventError, Error: fmt.Errorf("extension %s: %s", filepath.Base(extPath), errMsg)})
	case "turn_end":
		s.rpcHandleTurnEnd(raw)
	case "compaction_start", "compaction_end":
		s.rpcHandleCompactionEvent(eventType, raw)
	}
}

func (s *piRPCSession) rpcHandleCompactionEvent(eventType string, raw map[string]any) {
	switch eventType {
	case "compaction_start":
		slog.Info("piRPCSession: compaction started", "session_id", s.CurrentSessionID())
		s.tryEmit(core.Event{Type: core.EventThinking, Content: "Context compaction started"})
	case "compaction_end":
		if errMsg, _ := raw["errorMessage"].(string); errMsg != "" {
			slog.Warn("piRPCSession: compaction failed", "session_id", s.CurrentSessionID(), "error", errMsg)
			s.tryEmit(core.Event{Type: core.EventError, Error: fmt.Errorf("context compaction failed: %s", errMsg)})
			return
		}
		slog.Info("piRPCSession: compaction completed", "session_id", s.CurrentSessionID())
		s.tryEmit(core.Event{Type: core.EventThinking, Content: "Context compaction completed"})
	}
}

// rpcHandleTurnEnd emits EventResult{Done: true} so the engine flushes the
// buffered text-delta segments to the platform and marks the turn complete.
// Required for RPC mode because the pi process is persistent across turns —
// the readLoop's terminal EventResult only fires on session close, never per
// turn. Without this, text replies never reach the user and the typing
// indicator stays on forever.
func (s *piRPCSession) rpcHandleTurnEnd(raw map[string]any) {
	evt := core.Event{Type: core.EventResult, SessionID: s.CurrentSessionID(), Done: true}
	if msg, ok := raw["message"].(map[string]any); ok && msg != nil {
		if content, ok := msg["content"].([]any); ok {
			var b strings.Builder
			for _, c := range content {
				item, _ := c.(map[string]any)
				if item == nil {
					continue
				}
				if t, _ := item["type"].(string); t != "text" {
					continue
				}
				if text, ok := item["text"].(string); ok {
					b.WriteString(text)
				}
			}
			evt.Content = b.String()
		}
		if usage, ok := msg["usage"].(map[string]any); ok {
			if v, ok := usage["input"].(float64); ok {
				evt.InputTokens = int(v)
			}
			if v, ok := usage["output"].(float64); ok {
				evt.OutputTokens = int(v)
			}
		}
	}
	s.tryEmit(evt)
}

func (s *piRPCSession) rpcHandleMessageUpdate(raw map[string]any) {
	ame, _ := raw["assistantMessageEvent"].(map[string]any)
	if ame == nil {
		return
	}
	subType, _ := ame["type"].(string)
	switch subType {
	case "text_delta":
		if delta, _ := ame["delta"].(string); delta != "" {
			s.tryEmit(core.Event{Type: core.EventText, Content: delta})
		}
	case "thinking_delta":
		if delta, _ := ame["delta"].(string); delta != "" {
			s.thinkingBuf.WriteString(delta)
		}
	case "thinking_end":
		if s.thinkingBuf.Len() > 0 {
			s.tryEmit(core.Event{Type: core.EventThinking, Content: s.thinkingBuf.String()})
			s.thinkingBuf.Reset()
		}
	case "toolcall_end":
		s.rpcEmitToolFromMessage(ame)
	}
}

func (s *piRPCSession) rpcEmitToolFromMessage(ame map[string]any) {
	msg, _ := ame["message"].(map[string]any)
	if msg == nil {
		msg, _ = ame["partial"].(map[string]any)
	}
	if msg == nil {
		return
	}
	content, _ := msg["content"].([]any)
	idx := 0
	if ci, ok := ame["contentIndex"].(float64); ok {
		idx = int(ci)
	}
	if idx < 0 || idx >= len(content) {
		return
	}
	item, _ := content[idx].(map[string]any)
	if item == nil {
		return
	}
	if t, _ := item["type"].(string); t != "toolCall" {
		return
	}
	name, _ := item["name"].(string)
	input := extractToolInput(item)
	s.tryEmit(core.Event{Type: core.EventToolUse, ToolName: name, ToolInput: input})
}

func (s *piRPCSession) rpcHandleMessageEnd(raw map[string]any) {
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
		s.tryEmit(core.Event{Type: core.EventToolResult, ToolName: toolName, Content: truncStr(output, 500)})
	case "assistant":
		if errMsg, _ := msg["errorMessage"].(string); errMsg != "" {
			s.tryEmit(core.Event{Type: core.EventError, Error: fmt.Errorf("%s", errMsg)})
		}
	}
}

func (s *piRPCSession) handleResponse(line []byte) {
	var resp struct {
		ID      json.RawMessage `json:"id"`
		Success bool            `json:"success"`
		Data    json.RawMessage `json:"data"`
		Command string          `json:"command"`
		Error   string          `json:"error"`
		Message string          `json:"message"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		slog.Debug("piRPCSession: bad response line", "err", err)
		return
	}
	key := idKey(resp.ID)
	if key == "" {
		return
	}
	s.pendingMu.Lock()
	ch, ok := s.pending[key]
	delete(s.pending, key)
	s.pendingMu.Unlock()
	if !ok {
		slog.Debug("piRPCSession: unmatched response", "id", key, "command", resp.Command)
		return
	}
	errMsg := resp.Error
	if errMsg == "" {
		errMsg = resp.Message
	}
	select {
	case ch <- rpcResponse{success: resp.Success, data: resp.Data, errMsg: errMsg}:
	default:
	}
}

// handleUIRequest auto-cancels all dialog methods (Phase 1 strategy).
// Fire-and-forget methods are logged and dropped. Phase 2 will add a real
// bridge here (chat-text fallback or Telegram inline keyboard).
func (s *piRPCSession) handleUIRequest(line []byte) {
	var req struct {
		ID     string `json:"id"`
		Method string `json:"method"`
		Title  string `json:"title"`
	}
	if err := json.Unmarshal(line, &req); err != nil {
		return
	}
	slog.Debug("piRPCSession: extension UI request", "method", req.Method, "id", req.ID, "title", req.Title)

	switch req.Method {
	case "select", "confirm", "input", "editor":
		// Auto-cancel dialog. Extension sees cancelled=true; most well-behaved
		// extensions then fall back to a safe default (refuse-by-default for
		// confirm, no-op for select, etc.).
		_ = s.writeJSON(map[string]any{
			"type":      "extension_ui_response",
			"id":        req.ID,
			"cancelled": true,
		})
	default:
		// Fire-and-forget (notify, setStatus, setWidget, setTitle, ...).
		// Surface notifications as text events so the user at least sees them.
		var notify struct {
			Method     string `json:"method"`
			Message    string `json:"message"`
			NotifyType string `json:"notifyType"`
		}
		if err := json.Unmarshal(line, &notify); err == nil && notify.Method == "notify" && notify.Message != "" {
			prefix := "ℹ️ "
			if notify.NotifyType == "warning" {
				prefix = "⚠️ "
			} else if notify.NotifyType == "error" {
				prefix = "❌ "
			}
			s.tryEmit(core.Event{Type: core.EventText, Content: prefix + notify.Message + "\n"})
		}
		// All other fire-and-forget UI methods (setStatus, setWidget, setTitle,
		// setHeader, setFooter, ...) are TUI affordances; nothing to do.
	}
}

// Send implements core.AgentSession.Send. It posts a prompt RPC command and
// returns immediately; events stream asynchronously via Events().
func (s *piRPCSession) Send(prompt string, images []core.ImageAttachment, files []core.FileAttachment) error {
	if !s.alive.Load() {
		return fmt.Errorf("session is closed")
	}
	if err := s.startProcess(); err != nil {
		return err
	}

	cleanAttachments(s.workDir)
	var atFiles []string
	if len(images) > 0 {
		atFiles = append(atFiles, saveImagesToDisk(s.workDir, images)...)
	}
	// File attachments are classified: inline-safe text/code stays on the
	// `@<path>` inline path; everything else is surfaced as a path-only
	// reference in the message body so the model uses its `read` tool /
	// domain skills rather than blowing the context window. See
	// attachments.go for classification rules and rationale.
	attached := classifyFileAttachments(s.workDir, files)
	atFiles = append(atFiles, attached.inlinePaths...)
	body := prompt
	if refPrefix := buildAttachmentPrefix(attached.referencePaths, files); refPrefix != "" {
		body = refPrefix + body
	}
	// Pi RPC mode's `prompt` command doesn't accept @file as a separate
	// argument; we prefix `@<path>` tokens into the message text instead.
	// Pi expands those references identically to the CLI's positional form.
	msg := body
	if len(atFiles) > 0 {
		var b strings.Builder
		for _, f := range atFiles {
			b.WriteString("@")
			b.WriteString(f)
			b.WriteString(" ")
		}
		b.WriteString(body)
		msg = b.String()
	}

	// Decide command + streamingBehavior. If we're mid-stream, the prompt must
	// be sent as steering (well-behaved IM clients steer when the user types
	// during a turn). Extension commands bypass this: they always go through
	// `prompt` regardless of streaming.
	isExt := isLikelyExtensionCommand(prompt)
	s.streamMu.Lock()
	streaming := s.isStreaming
	s.streamMu.Unlock()

	var cmd map[string]any
	if isExt || !streaming {
		cmd = map[string]any{
			"type":    "prompt",
			"message": msg,
		}
		if streaming && isExt {
			// Extension commands execute immediately even during streaming —
			// no streamingBehavior needed per pi docs.
		} else if streaming {
			cmd["streamingBehavior"] = "steer"
		}
	} else {
		cmd = map[string]any{"type": "prompt", "message": msg, "streamingBehavior": "steer"}
	}

	resp, err := s.call(cmd)
	if err != nil {
		return fmt.Errorf("piRPCSession: prompt failed: %w", err)
	}
	if !resp.success {
		return fmt.Errorf("piRPCSession: pi rejected prompt: %s", resp.errMsg)
	}
	return nil
}

func (s *piRPCSession) CompactContext() error {
	if !s.alive.Load() {
		return fmt.Errorf("session is closed")
	}
	if err := s.startProcess(); err != nil {
		return err
	}
	resp, err := s.call(map[string]any{"type": "compact"})
	if err != nil {
		return fmt.Errorf("piRPCSession: compact failed: %w", err)
	}
	if !resp.success {
		return fmt.Errorf("piRPCSession: pi rejected compact: %s", resp.errMsg)
	}
	return nil
}

// isLikelyExtensionCommand returns true when the message starts with `/` but is
// not a built-in pi TUI command (which RPC ignores anyway) and not a known
// expansion prefix (skills, prompt templates). Used only to skip the
// streamingBehavior guard.
func isLikelyExtensionCommand(msg string) bool {
	m := strings.TrimSpace(msg)
	if !strings.HasPrefix(m, "/") {
		return false
	}
	rest := strings.SplitN(m, " ", 2)[0]
	if strings.HasPrefix(rest, "/skill:") {
		return false
	}
	return true
}

// call writes an RPC command (assigning an id) and waits for the matching response.
func (s *piRPCSession) call(payload map[string]any) (rpcResponse, error) {
	id := fmt.Sprintf("%d", s.nextID.Add(1))
	payload["id"] = id
	ch := make(chan rpcResponse, 1)
	s.pendingMu.Lock()
	s.pending[id] = ch
	s.pendingMu.Unlock()

	if err := s.writeJSON(payload); err != nil {
		s.pendingMu.Lock()
		delete(s.pending, id)
		s.pendingMu.Unlock()
		return rpcResponse{}, err
	}
	select {
	case out := <-ch:
		return out, nil
	case <-s.ctx.Done():
		s.pendingMu.Lock()
		delete(s.pending, id)
		s.pendingMu.Unlock()
		return rpcResponse{}, s.ctx.Err()
	case <-time.After(30 * time.Second):
		s.pendingMu.Lock()
		delete(s.pending, id)
		s.pendingMu.Unlock()
		return rpcResponse{}, fmt.Errorf("rpc call timed out (id=%s)", id)
	}
}

func (s *piRPCSession) writeJSON(v any) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.procMu.Lock()
	stdin := s.stdin
	s.procMu.Unlock()
	if stdin == nil {
		return errors.New("piRPCSession: stdin not ready")
	}
	enc := json.NewEncoder(stdin)
	return enc.Encode(v)
}

func (s *piRPCSession) cancelPending(err error) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	msg := err.Error()
	for k, ch := range s.pending {
		select {
		case ch <- rpcResponse{success: false, errMsg: msg}:
		default:
		}
		delete(s.pending, k)
	}
}

func (s *piRPCSession) tryEmit(evt core.Event) {
	select {
	case s.events <- evt:
	case <-s.ctx.Done():
	}
}

func (s *piRPCSession) RespondPermission(_ string, _ core.PermissionResult) error {
	return nil
}

func (s *piRPCSession) Events() <-chan core.Event { return s.events }

func (s *piRPCSession) CurrentSessionID() string {
	v, _ := s.sessionID.Load().(string)
	return v
}

func (s *piRPCSession) Alive() bool { return s.alive.Load() }

func (s *piRPCSession) Close() error {
	s.alive.Store(false)
	// Try a graceful shutdown by closing stdin first.
	s.procMu.Lock()
	stdin := s.stdin
	proc := s.proc
	s.procMu.Unlock()
	if stdin != nil {
		_ = stdin.Close()
	}
	if proc != nil && proc.Process != nil {
		_ = proc.Process.Signal(os.Interrupt)
	}
	s.cancel()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		if proc != nil && proc.Process != nil {
			_ = proc.Process.Kill()
		}
		slog.Warn("piRPCSession: close timed out, killed")
	}
	close(s.events)
	return nil
}

// idKey converts a JSON id (string or number) to a stable map key.
func idKey(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return string(n)
	}
	return string(raw)
}

func truncBytes(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
