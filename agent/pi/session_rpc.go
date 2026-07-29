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

	"github.com/tasercake/pi-connect/core"
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

	events                 chan core.Event
	eventsMu               sync.RWMutex
	emitMu                 sync.Mutex // reserves one channel slot for lifecycle events
	eventsClosed           bool
	closeOnce              sync.Once
	transportOnce          sync.Once
	sessionID              atomic.Value // string
	ctx                    context.Context
	cancel                 context.CancelFunc
	alive                  atomic.Bool
	closing                atomic.Bool
	suppressTransportEvent atomic.Bool

	startOnce sync.Once
	startErr  error

	// subprocess state — guarded by procMu.
	procMu sync.Mutex
	proc   *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr *bytes.Buffer

	// transport state.
	writeGate chan struct{} // serializes stdin writes and permits cancellation while queued
	nextID    atomic.Int64

	pendingMu sync.Mutex
	pending   map[string]*rpcPending

	// Pi prompt/compact preflights mutate session state and are not safe to
	// replay. The gate serializes their acceptance windows; Pi's own automatic
	// compaction remains inside one prompt command and does not acquire it.
	mutatingGate chan struct{}

	// Slow acceptance is diagnostic only. It never resolves or removes a call.
	slowAcceptanceAfter time.Duration
	slowAcceptanceLog   func(command string, elapsed time.Duration)

	// streaming state (when isStreaming, prompts must specify steer/follow_up).
	streamMu    sync.Mutex
	isStreaming bool

	// promptPending covers extension errors emitted while Send waits for its
	// authoritative prompt ACK, before agent_start establishes turn state.
	promptPending bool // guarded by turnMu

	// thinking accumulation buffer.
	thinkingBuf strings.Builder

	// Pi 0.82.1 makes agent_settled the only terminal prompt boundary. All
	// assistant text/errors remain private here until that event, so a failed
	// attempt cannot escape before Pi's native retry/compaction machinery runs.
	turnMu sync.Mutex
	turn   rpcTurnState

	usageMu                   sync.Mutex
	contextUsage              *core.ContextUsage
	pendingContextOverflowErr string
	recoveredAfterOverflow    bool

	wg sync.WaitGroup
}

type rpcResponse struct {
	success bool
	data    json.RawMessage
	errMsg  string
}

type rpcCallResult struct {
	response rpcResponse
	err      error
}

type rpcPending struct {
	ch       chan rpcCallResult
	mutating bool
}

// RPCAcceptanceUnknownError means a mutating command was written completely,
// but cancellation or transport/process loss happened before its matched ACK.
// Callers must not automatically resend it: Pi may already have accepted and
// executed the command.
type RPCAcceptanceUnknownError struct {
	Command string
	Cause   error
}

func (e *RPCAcceptanceUnknownError) Error() string {
	return fmt.Sprintf("pi RPC %s acceptance outcome unknown: %v", e.Command, e.Cause)
}
func (e *RPCAcceptanceUnknownError) Unwrap() error        { return e.Cause }
func (e *RPCAcceptanceUnknownError) OutcomeUnknown() bool { return true }

// RPCCommandNotSentError means no command bytes reached Pi. Unlike an unknown
// acceptance outcome, retrying may be safe if the caller otherwise permits it.
type RPCCommandNotSentError struct {
	Command string
	Cause   error
}

func (e *RPCCommandNotSentError) Error() string {
	return fmt.Sprintf("pi RPC %s not sent: %v", e.Command, e.Cause)
}
func (e *RPCCommandNotSentError) Unwrap() error { return e.Cause }

type rpcCallTimeoutError struct {
	command string
	id      string
	timeout time.Duration
}

type rpcWriteNotStartedError struct{ cause error }

func (e *rpcWriteNotStartedError) Error() string { return e.cause.Error() }
func (e *rpcWriteNotStartedError) Unwrap() error { return e.cause }

func (e *rpcCallTimeoutError) Error() string {
	return fmt.Sprintf("pi RPC %s call timed out after %s (id=%s)", e.command, e.timeout, e.id)
}

type rpcTurnState struct {
	active              bool
	attemptOutput       string
	attemptSucceeded    bool
	streamed            strings.Builder
	committed           strings.Builder
	committedSuccess    bool
	attemptErr          string
	finalErr            string
	attemptUsage        piUsage
	attemptUsages       []piUsage
	pendingExtensionErr string
	deferredCustom      []string
	finalizedSeq        uint64
}

// rpcProviderError is a settled assistant/provider-turn failure. The RPC
// transport and persistent Pi process are still healthy.
type rpcProviderError struct{ message string }

func (e *rpcProviderError) Error() string { return e.message }

// rpcTransportError means stdout/process/protocol liveness was lost and the
// persistent session can no longer accept another turn.
type rpcTransportError struct{ cause error }

func (e *rpcTransportError) Error() string { return e.cause.Error() }
func (e *rpcTransportError) Unwrap() error { return e.cause }

type rpcExtensionError struct {
	extension string
	message   string
}

func (e *rpcExtensionError) Error() string {
	return fmt.Sprintf("extension %s: %s", e.extension, e.message)
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
		cmd:                 cmd,
		workDir:             workDir,
		model:               model,
		mode:                mode,
		thinking:            thinking,
		extraEnv:            extraEnv,
		events:              make(chan core.Event, 64),
		ctx:                 sessCtx,
		cancel:              cancel,
		pending:             make(map[string]*rpcPending),
		writeGate:           make(chan struct{}, 1),
		mutatingGate:        make(chan struct{}, 1),
		slowAcceptanceAfter: 30 * time.Second,
	}
	s.writeGate <- struct{}{}
	s.mutatingGate <- struct{}{}
	s.slowAcceptanceLog = func(command string, elapsed time.Duration) {
		slog.Warn("piRPCSession: slow command acceptance",
			"command", command,
			"elapsed", elapsed,
			"session_id", s.CurrentSessionID(),
		)
	}
	s.alive.Store(true)
	if resumeID != "" && resumeID != core.ContinueSession {
		s.sessionID.Store(resumeID)
	}
	return s, nil
}

// startProcess spawns `pi --mode rpc` and starts the read loop. Idempotent.
func (s *piRPCSession) startProcess() error {
	return s.startProcessContext(s.ctx)
}

func (s *piRPCSession) startProcessContext(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.startOnce.Do(func() {
		s.startErr = s.doStart(ctx)
	})
	return s.startErr
}

func (s *piRPCSession) doStart(startCtx context.Context) error {
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

	s.acquireSessionIDFromStateContext(startCtx, "startup")

	// Reaper only owns process state. readLoop owns the single transport-failure
	// event, avoiding duplicate stderr/EOF errors racing into the engine.
	go func() {
		err := cmd.Wait()
		s.alive.Store(false)
		stderrMsg := strings.TrimSpace(stderrBuf.String())
		if err != nil && stderrMsg != "" {
			slog.Error("piRPCSession: process exited", "error", err, "stderr", truncStr(stderrMsg, 300))
		} else if err != nil {
			slog.Warn("piRPCSession: process exited", "error", err)
		}
		// readLoop exclusively owns transport completion after draining stdout.
		// Do not fail pending calls here: ACK/agent_settled frames may still be
		// buffered even though Wait has observed process exit.
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
	scanErr := scanner.Err()
	if scanErr != nil {
		slog.Debug("piRPCSession: stdout scanner closed", "error", scanErr)
	}
	if s.closing.Load() || s.ctx.Err() != nil {
		return
	}
	cause := scanErr
	if cause == nil {
		// stderr is still owned by cmd.Wait's writer here; reading its buffer
		// would race process teardown. Reaper logs details after Wait completes.
		cause = errors.New("pi RPC stdout closed unexpectedly")
	}
	s.emitTransportFailure(cause)
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
	s.updateUsageFromEvent(raw)
	eventType, _ := raw["type"].(string)
	switch eventType {
	case "message_update":
		s.rpcHandleMessageUpdate(raw)
	case "message_end":
		s.rpcHandleMessageEnd(raw)
	case "custom_message":
		if content := customMessageContent(raw); content != "" {
			s.rpcFinalizeCustomMessage(content)
		}
	case "agent_start":
		s.streamMu.Lock()
		s.isStreaming = true
		s.streamMu.Unlock()
		s.rpcBeginAttempt()
	case "agent_end":
		// agent_end is deliberately not a terminal boundary. Pi can retry,
		// compact, or continue queued work after it.
		s.rpcHandleAgentEnd(raw)
	case "agent_settled":
		s.streamMu.Lock()
		s.isStreaming = false
		s.streamMu.Unlock()
		s.rpcHandleAgentSettled()
	case "auto_retry_start":
		s.rpcHandleAutoRetryStart(raw)
	case "auto_retry_end":
		s.rpcHandleAutoRetryEnd(raw)
	case "extension_error":
		s.rpcHandleExtensionError(raw)
	case "turn_end":
		// A single prompt can produce multiple turns (assistant -> tools -> assistant).
		// Only agent_settled completes the pi-connect turn.
	case "compaction_start", "compaction_end", "summarization_retry_scheduled", "summarization_retry_attempt_start", "summarization_retry_finished":
		s.rpcHandleCompactionEvent(eventType, raw)
	case "queue_update":
		// agent_settled remains authoritative; queue updates are liveness only.
		slog.Debug("piRPCSession: queue state changed", "session_id", s.CurrentSessionID())
	}
}

func (s *piRPCSession) rpcHandleExtensionError(raw map[string]any) {
	errMsg, _ := raw["error"].(string)
	extPath, _ := raw["extensionPath"].(string)
	extErr := &rpcExtensionError{extension: filepath.Base(extPath), message: errMsg}

	s.turnMu.Lock()
	if s.turn.active || s.promptPending {
		// EventError ends the engine's foreground wait even when nontransport.
		// Defer errors tied to an ACK/active run to avoid a second settlement.
		s.turn.pendingExtensionErr = extErr.Error()
		s.turnMu.Unlock()
		return
	}
	s.turn.finalizedSeq++
	s.turnMu.Unlock()
	s.emitLifecycle(core.Event{Type: core.EventError, Error: extErr})
}

func (s *piRPCSession) rpcHandleCompactionEvent(eventType string, raw map[string]any) {
	switch eventType {
	case "compaction_start":
		slog.Info("piRPCSession: compaction started", "session_id", s.CurrentSessionID())
		s.tryEmit(core.Event{Type: core.EventThinking, Content: "Context compaction started"})
	case "compaction_end":
		if errMsg, _ := raw["errorMessage"].(string); errMsg != "" {
			slog.Warn("piRPCSession: compaction failed", "session_id", s.CurrentSessionID(), "error", errMsg)
			s.turnMu.Lock()
			active := s.turn.active
			hasValidAnswer := s.turn.committedSuccess || s.turn.attemptSucceeded
			if active && !hasValidAnswer {
				s.turn.finalErr = "context compaction failed: " + errMsg
			}
			s.turnMu.Unlock()
			if active && hasValidAnswer {
				// Threshold compaction runs after a valid answer. Its failure must
				// not replace that answer with a failed turn.
				s.tryEmit(core.Event{Type: core.EventThinking, Content: "Context compaction failed; answer preserved"})
			}
			if !active {
				// Manual compact commands do not create an agent run/agent_settled.
				s.emitLifecycle(core.Event{Type: core.EventError, Error: &rpcProviderError{message: "context compaction failed: " + errMsg}})
			}
			return
		}
		slog.Info("piRPCSession: compaction completed", "session_id", s.CurrentSessionID())
		s.tryEmit(core.Event{Type: core.EventThinking, Content: "Context compaction completed"})
	case "summarization_retry_scheduled":
		s.tryEmit(core.Event{Type: core.EventThinking, Content: "Context compaction retry scheduled"})
	case "summarization_retry_attempt_start":
		s.tryEmit(core.Event{Type: core.EventThinking, Content: "Context compaction retry started"})
	}
}

func (s *piRPCSession) rpcBeginAttempt() {
	s.turnMu.Lock()
	var extensionWarning string
	if !s.turn.active {
		seq := s.turn.finalizedSeq
		extensionWarning = s.turn.pendingExtensionErr
		deferredCustom := append([]string(nil), s.turn.deferredCustom...)
		s.turn = rpcTurnState{active: true, finalizedSeq: seq, deferredCustom: deferredCustom}
	}
	s.turn.attemptOutput = ""
	s.turn.streamed.Reset()
	s.turn.attemptErr = ""
	s.turn.attemptSucceeded = false
	s.turn.attemptUsage = piUsage{}
	s.turnMu.Unlock()
	if extensionWarning != "" {
		s.tryEmit(core.Event{Type: core.EventThinking, Content: extensionWarning})
	}
}

// rpcHandleAgentEnd records one low-level run. It never emits EventResult or
// EventError: willRetry, overflow compaction, or extension-queued continuation
// can all follow before agent_settled.
func (s *piRPCSession) rpcHandleAgentEnd(raw map[string]any) {
	var output strings.Builder
	var finalErr string
	var sawSuccessfulAssistant bool
	var attemptUsage piUsage
	if messages, ok := raw["messages"].([]any); ok {
		for _, m := range messages {
			msg, _ := m.(map[string]any)
			if msg == nil {
				continue
			}
			if role, _ := msg["role"].(string); role != "assistant" {
				continue
			}
			s.recordUsageFromMessage(msg)
			if u, ok := parsePiUsage(msg["usage"]); ok {
				attemptUsage = u
			}
			if errMsg := assistantError(msg); errMsg != "" {
				finalErr = errMsg
				if isRecoverablePiOverflow(errMsg) {
					s.storePendingOverflowError(errMsg)
				}
				continue
			}
			if assistantMessageSucceeded(msg) {
				finalErr = ""
				sawSuccessfulAssistant = true
				s.clearPendingOverflowError()
			}
			output.WriteString(assistantText(msg))
		}
	}

	willRetry, _ := raw["willRetry"].(bool)
	s.turnMu.Lock()
	if !s.turn.active {
		seq := s.turn.finalizedSeq
		s.turn = rpcTurnState{active: true, finalizedSeq: seq}
	}
	if output.Len() > 0 {
		s.turn.attemptOutput = output.String()
	} else if s.turn.attemptOutput == "" {
		s.turn.attemptOutput = s.turn.streamed.String()
	}
	if finalErr != "" {
		s.turn.attemptErr = finalErr
	}
	if sawSuccessfulAssistant {
		s.turn.attemptSucceeded = true
	}
	if attemptUsage.usedTokens() > 0 {
		s.turn.attemptUsage = attemptUsage
	}
	if s.turn.attemptUsage.usedTokens() > 0 {
		s.turn.attemptUsages = append(s.turn.attemptUsages, s.turn.attemptUsage)
	}
	if willRetry {
		// Preserve completed assistant/tool-turn text from this run, but discard
		// the final failed assistant call's uncommitted stream.
		s.turn.committed.WriteString(output.String())
		s.turn.committedSuccess = s.turn.committedSuccess || sawSuccessfulAssistant
		s.turn.attemptOutput = ""
		s.turn.streamed.Reset()
		s.turn.attemptErr = ""
		s.turn.finalErr = ""
	} else if s.turn.attemptErr != "" {
		s.turn.finalErr = s.turn.attemptErr
		s.turn.attemptOutput = ""
	} else {
		s.turn.committed.WriteString(s.turn.attemptOutput)
		s.turn.committedSuccess = s.turn.committedSuccess || s.turn.attemptSucceeded
		s.turn.finalErr = ""
		s.turn.attemptOutput = ""
	}
	s.turnMu.Unlock()
}

func (s *piRPCSession) rpcHandleAutoRetryStart(_ map[string]any) {
	s.turnMu.Lock()
	if !s.turn.active {
		seq := s.turn.finalizedSeq
		s.turn = rpcTurnState{active: true, finalizedSeq: seq}
	}
	// Be defensive with older producers that omit agent_end.willRetry.
	if s.turn.finalErr != "" {
		s.turn.finalErr = ""
	}
	s.turn.attemptOutput = ""
	s.turn.streamed.Reset()
	s.turn.attemptErr = ""
	s.turn.attemptSucceeded = false
	s.turnMu.Unlock()
	s.tryEmit(core.Event{Type: core.EventThinking, Content: "Provider retry in progress"})
}

func (s *piRPCSession) rpcHandleAutoRetryEnd(raw map[string]any) {
	if success, _ := raw["success"].(bool); success {
		return
	}
	if finalErr, _ := raw["finalError"].(string); finalErr != "" {
		s.turnMu.Lock()
		if s.turn.active {
			s.turn.finalErr = finalErr
		}
		s.turnMu.Unlock()
	}
}

func (s *piRPCSession) rpcHandleAgentSettled() {
	s.turnMu.Lock()
	if !s.turn.active {
		s.turnMu.Unlock()
		return
	}
	content := s.turn.committed.String()
	if s.turn.finalErr == "" && s.turn.attemptOutput != "" {
		content += s.turn.attemptOutput
	}
	errMsg := s.turn.finalErr
	if errMsg == "" && content == "" && !s.turn.committedSuccess && !s.turn.attemptSucceeded {
		errMsg = s.pendingOverflowErrorForEOF()
	}
	extensionWarning := s.turn.pendingExtensionErr
	deferredCustom := append([]string(nil), s.turn.deferredCustom...)
	s.turn.pendingExtensionErr = ""
	s.turn.deferredCustom = nil
	s.turn.active = false
	s.turn.finalizedSeq++
	s.turnMu.Unlock()

	if extensionWarning != "" {
		s.tryEmit(core.Event{Type: core.EventThinking, Content: extensionWarning})
	}
	if errMsg != "" {
		if len(deferredCustom) > 0 {
			// Core returns immediately on EventError and resyncs remaining events.
			// Fold side notifications into the one settled error so none are lost.
			errMsg += "\n\n" + strings.Join(deferredCustom, "\n")
			deferredCustom = nil
		}
		s.emitLifecycle(core.Event{Type: core.EventError, Error: &rpcProviderError{message: errMsg}})
	} else {
		evt := core.Event{Type: core.EventResult, Content: content, SessionID: s.CurrentSessionID(), Done: true}
		if usage := s.GetContextUsage(); usage != nil {
			evt.InputTokens = usage.UsedTokens
			evt.OutputTokens = usage.OutputTokens
		}
		s.emitLifecycle(evt)
	}
	for _, custom := range deferredCustom {
		s.emitLifecycle(core.Event{Type: core.EventResult, Content: custom, Done: true, SessionID: s.CurrentSessionID()})
	}
}

func assistantError(msg map[string]any) string {
	if errMsg, _ := msg["errorMessage"].(string); errMsg != "" {
		return errMsg
	}
	if stopReason, _ := msg["stopReason"].(string); stopReason == "error" {
		return "assistant provider error"
	}
	return ""
}

func assistantText(msg map[string]any) string {
	content, _ := msg["content"].([]any)
	var b strings.Builder
	for _, c := range content {
		item, _ := c.(map[string]any)
		if item == nil {
			continue
		}
		if typ, _ := item["type"].(string); typ != "text" {
			continue
		}
		if text, ok := item["text"].(string); ok {
			b.WriteString(text)
		}
	}
	return b.String()
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
			// Buffer until agent_settled. Streaming failed-attempt text outward
			// cannot be retracted safely when Pi retries.
			s.turnMu.Lock()
			if !s.turn.active {
				seq := s.turn.finalizedSeq
				s.turn = rpcTurnState{active: true, finalizedSeq: seq}
			}
			s.turn.streamed.WriteString(delta)
			s.turnMu.Unlock()
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
		s.recordUsageFromMessage(msg)
		s.turnMu.Lock()
		if !s.turn.active {
			seq := s.turn.finalizedSeq
			s.turn = rpcTurnState{active: true, finalizedSeq: seq}
		}
		if u, ok := parsePiUsage(msg["usage"]); ok {
			s.turn.attemptUsage = u
		}
		if errMsg := assistantError(msg); errMsg != "" {
			s.turn.attemptErr = errMsg
			s.turn.attemptOutput = ""
			s.turnMu.Unlock()
			if isRecoverablePiOverflow(errMsg) {
				s.storePendingOverflowError(errMsg)
			}
			return
		}
		text := assistantText(msg)
		if text != "" {
			s.turn.attemptOutput += text
		}
		s.turnMu.Unlock()
		if assistantMessageSucceeded(msg) {
			s.turnMu.Lock()
			s.turn.attemptSucceeded = true
			s.turnMu.Unlock()
			s.clearPendingOverflowError()
		}
	}
}

func (s *piRPCSession) updateUsageFromEvent(raw map[string]any) {
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

func (s *piRPCSession) recordUsageFromMessage(msg map[string]any) {
	u, ok := parsePiUsage(msg["usage"])
	if !ok {
		return
	}
	s.usageMu.Lock()
	s.contextUsage = u.contextUsage()
	s.usageMu.Unlock()
}

func (s *piRPCSession) GetContextUsage() *core.ContextUsage {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	return cloneContextUsage(s.contextUsage)
}

func (s *piRPCSession) storePendingOverflowError(errMsg string) {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	s.pendingContextOverflowErr = errMsg
	s.recoveredAfterOverflow = false
}

func (s *piRPCSession) clearPendingOverflowError() {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	if s.pendingContextOverflowErr != "" {
		s.pendingContextOverflowErr = ""
		s.recoveredAfterOverflow = true
	}
}

func (s *piRPCSession) pendingOverflowErrorForEOF() string {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	if s.recoveredAfterOverflow {
		return ""
	}
	return s.pendingContextOverflowErr
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
	if resp.Success && resp.Command == "get_state" {
		s.storeSessionIDFromStateData(resp.Data)
	}
	key := idKey(resp.ID)
	if key == "" {
		return
	}
	s.pendingMu.Lock()
	pending, ok := s.pending[key]
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
	// Buffered channel plus map ownership gives exactly-once completion. The
	// receiver may be racing caller cancellation, but only map winner decides.
	pending.ch <- rpcCallResult{response: rpcResponse{success: resp.Success, data: resp.Data, errMsg: errMsg}}
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

// Send implements core.AgentSession.Send. It waits for prompt acceptance while
// turn events stream asynchronously via Events().
func (s *piRPCSession) Send(prompt string, images []core.ImageAttachment, files []core.FileAttachment) error {
	return s.SendContext(s.ctx, prompt, images, files)
}

// SendContext waits for Pi's authoritative prompt-acceptance ACK. Once stdin
// accepts the complete command, cancellation before that ACK is outcome-unknown
// and must never trigger an automatic replay.
func (s *piRPCSession) SendContext(ctx context.Context, prompt string, images []core.ImageAttachment, files []core.FileAttachment) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if !s.alive.Load() {
		return &RPCCommandNotSentError{Command: "prompt", Cause: errors.New("session is closed")}
	}
	if err := s.startProcessContext(ctx); err != nil {
		return &RPCCommandNotSentError{Command: "prompt", Cause: err}
	}
	if err := s.acquireMutating(ctx, "prompt"); err != nil {
		return err
	}
	defer s.releaseMutating()

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
	var extensionSeq uint64
	s.turnMu.Lock()
	s.promptPending = true
	if isExt {
		extensionSeq = s.turn.finalizedSeq
	}
	s.turnMu.Unlock()
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

	resp, err := s.callMutating(ctx, cmd)
	s.turnMu.Lock()
	s.promptPending = false
	if err != nil || !resp.success {
		s.turn.pendingExtensionErr = ""
	}
	s.turnMu.Unlock()
	if err != nil {
		return fmt.Errorf("piRPCSession: prompt command: %w", err)
	}
	if !resp.success {
		return fmt.Errorf("piRPCSession: pi rejected prompt: %s", resp.errMsg)
	}
	if s.CurrentSessionID() == "" {
		s.acquireSessionIDFromState("post_prompt")
	}
	if isExt {
		// Extension commands can be fully handled during prompt preflight and
		// ACK without starting an agent run; such commands never emit
		// agent_settled. Complete only when no run/custom result appeared.
		s.rpcFinalizeExtensionACK(extensionSeq)
	}
	return nil
}

func (s *piRPCSession) CompactContext() error {
	return s.CompactContextWithContext(s.ctx)
}

// CompactContextWithContext gives manual compaction the same cancellation and
// outcome semantics as prompts. Process death between write and ACK remains
// unknown; durable restart journaling/exact-once recovery is intentionally out
// of scope, so callers must not replay automatically.
func (s *piRPCSession) CompactContextWithContext(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if !s.alive.Load() {
		return &RPCCommandNotSentError{Command: "compact", Cause: errors.New("session is closed")}
	}
	if err := s.startProcessContext(ctx); err != nil {
		return &RPCCommandNotSentError{Command: "compact", Cause: err}
	}
	if err := s.acquireMutating(ctx, "compact"); err != nil {
		return err
	}
	defer s.releaseMutating()

	resp, err := s.callMutating(ctx, map[string]any{"type": "compact"})
	if err != nil {
		return fmt.Errorf("piRPCSession: compact command: %w", err)
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

func (s *piRPCSession) rpcFinalizeCustomMessage(content string) {
	s.turnMu.Lock()
	if s.turn.active || s.promptPending {
		// Custom/subagent notifications are independent side-channel results.
		// Defer them behind the authoritative active-turn settlement. promptPending
		// closes the pre-agent_start race after Send has accepted a prompt.
		s.turn.deferredCustom = append(s.turn.deferredCustom, content)
		s.turnMu.Unlock()
		return
	}
	s.turn.finalizedSeq++
	s.turnMu.Unlock()
	s.emitLifecycle(core.Event{Type: core.EventResult, Content: content, Done: true, SessionID: s.CurrentSessionID()})
}

func (s *piRPCSession) rpcFinalizeExtensionACK(before uint64) {
	s.turnMu.Lock()
	idle := !s.turn.active && s.turn.finalizedSeq == before
	s.turnMu.Unlock()
	if !idle {
		return
	}

	// Prompt ACK is emitted immediately before a normal agent run starts, so
	// local event state alone has a race. A subsequent protocol state query is
	// ordered after prompt handling and distinguishes true extension-only ACKs
	// from slash templates/commands that started an agent. This is capability
	// state, not a settlement timer.
	resp, err := s.callWithTimeout(map[string]any{"type": "get_state"}, 2*time.Second)
	if err != nil || !resp.success {
		if err == nil {
			err = fmt.Errorf("get_state rejected: %s", resp.errMsg)
		}
		s.emitTransportFailure(fmt.Errorf("cannot confirm extension-only prompt completion: %w", err))
		return
	}
	var state struct {
		IsStreaming bool `json:"isStreaming"`
	}
	if err := json.Unmarshal(resp.data, &state); err != nil {
		s.emitTransportFailure(fmt.Errorf("cannot decode extension completion state: %w", err))
		return
	}
	if state.IsStreaming {
		return
	}

	s.turnMu.Lock()
	if s.turn.active || s.turn.finalizedSeq != before {
		s.turnMu.Unlock()
		return
	}
	extensionErr := s.turn.pendingExtensionErr
	deferredCustom := append([]string(nil), s.turn.deferredCustom...)
	s.turn.pendingExtensionErr = ""
	s.turn.deferredCustom = nil
	s.turn.finalizedSeq++
	s.turnMu.Unlock()
	if extensionErr != "" {
		if len(deferredCustom) > 0 {
			extensionErr += "\n\n" + strings.Join(deferredCustom, "\n")
		}
		s.emitLifecycle(core.Event{Type: core.EventError, Error: errors.New(extensionErr)})
		return
	}
	if len(deferredCustom) == 0 {
		s.emitLifecycle(core.Event{Type: core.EventResult, Done: true, SessionID: s.CurrentSessionID()})
		return
	}
	for _, custom := range deferredCustom {
		s.emitLifecycle(core.Event{Type: core.EventResult, Content: custom, Done: true, SessionID: s.CurrentSessionID()})
	}
}

func (s *piRPCSession) acquireSessionIDFromState(reason string) {
	s.acquireSessionIDFromStateContext(s.ctx, reason)
}

func (s *piRPCSession) acquireSessionIDFromStateContext(ctx context.Context, reason string) {
	if s.CurrentSessionID() != "" {
		return
	}
	resp, err := s.callWithTimeoutContext(ctx, map[string]any{"type": "get_state"}, 2*time.Second)
	if err != nil {
		slog.Debug("piRPCSession: get_state session id lookup failed", "reason", reason, "error", err)
		return
	}
	if !resp.success {
		slog.Debug("piRPCSession: get_state session id lookup rejected", "reason", reason, "error", resp.errMsg)
		return
	}
	s.storeSessionIDFromStateData(resp.data)
}

func (s *piRPCSession) storeSessionIDFromStateData(data json.RawMessage) {
	if s.CurrentSessionID() != "" || len(bytes.TrimSpace(data)) == 0 {
		return
	}
	var state struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		slog.Debug("piRPCSession: bad get_state data", "error", err)
		return
	}
	if state.SessionID != "" {
		s.sessionID.Store(state.SessionID)
	}
}

// callWithTimeout is reserved for bounded read-only/idempotent commands. A
// timeout removes that request; monotonic IDs ensure any late response cannot
// satisfy or corrupt a future call.
func (s *piRPCSession) callWithTimeout(payload map[string]any, timeout time.Duration) (rpcResponse, error) {
	return s.callWithTimeoutContext(s.ctx, payload, timeout)
}

func (s *piRPCSession) callWithTimeoutContext(ctx context.Context, payload map[string]any, timeout time.Duration) (rpcResponse, error) {
	return s.callRPC(ctx, payload, timeout, false)
}

// callMutating has no acceptance-failure timeout after a successful write.
// Its only diagnostic timer emits one slow-acceptance warning and keeps waiting.
func (s *piRPCSession) callMutating(ctx context.Context, payload map[string]any) (rpcResponse, error) {
	return s.callRPC(ctx, payload, 0, true)
}

func (s *piRPCSession) callRPC(ctx context.Context, payload map[string]any, timeout time.Duration, mutating bool) (rpcResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	command, _ := payload["type"].(string)
	if command == "" {
		command = "unknown"
	}
	if err := s.commandUnavailable(ctx); err != nil {
		return rpcResponse{}, &RPCCommandNotSentError{Command: command, Cause: err}
	}

	id := fmt.Sprintf("%d", s.nextID.Add(1))
	payload["id"] = id
	pending := &rpcPending{ch: make(chan rpcCallResult, 1), mutating: mutating}
	s.pendingMu.Lock()
	if !s.alive.Load() {
		s.pendingMu.Unlock()
		return rpcResponse{}, &RPCCommandNotSentError{Command: command, Cause: errors.New("pi RPC transport is closed")}
	}
	s.pending[id] = pending
	s.pendingMu.Unlock()

	written, cancellationClaimed, err := s.writeJSONTracked(ctx, payload, func() bool {
		return s.claimPendingCancellation(id, mutating)
	})
	if err != nil {
		var notStarted *rpcWriteNotStartedError
		owned := cancellationClaimed
		if !owned {
			if errors.As(err, &notStarted) {
				owned = s.removePending(id)
			} else {
				owned = s.claimPendingCancellation(id, mutating)
			}
		}
		if !owned {
			// Matched ACK or transport failure already owns the request. A
			// definitive ACK wins even if the local writer reports an error later.
			claimed := <-pending.ch
			if claimed.err == nil {
				s.emitTransportFailure(err)
				return claimed.response, nil
			}
			err = claimed.err
		} else {
			if !errors.As(err, &notStarted) {
				s.alive.Store(false)
				s.procMu.Lock()
				stdin := s.stdin
				s.procMu.Unlock()
				if stdin != nil {
					_ = stdin.Close()
				}
				s.cancelPending(&rpcTransportError{cause: err})
			}
		}
		if written == 0 {
			return rpcResponse{}, &RPCCommandNotSentError{Command: command, Cause: err}
		}
		if mutating {
			return rpcResponse{}, &RPCAcceptanceUnknownError{Command: command, Cause: err}
		}
		return rpcResponse{}, fmt.Errorf("pi RPC %s partial write: %w", command, err)
	}

	ch := pending.ch

	var timeoutTimer *time.Timer
	var timeoutCh <-chan time.Time
	if timeout > 0 {
		timeoutTimer = time.NewTimer(timeout)
		defer timeoutTimer.Stop()
		timeoutCh = timeoutTimer.C
	}
	var slowTimer *time.Timer
	var slowCh <-chan time.Time
	if mutating && s.slowAcceptanceAfter > 0 {
		slowTimer = time.NewTimer(s.slowAcceptanceAfter)
		defer slowTimer.Stop()
		slowCh = slowTimer.C
	}

	for {
		select {
		case result := <-ch:
			if result.err != nil {
				if mutating {
					return rpcResponse{}, &RPCAcceptanceUnknownError{Command: command, Cause: result.err}
				}
				return rpcResponse{}, result.err
			}
			return result.response, nil
		case <-ctx.Done():
			var claimed bool
			if mutating {
				claimed = s.claimPendingCancellation(id, true)
			} else {
				claimed = s.removePending(id)
			}
			if claimed {
				if mutating {
					s.terminateUnknownTransport(ctx.Err())
					return rpcResponse{}, &RPCAcceptanceUnknownError{Command: command, Cause: ctx.Err()}
				}
				return rpcResponse{}, ctx.Err()
			}
			// Response/process exit already won map ownership. Consume its buffered
			// result so cancellation cannot overwrite a definitive outcome.
			return s.finishClaimedRPC(command, mutating, <-ch)
		case <-s.sessionDone():
			var claimed bool
			if mutating {
				claimed = s.claimPendingCancellation(id, true)
			} else {
				claimed = s.removePending(id)
			}
			if claimed {
				if mutating {
					s.terminateUnknownTransport(s.ctx.Err())
					return rpcResponse{}, &RPCAcceptanceUnknownError{Command: command, Cause: s.ctx.Err()}
				}
				return rpcResponse{}, s.ctx.Err()
			}
			return s.finishClaimedRPC(command, mutating, <-ch)
		case <-timeoutCh:
			if s.removePending(id) {
				return rpcResponse{}, &rpcCallTimeoutError{command: command, id: id, timeout: timeout}
			}
			return s.finishClaimedRPC(command, mutating, <-ch)
		case <-slowCh:
			slowCh = nil
			if s.slowAcceptanceLog != nil {
				s.slowAcceptanceLog(command, s.slowAcceptanceAfter)
			} else {
				slog.Warn("piRPCSession: slow command acceptance", "command", command, "elapsed", s.slowAcceptanceAfter, "session_id", s.CurrentSessionID())
			}
		}
	}
}

func (s *piRPCSession) finishClaimedRPC(command string, mutating bool, result rpcCallResult) (rpcResponse, error) {
	if result.err == nil {
		return result.response, nil
	}
	if mutating {
		return rpcResponse{}, &RPCAcceptanceUnknownError{Command: command, Cause: result.err}
	}
	return rpcResponse{}, result.err
}

func (s *piRPCSession) removePending(id string) bool {
	return s.claimPendingCancellation(id, false)
}

// claimPendingCancellation makes mutation cancellation ownership and lifecycle
// suppression one atomic pending-map operation. Transport failure therefore
// cannot slip a competing EventError between request removal and suppression.
func (s *piRPCSession) claimPendingCancellation(id string, suppress bool) bool {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	if _, ok := s.pending[id]; !ok {
		return false
	}
	if suppress {
		s.suppressTransportEvent.Store(true)
	}
	delete(s.pending, id)
	return true
}

func (s *piRPCSession) acquireMutating(ctx context.Context, command string) error {
	if s.mutatingGate == nil {
		// Only zero-value test sessions can reach this path; real sessions always
		// initialize the gate in newPiRPCSession.
		s.mutatingGate = make(chan struct{}, 1)
		s.mutatingGate <- struct{}{}
	}
	select {
	case <-s.mutatingGate:
		if err := s.commandUnavailable(ctx); err != nil {
			s.releaseMutating()
			return &RPCCommandNotSentError{Command: command, Cause: err}
		}
		return nil
	case <-ctx.Done():
		return &RPCCommandNotSentError{Command: command, Cause: ctx.Err()}
	case <-s.sessionDone():
		return &RPCCommandNotSentError{Command: command, Cause: s.ctx.Err()}
	}
}

func (s *piRPCSession) commandUnavailable(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	select {
	case <-s.sessionDone():
		return s.ctx.Err()
	default:
	}
	if !s.alive.Load() {
		return errors.New("pi RPC transport is closed")
	}
	return nil
}

func (s *piRPCSession) sessionDone() <-chan struct{} {
	if s.ctx == nil {
		return nil
	}
	return s.ctx.Done()
}

func (s *piRPCSession) releaseMutating() {
	s.mutatingGate <- struct{}{}
}

func (s *piRPCSession) writeJSON(v any) error {
	_, _, err := s.writeJSONTracked(s.ctx, v, nil)
	return err
}

// writeJSONTracked marshals before touching stdin, permits cancellation while
// queued for the write lock, and closes stdin to interrupt an in-progress pipe
// write on cancellation. Closing stdin makes that transport terminal rather
// than leaving an immortal writer goroutine.
func (s *piRPCSession) writeJSONTracked(ctx context.Context, v any, claimCancellation func() bool) (written int, cancellationClaimed bool, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	data, err := json.Marshal(v)
	if err != nil {
		return 0, false, &rpcWriteNotStartedError{cause: err}
	}
	data = append(data, '\n')

	if s.writeGate == nil {
		s.writeGate = make(chan struct{}, 1)
		s.writeGate <- struct{}{}
	}
	select {
	case <-s.writeGate:
		defer func() { s.writeGate <- struct{}{} }()
	case <-ctx.Done():
		return 0, false, &rpcWriteNotStartedError{cause: ctx.Err()}
	case <-s.sessionDone():
		return 0, false, &rpcWriteNotStartedError{cause: s.ctx.Err()}
	}
	if err := s.commandUnavailable(ctx); err != nil {
		return 0, false, &rpcWriteNotStartedError{cause: err}
	}

	s.procMu.Lock()
	stdin := s.stdin
	s.procMu.Unlock()
	if stdin == nil {
		return 0, false, &rpcWriteNotStartedError{cause: errors.New("piRPCSession: stdin not ready")}
	}

	var stateMu sync.Mutex
	finished := false
	cancelled := false
	writeDone := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-ctx.Done():
		case <-s.sessionDone():
		case <-writeDone:
			return
		}

		stateMu.Lock()
		if finished || (claimCancellation != nil && !claimCancellation()) {
			stateMu.Unlock()
			return
		}
		cancelled = true
		s.alive.Store(false)
		stateMu.Unlock()
		_ = stdin.Close()
	}()

	total := 0
	var writeErr error
	for total < len(data) {
		n, currentErr := stdin.Write(data[total:])
		total += n
		if currentErr != nil {
			writeErr = currentErr
			break
		}
		if n == 0 {
			writeErr = io.ErrShortWrite
			break
		}
	}
	stateMu.Lock()
	finished = true
	cancellationClaimed = cancelled
	stateMu.Unlock()
	close(writeDone)
	<-watcherDone

	if cancellationClaimed {
		if writeErr != nil {
			return total, true, writeErr
		}
		if ctx.Err() != nil {
			return total, true, ctx.Err()
		}
		if s.ctx != nil && s.ctx.Err() != nil {
			return total, true, s.ctx.Err()
		}
		return total, true, errors.New("pi RPC write cancelled")
	}
	if writeErr != nil {
		return total, false, writeErr
	}
	return total, false, nil
}

// cancelPending claims every outstanding request. Its return value identifies
// whether a mutating caller will report transport loss through its Send/compact
// return path, avoiding a contradictory competing lifecycle event.
func (s *piRPCSession) cancelPending(err error) (hadMutating bool) {
	if err == nil {
		err = errors.New("pi RPC transport closed")
	}
	s.pendingMu.Lock()
	pending := s.pending
	s.pending = make(map[string]*rpcPending)
	s.pendingMu.Unlock()
	for _, request := range pending {
		hadMutating = hadMutating || request.mutating
		request.ch <- rpcCallResult{err: err}
	}
	return hadMutating
}

func (s *piRPCSession) terminateUnknownTransport(cause error) {
	if cause == nil {
		cause = errors.New("mutating command acceptance cancelled")
	}
	s.suppressTransportEvent.Store(true)
	s.alive.Store(false)
	s.procMu.Lock()
	stdin := s.stdin
	s.procMu.Unlock()
	if stdin != nil {
		_ = stdin.Close()
	}
	s.cancelPending(&rpcTransportError{cause: cause})
}

func (s *piRPCSession) tryEmit(evt core.Event) {
	if s.events == nil {
		return
	}
	s.emitMu.Lock()
	defer s.emitMu.Unlock()
	if cap(s.events) > 0 && len(s.events) >= cap(s.events)-1 {
		// Keep one slot reserved for EventResult/EventError. Only progress may
		// be coalesced/dropped under backpressure.
		return
	}
	s.eventsMu.RLock()
	defer s.eventsMu.RUnlock()
	if s.eventsClosed {
		return
	}
	select {
	case s.events <- evt:
	default:
	}
}

func (s *piRPCSession) emitLifecycle(evt core.Event) {
	if s.events == nil {
		return
	}
	s.emitMu.Lock()
	defer s.emitMu.Unlock()
	s.eventsMu.RLock()
	defer s.eventsMu.RUnlock()
	if s.eventsClosed {
		return
	}
	if s.ctx == nil {
		s.events <- evt
		return
	}
	select {
	case s.events <- evt:
	case <-s.ctx.Done():
	}
}

func (s *piRPCSession) emitTransportFailure(cause error) {
	if cause == nil {
		cause = errors.New("pi RPC transport failed")
	}
	s.transportOnce.Do(func() {
		transportErr := &rpcTransportError{cause: cause}
		s.alive.Store(false)
		// Claim waiters before closing stdin. This makes pending-map ownership
		// the outcome linearization point and lets a blocked writer unwind after
		// its caller has been selected as the sole error reporter.
		hadMutating := s.cancelPending(transportErr)
		s.procMu.Lock()
		stdin := s.stdin
		s.procMu.Unlock()
		if stdin != nil {
			_ = stdin.Close()
		}
		if hadMutating || s.suppressTransportEvent.Load() {
			// The waiting Send/compact caller reports typed outcome-unknown (or
			// typed not-sent for a zero-byte write). Do not race it with a
			// contradictory lifecycle transport event.
			return
		}
		s.emitLifecycle(core.Event{Type: core.EventError, Error: transportErr})
	})
}

func (s *piRPCSession) closeEvents() {
	if s.events == nil {
		return
	}

	s.eventsMu.Lock()
	defer s.eventsMu.Unlock()
	if !s.eventsClosed {
		s.eventsClosed = true
		close(s.events)
	}
}

func (s *piRPCSession) RespondPermission(_ string, _ core.PermissionResult) error {
	return nil
}

func (s *piRPCSession) EventErrorIsTerminal(err error) bool {
	var transportErr *rpcTransportError
	return errors.As(err, &transportErr)
}

func (s *piRPCSession) Events() <-chan core.Event { return s.events }

func (s *piRPCSession) CurrentSessionID() string {
	v, _ := s.sessionID.Load().(string)
	return v
}

func (s *piRPCSession) Alive() bool { return s.alive.Load() }

func (s *piRPCSession) Close() error {
	s.closeOnce.Do(func() {
		s.closing.Store(true)
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
		// Wake every ACK waiter before waiting for process teardown. Calls that
		// completed their write report acceptance/outcome unknown.
		s.cancelPending(&rpcTransportError{cause: errors.New("pi RPC session closed")})
		if s.cancel != nil {
			s.cancel()
		}
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
		s.closeEvents()
	})
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
