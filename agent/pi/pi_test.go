package pi

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// ── normalizeTransport ───────────────────────────────────────

func TestNormalizeTransport(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", "rpc"},
		{"rpc", "rpc"},
		{"RPC", "rpc"},
		{"  rpc  ", "rpc"},
		{"json", "json"},
		{"JSON", "json"},
		{"  json  ", "json"},
		{"unknown", "rpc"},
	}
	for _, tt := range tests {
		if got := normalizeTransport(tt.in); got != tt.want {
			t.Errorf("normalizeTransport(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// ── normalizeMode ────────────────────────────────────────────

func TestNormalizeMode(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", "default"},
		{"default", "default"},
		{"yolo", "yolo"},
		{"YOLO", "yolo"},
		{"bypass", "yolo"},
		{"auto-approve", "yolo"},
		{"  Yolo  ", "yolo"},
		{"  Bypass ", "yolo"},
		{"unknown", "default"},
		{"something", "default"},
	}
	for _, tt := range tests {
		if got := normalizeMode(tt.in); got != tt.want {
			t.Errorf("normalizeMode(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// ── Agent constructor ────────────────────────────────────────

func TestNew_DefaultValues(t *testing.T) {
	// Use a command that exists on all systems.
	ag, err := New(map[string]any{"cmd": "echo"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	a := ag.(*Agent)
	if a.workDir != "." {
		t.Errorf("workDir = %q, want \".\"", a.workDir)
	}
	if a.model != "" {
		t.Errorf("model = %q, want empty", a.model)
	}
	if a.mode != "default" {
		t.Errorf("mode = %q, want \"default\"", a.mode)
	}
	if a.cmd != "echo" {
		t.Errorf("cmd = %q, want \"echo\"", a.cmd)
	}
	if a.transport != "rpc" {
		t.Errorf("transport = %q, want \"rpc\"", a.transport)
	}
}

func TestNew_CustomOptions(t *testing.T) {
	ag, err := New(map[string]any{
		"cmd":       "echo",
		"work_dir":  "/tmp",
		"model":     "qwen3.5-plus",
		"mode":      "yolo",
		"transport": "json",
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	a := ag.(*Agent)
	if a.workDir != "/tmp" {
		t.Errorf("workDir = %q", a.workDir)
	}
	if a.model != "qwen3.5-plus" {
		t.Errorf("model = %q", a.model)
	}
	if a.mode != "yolo" {
		t.Errorf("mode = %q", a.mode)
	}
	if a.transport != "json" {
		t.Errorf("transport = %q", a.transport)
	}
}

func TestNew_CmdNotFound(t *testing.T) {
	_, err := New(map[string]any{"cmd": "nonexistent-binary-xyz-12345"})
	if err == nil {
		t.Fatal("expected error for missing binary")
	}
	if !strings.Contains(err.Error(), "not found in PATH") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestNew_DefaultCmd(t *testing.T) {
	// When cmd is not specified, it defaults to "pi".
	// This will fail if pi is not installed, which is expected in CI.
	_, err := New(map[string]any{"cmd": ""})
	if err == nil {
		// pi is installed — verify the cmd was set
		return
	}
	if !strings.Contains(err.Error(), "'pi' not found") {
		t.Errorf("unexpected error: %v", err)
	}
}

// ── Agent interface methods ──────────────────────────────────

func TestAgent_NameAndDisplay(t *testing.T) {
	a := &Agent{}
	if a.Name() != "pi" {
		t.Errorf("Name() = %q", a.Name())
	}
	if a.CLIBinaryName() != "pi" {
		t.Errorf("CLIBinaryName() = %q", a.CLIBinaryName())
	}
	if a.CLIDisplayName() != "Pi" {
		t.Errorf("CLIDisplayName() = %q", a.CLIDisplayName())
	}
}

func TestAgent_ModelGetSet(t *testing.T) {
	a := &Agent{}
	if a.GetModel() != "" {
		t.Errorf("initial model = %q", a.GetModel())
	}
	a.SetModel("gpt-4o")
	if a.GetModel() != "gpt-4o" {
		t.Errorf("after SetModel = %q", a.GetModel())
	}
}

func TestAgent_ModeGetSet(t *testing.T) {
	a := &Agent{mode: "default"}
	if a.GetMode() != "default" {
		t.Errorf("initial mode = %q", a.GetMode())
	}
	a.SetMode("yolo")
	if a.GetMode() != "yolo" {
		t.Errorf("after SetMode(yolo) = %q", a.GetMode())
	}
	a.SetMode("bypass")
	if a.GetMode() != "yolo" {
		t.Errorf("after SetMode(bypass) = %q", a.GetMode())
	}
	a.SetMode("unknown")
	if a.GetMode() != "default" {
		t.Errorf("after SetMode(unknown) = %q", a.GetMode())
	}
}

func TestAgent_AvailableModels(t *testing.T) {
	a := &Agent{}
	if models := a.AvailableModels(context.Background()); models != nil {
		t.Errorf("AvailableModels() = %v, want nil", models)
	}
}

func TestAgent_SetSessionEnv(t *testing.T) {
	a := &Agent{}
	a.SetSessionEnv([]string{"FOO=bar", "BAZ=qux"})
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.sessionEnv) != 2 {
		t.Errorf("sessionEnv len = %d, want 2", len(a.sessionEnv))
	}
}

func TestAgent_ListSessions(t *testing.T) {
	a := &Agent{}
	sessions, err := a.ListSessions(context.Background())
	if err != nil {
		t.Errorf("ListSessions() error = %v", err)
	}
	if sessions != nil {
		t.Errorf("ListSessions() = %v, want nil", sessions)
	}
}

func TestAgent_Stop(t *testing.T) {
	a := &Agent{}
	if err := a.Stop(); err != nil {
		t.Errorf("Stop() error = %v", err)
	}
}

func TestAgent_PermissionModes(t *testing.T) {
	a := &Agent{}
	modes := a.PermissionModes()
	if len(modes) != 2 {
		t.Fatalf("PermissionModes() len = %d, want 2", len(modes))
	}
	if modes[0].Key != "default" {
		t.Errorf("modes[0].Key = %q", modes[0].Key)
	}
	if modes[1].Key != "yolo" {
		t.Errorf("modes[1].Key = %q", modes[1].Key)
	}
}

func TestAgent_MemoryFiles(t *testing.T) {
	a := &Agent{workDir: "/tmp/test-project"}
	proj := a.ProjectMemoryFile()
	if !strings.HasSuffix(proj, "AGENTS.md") {
		t.Errorf("ProjectMemoryFile() = %q, want suffix AGENTS.md", proj)
	}
	if !strings.Contains(proj, "test-project") {
		t.Errorf("ProjectMemoryFile() = %q, want to contain work_dir", proj)
	}

	global := a.GlobalMemoryFile()
	if !strings.HasSuffix(global, filepath.Join(".pi", "AGENTS.md")) {
		t.Errorf("GlobalMemoryFile() = %q", global)
	}
}

func TestAgent_StartSession(t *testing.T) {
	a := &Agent{cmd: "echo", workDir: "/tmp", model: "test-model", mode: "yolo", transport: "json"}
	a.SetSessionEnv([]string{"TEST_VAR=1"})

	sess, err := a.StartSession(context.Background(), "resume-123")
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	defer sess.Close()

	ps := sess.(*piSession)
	if ps.cmd != "echo" {
		t.Errorf("cmd = %q", ps.cmd)
	}
	if ps.model != "test-model" {
		t.Errorf("model = %q", ps.model)
	}
	if ps.mode != "yolo" {
		t.Errorf("mode = %q", ps.mode)
	}
	if ps.CurrentSessionID() != "resume-123" {
		t.Errorf("sessionID = %q, want resume-123", ps.CurrentSessionID())
	}
	if !ps.Alive() {
		t.Error("session should be alive")
	}
}

// ── extractToolInput ─────────────────────────────────────────

func TestExtractToolInput(t *testing.T) {
	tests := []struct {
		name string
		item map[string]any
		want string
	}{
		{"nil arguments", map[string]any{}, ""},
		{"description", map[string]any{"arguments": map[string]any{"description": "List files"}}, "List files"},
		{"command", map[string]any{"arguments": map[string]any{"command": "ls -la"}}, "ls -la"},
		{"file_path", map[string]any{"arguments": map[string]any{"file_path": "/tmp/foo.go"}}, "/tmp/foo.go"},
		{"pattern", map[string]any{"arguments": map[string]any{"pattern": "*.go"}}, "*.go"},
		{"query", map[string]any{"arguments": map[string]any{"query": "find errors"}}, "find errors"},
		{"description takes priority", map[string]any{"arguments": map[string]any{
			"description": "desc", "command": "cmd",
		}}, "desc"},
		{"fallback to json", map[string]any{"arguments": map[string]any{"foo": "bar"}}, `{"foo":"bar"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractToolInput(tt.item)
			if got != tt.want {
				t.Errorf("extractToolInput() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractToolInput_LongFallbackTruncated(t *testing.T) {
	longVal := strings.Repeat("x", 300)
	item := map[string]any{"arguments": map[string]any{"data": longVal}}
	got := extractToolInput(item)
	if len(got) > 210 { // 200 + "..."
		t.Errorf("expected truncated output, got len=%d", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Error("expected '...' suffix")
	}
}

// ── truncStr ─────────────────────────────────────────────────

func TestTruncStr(t *testing.T) {
	tests := []struct {
		in       string
		max      int
		want     string
		wantSame bool
	}{
		{"hello", 10, "hello", true},
		{"hello", 5, "hello", true},
		{"hello", 3, "hel...", false},
		{"", 5, "", true},
		{"日本語テスト", 3, "日本語...", false},
		{"日本語テスト", 10, "日本語テスト", true},
	}
	for _, tt := range tests {
		got := truncStr(tt.in, tt.max)
		if got != tt.want {
			t.Errorf("truncStr(%q, %d) = %q, want %q", tt.in, tt.max, got, tt.want)
		}
	}
}

// ── saveImagesToDisk ─────────────────────────────────────────

func TestSaveImagesToDisk(t *testing.T) {
	tmpDir := t.TempDir()
	images := []core.ImageAttachment{
		{MimeType: "image/png", Data: []byte("png-data"), FileName: "test.png"},
		{MimeType: "image/jpeg", Data: []byte("jpg-data")},
		{MimeType: "image/gif", Data: []byte("gif-data")},
		{MimeType: "image/webp", Data: []byte("webp-data")},
		{MimeType: "image/bmp", Data: []byte("bmp-data")}, // unknown mime → .png default
	}

	paths := saveImagesToDisk(tmpDir, images)
	if len(paths) != 5 {
		t.Fatalf("got %d paths, want 5", len(paths))
	}

	// First file should use the provided filename.
	if filepath.Base(paths[0]) != "test.png" {
		t.Errorf("paths[0] base = %q, want test.png", filepath.Base(paths[0]))
	}

	// Check extensions of auto-named files.
	if !strings.HasSuffix(paths[1], ".jpg") {
		t.Errorf("jpeg path = %q, want .jpg suffix", paths[1])
	}
	if !strings.HasSuffix(paths[2], ".gif") {
		t.Errorf("gif path = %q, want .gif suffix", paths[2])
	}
	if !strings.HasSuffix(paths[3], ".webp") {
		t.Errorf("webp path = %q, want .webp suffix", paths[3])
	}
	if !strings.HasSuffix(paths[4], ".png") {
		t.Errorf("unknown mime path = %q, want .png suffix", paths[4])
	}

	// Verify file contents.
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "png-data" {
		t.Errorf("file content = %q", data)
	}
}

func TestSaveImagesToDisk_Empty(t *testing.T) {
	paths := saveImagesToDisk(t.TempDir(), nil)
	if len(paths) != 0 {
		t.Errorf("expected empty, got %d", len(paths))
	}
}

// ── cleanAttachments ─────────────────────────────────────────

func TestCleanAttachments(t *testing.T) {
	tmpDir := t.TempDir()
	attachDir := filepath.Join(tmpDir, ".cc-connect", "attachments")
	os.MkdirAll(attachDir, 0o755)

	// Create some files.
	os.WriteFile(filepath.Join(attachDir, "old1.png"), []byte("data"), 0o644)
	os.WriteFile(filepath.Join(attachDir, "old2.jpg"), []byte("data"), 0o644)

	// Verify files exist.
	entries, _ := os.ReadDir(attachDir)
	if len(entries) != 2 {
		t.Fatalf("expected 2 files, got %d", len(entries))
	}

	cleanAttachments(tmpDir)

	// Files should be removed.
	entries, _ = os.ReadDir(attachDir)
	if len(entries) != 0 {
		t.Errorf("expected 0 files after clean, got %d", len(entries))
	}
}

func TestCleanAttachments_NonexistentDir(t *testing.T) {
	// Should not panic or error on non-existent directory.
	cleanAttachments("/nonexistent/path/xyz")
}

// ── handleEvent ──────────────────────────────────────────────

func newTestSession() *piSession {
	ctx, cancel := context.WithCancel(context.Background())
	s := &piSession{
		events: make(chan core.Event, 64),
		ctx:    ctx,
		cancel: cancel,
	}
	s.alive.Store(true)
	return s
}

func drainEvents(s *piSession) []core.Event {
	var evts []core.Event
	for {
		select {
		case e := <-s.events:
			evts = append(evts, e)
		default:
			return evts
		}
	}
}

func newTestRPCSession() *piRPCSession {
	ctx, cancel := context.WithCancel(context.Background())
	s := &piRPCSession{
		events:  make(chan core.Event, 64),
		ctx:     ctx,
		cancel:  cancel,
		pending: make(map[string]chan rpcResponse),
	}
	s.alive.Store(true)
	return s
}

func drainRPCEvents(s *piRPCSession) []core.Event {
	var evts []core.Event
	for {
		select {
		case e := <-s.events:
			evts = append(evts, e)
		default:
			return evts
		}
	}
}

func TestHandleEvent_CustomMessageSubagentNotifyEmitsResult(t *testing.T) {
	s := newTestSession()
	defer s.cancel()

	s.handleEvent(map[string]any{
		"type":       "custom_message",
		"customType": "subagent-notify",
		"display":    true,
		"content":    "child done",
	})

	evts := drainEvents(s)
	if len(evts) != 1 {
		t.Fatalf("events len = %d, want 1", len(evts))
	}
	if evts[0].Type != core.EventResult || evts[0].Content != "child done" || !evts[0].Done {
		t.Fatalf("event = %#v, want done EventResult child done", evts[0])
	}
}

func TestHandleEvent_CustomMessageInvisibleOrEmptyEmitsNothing(t *testing.T) {
	s := newTestSession()
	defer s.cancel()

	s.handleEvent(map[string]any{"type": "custom_message", "customType": "subagent-notify", "display": false, "content": "hidden"})
	s.handleEvent(map[string]any{"type": "custom_message", "customType": "other", "display": true, "content": "hidden"})
	s.handleEvent(map[string]any{"type": "custom_message", "customType": "subagent-notify", "display": true, "content": ""})

	if evts := drainEvents(s); len(evts) != 0 {
		t.Fatalf("events len = %d, want 0", len(evts))
	}
}

func TestRPCHandleAgentEvent_CustomMessageSubagentNotifyEmitsResult(t *testing.T) {
	s := newTestRPCSession()
	defer s.cancel()

	s.handleAgentEvent(map[string]any{
		"type":       "custom_message",
		"customType": "subagent-notify",
		"display":    true,
		"content":    "rpc child done",
	})

	evts := drainRPCEvents(s)
	if len(evts) != 1 {
		t.Fatalf("events len = %d, want 1", len(evts))
	}
	if evts[0].Type != core.EventResult || evts[0].Content != "rpc child done" || !evts[0].Done {
		t.Fatalf("event = %#v, want done EventResult rpc child done", evts[0])
	}
}

func TestHandleEvent_Session(t *testing.T) {
	s := newTestSession()
	defer s.cancel()

	s.handleEvent(map[string]any{"type": "session", "id": "sess-abc-123"})
	if s.CurrentSessionID() != "sess-abc-123" {
		t.Errorf("sessionID = %q", s.CurrentSessionID())
	}
}

func TestHandleEvent_SessionEmptyID(t *testing.T) {
	s := newTestSession()
	defer s.cancel()

	s.handleEvent(map[string]any{"type": "session", "id": ""})
	if s.CurrentSessionID() != "" {
		t.Errorf("sessionID = %q, want empty", s.CurrentSessionID())
	}
}

func TestHandleEvent_SessionNoID(t *testing.T) {
	s := newTestSession()
	defer s.cancel()

	s.handleEvent(map[string]any{"type": "session"})
	if s.CurrentSessionID() != "" {
		t.Errorf("sessionID = %q, want empty", s.CurrentSessionID())
	}
}

func TestHandleEvent_LifecycleEventsNoOp(t *testing.T) {
	s := newTestSession()
	defer s.cancel()

	for _, evType := range []string{"agent_start", "agent_end", "turn_start", "turn_end", "message_start"} {
		s.handleEvent(map[string]any{"type": evType})
	}
	evts := drainEvents(s)
	if len(evts) != 0 {
		t.Errorf("expected no events, got %d", len(evts))
	}
}

func TestHandleEvent_UnhandledType(t *testing.T) {
	s := newTestSession()
	defer s.cancel()

	s.handleEvent(map[string]any{"type": "unknown_event"})
	evts := drainEvents(s)
	if len(evts) != 0 {
		t.Errorf("expected no events, got %d", len(evts))
	}
}

// ── handleMessageUpdate: text_delta ──────────────────────────

func TestHandleMessageUpdate_TextDelta(t *testing.T) {
	s := newTestSession()
	defer s.cancel()

	s.handleEvent(map[string]any{
		"type": "message_update",
		"assistantMessageEvent": map[string]any{
			"type":  "text_delta",
			"delta": "Hello world",
		},
	})

	evts := drainEvents(s)
	if len(evts) != 1 {
		t.Fatalf("got %d events, want 1", len(evts))
	}
	if evts[0].Type != core.EventText || evts[0].Content != "Hello world" {
		t.Errorf("event = %+v", evts[0])
	}
}

func TestHandleMessageUpdate_TextDeltaEmpty(t *testing.T) {
	s := newTestSession()
	defer s.cancel()

	s.handleEvent(map[string]any{
		"type": "message_update",
		"assistantMessageEvent": map[string]any{
			"type":  "text_delta",
			"delta": "",
		},
	})

	evts := drainEvents(s)
	if len(evts) != 0 {
		t.Errorf("expected no events for empty delta, got %d", len(evts))
	}
}

// ── handleMessageUpdate: thinking accumulation ───────────────

func TestHandleMessageUpdate_ThinkingAccumulation(t *testing.T) {
	s := newTestSession()
	defer s.cancel()

	// Multiple thinking deltas should be accumulated.
	s.handleEvent(map[string]any{
		"type":                  "message_update",
		"assistantMessageEvent": map[string]any{"type": "thinking_delta", "delta": "Let me "},
	})
	s.handleEvent(map[string]any{
		"type":                  "message_update",
		"assistantMessageEvent": map[string]any{"type": "thinking_delta", "delta": "think about "},
	})
	s.handleEvent(map[string]any{
		"type":                  "message_update",
		"assistantMessageEvent": map[string]any{"type": "thinking_delta", "delta": "this."},
	})

	// No events should be emitted yet.
	evts := drainEvents(s)
	if len(evts) != 0 {
		t.Fatalf("expected no events before thinking_end, got %d", len(evts))
	}

	// thinking_end triggers the accumulated event.
	s.handleEvent(map[string]any{
		"type":                  "message_update",
		"assistantMessageEvent": map[string]any{"type": "thinking_end"},
	})

	evts = drainEvents(s)
	if len(evts) != 1 {
		t.Fatalf("got %d events, want 1", len(evts))
	}
	if evts[0].Type != core.EventThinking {
		t.Errorf("type = %s, want EventThinking", evts[0].Type)
	}
	if evts[0].Content != "Let me think about this." {
		t.Errorf("content = %q", evts[0].Content)
	}
}

func TestHandleMessageUpdate_ThinkingEndEmpty(t *testing.T) {
	s := newTestSession()
	defer s.cancel()

	// thinking_end with no prior deltas should not emit.
	s.handleEvent(map[string]any{
		"type":                  "message_update",
		"assistantMessageEvent": map[string]any{"type": "thinking_end"},
	})

	evts := drainEvents(s)
	if len(evts) != 0 {
		t.Errorf("expected no events for empty thinking, got %d", len(evts))
	}
}

func TestHandleMessageUpdate_ThinkingDeltaEmpty(t *testing.T) {
	s := newTestSession()
	defer s.cancel()

	// Empty deltas should not grow the buffer.
	s.handleEvent(map[string]any{
		"type":                  "message_update",
		"assistantMessageEvent": map[string]any{"type": "thinking_delta", "delta": ""},
	})

	if s.thinkingBuf.Len() != 0 {
		t.Errorf("thinkingBuf.Len() = %d, want 0", s.thinkingBuf.Len())
	}
}

// ── handleMessageUpdate: toolcall_end ────────────────────────

func TestHandleMessageUpdate_ToolcallEnd(t *testing.T) {
	s := newTestSession()
	defer s.cancel()

	s.handleEvent(map[string]any{
		"type": "message_update",
		"assistantMessageEvent": map[string]any{
			"type":         "toolcall_end",
			"contentIndex": float64(1),
			"message": map[string]any{
				"content": []any{
					map[string]any{"type": "thinking", "thinking": "..."},
					map[string]any{
						"type":      "toolCall",
						"name":      "bash",
						"arguments": map[string]any{"command": "ls -la"},
					},
				},
			},
		},
	})

	evts := drainEvents(s)
	if len(evts) != 1 {
		t.Fatalf("got %d events, want 1", len(evts))
	}
	if evts[0].Type != core.EventToolUse {
		t.Errorf("type = %s", evts[0].Type)
	}
	if evts[0].ToolName != "bash" {
		t.Errorf("toolName = %q", evts[0].ToolName)
	}
	if evts[0].ToolInput != "ls -la" {
		t.Errorf("toolInput = %q", evts[0].ToolInput)
	}
}

func TestHandleMessageUpdate_ToolcallEnd_UsesPartialFallback(t *testing.T) {
	s := newTestSession()
	defer s.cancel()

	s.handleEvent(map[string]any{
		"type": "message_update",
		"assistantMessageEvent": map[string]any{
			"type":         "toolcall_end",
			"contentIndex": float64(0),
			"partial": map[string]any{
				"content": []any{
					map[string]any{
						"type":      "toolCall",
						"name":      "read",
						"arguments": map[string]any{"file_path": "/tmp/foo.txt"},
					},
				},
			},
		},
	})

	evts := drainEvents(s)
	if len(evts) != 1 {
		t.Fatalf("got %d events, want 1", len(evts))
	}
	if evts[0].ToolName != "read" || evts[0].ToolInput != "/tmp/foo.txt" {
		t.Errorf("event = %+v", evts[0])
	}
}

func TestHandleMessageUpdate_ToolcallEnd_NonToolCallItem(t *testing.T) {
	s := newTestSession()
	defer s.cancel()

	s.handleEvent(map[string]any{
		"type": "message_update",
		"assistantMessageEvent": map[string]any{
			"type":         "toolcall_end",
			"contentIndex": float64(0),
			"message": map[string]any{
				"content": []any{
					map[string]any{"type": "text", "text": "hello"},
				},
			},
		},
	})

	evts := drainEvents(s)
	if len(evts) != 0 {
		t.Errorf("expected no events for non-toolCall item, got %d", len(evts))
	}
}

func TestHandleMessageUpdate_ToolcallEnd_OutOfBoundsIndex(t *testing.T) {
	s := newTestSession()
	defer s.cancel()

	s.handleEvent(map[string]any{
		"type": "message_update",
		"assistantMessageEvent": map[string]any{
			"type":         "toolcall_end",
			"contentIndex": float64(5),
			"message": map[string]any{
				"content": []any{
					map[string]any{"type": "toolCall", "name": "bash"},
				},
			},
		},
	})

	evts := drainEvents(s)
	if len(evts) != 0 {
		t.Errorf("expected no events for out-of-bounds index, got %d", len(evts))
	}
}

func TestHandleMessageUpdate_ToolcallEnd_NilMessage(t *testing.T) {
	s := newTestSession()
	defer s.cancel()

	s.handleEvent(map[string]any{
		"type": "message_update",
		"assistantMessageEvent": map[string]any{
			"type": "toolcall_end",
			// no "message" or "partial"
		},
	})

	evts := drainEvents(s)
	if len(evts) != 0 {
		t.Errorf("expected no events, got %d", len(evts))
	}
}

func TestHandleMessageUpdate_NilAssistantEvent(t *testing.T) {
	s := newTestSession()
	defer s.cancel()

	s.handleEvent(map[string]any{"type": "message_update"})

	evts := drainEvents(s)
	if len(evts) != 0 {
		t.Errorf("expected no events, got %d", len(evts))
	}
}

func TestHandleMessageUpdate_UnknownSubType(t *testing.T) {
	s := newTestSession()
	defer s.cancel()

	s.handleEvent(map[string]any{
		"type": "message_update",
		"assistantMessageEvent": map[string]any{
			"type": "some_unknown_delta",
		},
	})

	evts := drainEvents(s)
	if len(evts) != 0 {
		t.Errorf("expected no events for unknown sub-type, got %d", len(evts))
	}
}

// ── handleMessageEnd ─────────────────────────────────────────

func TestHandleMessageEnd_ToolResult(t *testing.T) {
	s := newTestSession()
	defer s.cancel()

	s.handleEvent(map[string]any{
		"type": "message_end",
		"message": map[string]any{
			"role":     "toolResult",
			"toolName": "bash",
			"content": []any{
				map[string]any{"type": "text", "text": "file1.go\nfile2.go"},
			},
		},
	})

	evts := drainEvents(s)
	if len(evts) != 1 {
		t.Fatalf("got %d events, want 1", len(evts))
	}
	if evts[0].Type != core.EventToolResult {
		t.Errorf("type = %s", evts[0].Type)
	}
	if evts[0].ToolName != "bash" {
		t.Errorf("toolName = %q", evts[0].ToolName)
	}
	if evts[0].Content != "file1.go\nfile2.go" {
		t.Errorf("content = %q", evts[0].Content)
	}
}

func TestHandleMessageEnd_ToolResultLongOutput(t *testing.T) {
	s := newTestSession()
	defer s.cancel()

	longOutput := strings.Repeat("x", 600)
	s.handleEvent(map[string]any{
		"type": "message_end",
		"message": map[string]any{
			"role":     "toolResult",
			"toolName": "bash",
			"content": []any{
				map[string]any{"type": "text", "text": longOutput},
			},
		},
	})

	evts := drainEvents(s)
	if len(evts) != 1 {
		t.Fatalf("got %d events, want 1", len(evts))
	}
	if len(evts[0].Content) > 510 {
		t.Errorf("content should be truncated, got len=%d", len(evts[0].Content))
	}
}

func TestHandleMessageEnd_ToolResultEmptyContent(t *testing.T) {
	s := newTestSession()
	defer s.cancel()

	s.handleEvent(map[string]any{
		"type": "message_end",
		"message": map[string]any{
			"role":     "toolResult",
			"toolName": "bash",
			"content":  []any{},
		},
	})

	evts := drainEvents(s)
	if len(evts) != 1 {
		t.Fatalf("got %d events, want 1", len(evts))
	}
	if evts[0].Content != "" {
		t.Errorf("content = %q, want empty", evts[0].Content)
	}
}

func TestHandleMessageEnd_AssistantError(t *testing.T) {
	s := newTestSession()
	defer s.cancel()

	s.handleEvent(map[string]any{
		"type": "message_end",
		"message": map[string]any{
			"role":         "assistant",
			"errorMessage": "WebSocket closed 1006 Connection ended",
			"stopReason":   "error",
			"details":      map[string]any{"phase": "after_message_stream_start", "requestBytes": float64(1222867)},
			"diagnostics":  []any{map[string]any{"type": "provider_transport_failure"}},
		},
	})

	evts := drainEvents(s)
	if len(evts) != 1 {
		t.Fatalf("got %d events, want 1", len(evts))
	}
	if evts[0].Type != core.EventError {
		t.Errorf("type = %s", evts[0].Type)
	}
	if evts[0].Error == nil {
		t.Fatal("expected EventError error")
	}
	errText := evts[0].Error.Error()
	for _, want := range []string{"WebSocket closed 1006", "stopReason=error", "phase=after_message_stream_start", "bytes=1222867", "diag=provider_transport_failure"} {
		if !strings.Contains(errText, want) {
			t.Errorf("error %q missing %q", errText, want)
		}
	}
	if s.Alive() {
		t.Error("expected assistant error to mark Pi JSON session dead")
	}
	select {
	case <-s.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("expected assistant error to cancel session context")
	}
}

func TestPiSessionTerminalErrorDoesNotBlockWhenEventsFull(t *testing.T) {
	s := newTestSession()
	defer s.cancel()

	for i := 0; i < cap(s.events); i++ {
		s.events <- core.Event{Type: core.EventHeartbeat}
	}

	done := make(chan struct{})
	go func() {
		s.handleEvent(map[string]any{
			"type": "message_end",
			"message": map[string]any{
				"role":         "assistant",
				"errorMessage": "WebSocket closed 1006 Connection ended",
				"stopReason":   "error",
			},
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("terminal assistant error blocked on full events channel")
	}
	if s.Alive() {
		t.Error("expected terminal assistant error to mark session dead")
	}
	select {
	case <-s.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("expected terminal assistant error to cancel session context")
	}

	evts := drainEvents(s)
	if len(evts) != cap(s.events) {
		t.Fatalf("drained %d events, want channel to remain at capacity %d", len(evts), cap(s.events))
	}
	last := evts[len(evts)-1]
	if last.Type != core.EventError {
		t.Fatalf("last event type = %s, want terminal %s; events=%v", last.Type, core.EventError, evts)
	}
	if last.Error == nil || !strings.Contains(last.Error.Error(), "WebSocket closed 1006") {
		t.Fatalf("terminal error = %v", last.Error)
	}
}

func TestPiSessionReadLoopDoesNotEmitResultAfterTerminalError(t *testing.T) {
	tmpDir := t.TempDir()
	script := filepath.Join(tmpDir, "pi-json-error.sh")
	errLine := `{"type":"message_end","message":{"role":"assistant","errorMessage":"WebSocket closed 1006 Connection ended","stopReason":"error","diagnostics":[{"type":"provider_transport_failure"}]}}`
	finalLine := `{"type":"message_end","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"final answer that must not become EventResult"}]}}`
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' '"+errLine+"'\nprintf '%s\\n' '"+finalLine+"'\n"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	s, err := newPiSession(context.Background(), script, tmpDir, "", "", "", "", nil)
	if err != nil {
		t.Fatalf("newPiSession: %v", err)
	}
	defer s.Close()

	if err := s.Send("hello", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}

	select {
	case evt := <-s.Events():
		if evt.Type != core.EventError {
			t.Fatalf("first event type = %s, want %s", evt.Type, core.EventError)
		}
		if evt.Error == nil || !strings.Contains(evt.Error.Error(), "WebSocket closed 1006") {
			t.Fatalf("event error = %v", evt.Error)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for EventError")
	}

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("readLoop did not exit after terminal error")
	}

	select {
	case evt := <-s.Events():
		if evt.Type == core.EventResult {
			t.Fatalf("unexpected EventResult after terminal error: %+v", evt)
		}
		// A stderr/process error would also be a regression: terminal-error
		// cancellation should not enqueue follow-up events after EventError.
		t.Fatalf("unexpected follow-up event after terminal error: %+v", evt)
	default:
	}
}

func TestHandleMessageEnd_AssistantNoError(t *testing.T) {
	s := newTestSession()
	defer s.cancel()

	s.handleEvent(map[string]any{
		"type": "message_end",
		"message": map[string]any{
			"role": "assistant",
		},
	})

	evts := drainEvents(s)
	if len(evts) != 0 {
		t.Errorf("expected no events for assistant without error, got %d", len(evts))
	}
}

func TestHandleMessageEnd_AssistantFinalTextFallback(t *testing.T) {
	s := newTestSession()
	defer s.cancel()

	s.handleEvent(map[string]any{
		"type": "message_end",
		"message": map[string]any{
			"role":       "assistant",
			"stopReason": "stop",
			"content": []any{
				map[string]any{"type": "thinking", "thinking": "hidden"},
				map[string]any{"type": "text", "text": "real final answer"},
				map[string]any{"type": "text", "text": " with suffix"},
			},
			"usage": map[string]any{"input": float64(123), "output": float64(45)},
		},
	})

	if evts := drainEvents(s); len(evts) != 0 {
		t.Fatalf("expected no immediate events, got %d", len(evts))
	}
	if got := s.finalTextBuf.String(); got != "real final answer with suffix" {
		t.Fatalf("finalTextBuf = %q", got)
	}
	if s.inputTokens != 123 || s.outputTokens != 45 {
		t.Fatalf("usage = %d/%d, want 123/45", s.inputTokens, s.outputTokens)
	}
}

func TestHandleMessageEnd_AssistantFinalSignatureFallback(t *testing.T) {
	s := newTestSession()
	defer s.cancel()

	s.handleEvent(map[string]any{
		"type": "message_end",
		"message": map[string]any{
			"role": "assistant",
			"content": []any{
				map[string]any{
					"type":          "text",
					"text":          "signed final answer",
					"textSignature": map[string]any{"phase": "final_answer"},
				},
			},
		},
	})

	if evts := drainEvents(s); len(evts) != 0 {
		t.Fatalf("expected no immediate events, got %d", len(evts))
	}
	if got := s.finalTextBuf.String(); got != "signed final answer" {
		t.Fatalf("finalTextBuf = %q", got)
	}
}

func TestHandleMessageEnd_AssistantToolUseIgnored(t *testing.T) {
	s := newTestSession()
	defer s.cancel()

	s.handleEvent(map[string]any{
		"type": "message_end",
		"message": map[string]any{
			"role":       "assistant",
			"stopReason": "toolUse",
			"content": []any{
				map[string]any{
					"type":          "text",
					"text":          "not a final answer",
					"textSignature": map[string]any{"phase": "final_answer"},
				},
				map[string]any{"type": "toolCall", "name": "bash"},
			},
		},
	})

	if evts := drainEvents(s); len(evts) != 0 {
		t.Fatalf("expected no events, got %d", len(evts))
	}
	if got := s.finalTextBuf.String(); got != "" {
		t.Fatalf("finalTextBuf = %q, want empty", got)
	}
}

func TestHandleTurnEnd_AssistantErrorIgnored(t *testing.T) {
	s := newTestSession()
	defer s.cancel()

	s.handleEvent(map[string]any{
		"type": "turn_end",
		"message": map[string]any{
			"role":         "assistant",
			"errorMessage": "400 model not supported",
			"content": []any{
				map[string]any{
					"type":          "text",
					"text":          "not a successful final answer",
					"textSignature": map[string]any{"phase": "final_answer"},
				},
			},
		},
	})

	if evts := drainEvents(s); len(evts) != 0 {
		t.Fatalf("expected no events, got %d", len(evts))
	}
	if got := s.finalTextBuf.String(); got != "" {
		t.Fatalf("finalTextBuf = %q, want empty", got)
	}
}

func TestHandleTurnEnd_AssistantFinalTextFallbackNoEvent(t *testing.T) {
	s := newTestSession()
	defer s.cancel()

	s.handleEvent(map[string]any{
		"type": "turn_end",
		"message": map[string]any{
			"role":       "assistant",
			"stopReason": "stop",
			"content": []any{
				map[string]any{"type": "text", "text": "turn final answer"},
			},
		},
	})

	if evts := drainEvents(s); len(evts) != 0 {
		t.Fatalf("expected no immediate events, got %d", len(evts))
	}
	if got := s.finalTextBuf.String(); got != "turn final answer" {
		t.Fatalf("finalTextBuf = %q", got)
	}
}

func TestPiSession_ResponseStateResetsBetweenTurns(t *testing.T) {
	s := newTestSession()
	defer s.cancel()

	s.finalTextBuf.WriteString("stale final")
	s.emittedTextDelta = true
	s.inputTokens = 10
	s.outputTokens = 20

	s.resetResponseState()

	if got := s.finalTextBuf.String(); got != "" {
		t.Fatalf("finalTextBuf = %q, want empty", got)
	}
	if s.emittedTextDelta {
		t.Fatal("emittedTextDelta = true, want false")
	}
	if s.inputTokens != 0 || s.outputTokens != 0 {
		t.Fatalf("usage = %d/%d, want 0/0", s.inputTokens, s.outputTokens)
	}
}

func TestHandleMessageEnd_NilMessage(t *testing.T) {
	s := newTestSession()
	defer s.cancel()

	s.handleEvent(map[string]any{"type": "message_end"})

	evts := drainEvents(s)
	if len(evts) != 0 {
		t.Errorf("expected no events, got %d", len(evts))
	}
}

func TestHandleMessageEnd_UserRole(t *testing.T) {
	s := newTestSession()
	defer s.cancel()

	s.handleEvent(map[string]any{
		"type": "message_end",
		"message": map[string]any{
			"role": "user",
		},
	})

	evts := drainEvents(s)
	if len(evts) != 0 {
		t.Errorf("expected no events for user role, got %d", len(evts))
	}
}

// ── piSession lifecycle ──────────────────────────────────────

func TestPiSession_NewWithResumeID(t *testing.T) {
	s, err := newPiSession(context.Background(), "echo", "/tmp", "model", "default", "", "resume-id", nil)
	if err != nil {
		t.Fatalf("newPiSession: %v", err)
	}
	defer s.Close()

	if s.CurrentSessionID() != "resume-id" {
		t.Errorf("sessionID = %q", s.CurrentSessionID())
	}
}

func TestPiSession_ContinueSessionTreatedAsFresh(t *testing.T) {
	// ContinueSession ("__continue__") is a sentinel used by the engine to tell
	// Claude Code to pick up the latest CLI session via --continue. Agents that
	// don't support --continue must treat it as "" (fresh session), otherwise
	// they pass the literal "__continue__" as a session ID which always fails.
	s, err := newPiSession(context.Background(), "echo", "/tmp", "", "default", "", core.ContinueSession, nil)
	if err != nil {
		t.Fatalf("newPiSession: %v", err)
	}
	defer s.Close()

	if got := s.CurrentSessionID(); got != "" {
		t.Errorf("ContinueSession should be treated as fresh: sessionID = %q, want empty", got)
	}
}

func TestPiSession_NewWithoutResumeID(t *testing.T) {
	s, err := newPiSession(context.Background(), "echo", "/tmp", "", "default", "", "", nil)
	if err != nil {
		t.Fatalf("newPiSession: %v", err)
	}
	defer s.Close()

	if s.CurrentSessionID() != "" {
		t.Errorf("sessionID = %q, want empty", s.CurrentSessionID())
	}
}

func TestPiSession_SendWhenClosed(t *testing.T) {
	s, _ := newPiSession(context.Background(), "echo", "/tmp", "", "default", "", "", nil)
	s.Close()

	err := s.Send("hello", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "closed") {
		t.Errorf("expected 'closed' error, got %v", err)
	}
}

func TestPiSession_RespondPermission(t *testing.T) {
	s := newTestSession()
	defer s.cancel()

	if err := s.RespondPermission("id", core.PermissionResult{}); err != nil {
		t.Errorf("RespondPermission() error = %v", err)
	}
}

func TestPiSession_Events(t *testing.T) {
	s := newTestSession()
	defer s.cancel()

	ch := s.Events()
	if ch == nil {
		t.Fatal("Events() returned nil")
	}
}

func TestPiSession_Close(t *testing.T) {
	s, _ := newPiSession(context.Background(), "echo", "/tmp", "", "default", "", "", nil)

	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if s.Alive() {
		t.Error("session should not be alive after Close()")
	}
}

func TestPiAgentSupportsContextCompression(t *testing.T) {
	agent, err := New(map[string]any{"cmd": "echo"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	supporter, ok := agent.(core.ContextCompressionSupporter)
	if !ok {
		t.Fatal("Pi agent should implement ContextCompressionSupporter")
	}
	if !supporter.SupportsContextCompression() {
		t.Fatal("SupportsContextCompression() = false, want true")
	}
}

func writeFakePiRPC(t *testing.T, dir string) (string, string, string) {
	t.Helper()
	argsPath := filepath.Join(dir, "args.txt")
	cmdPath := filepath.Join(dir, "cmd.json")
	scriptPath := filepath.Join(dir, "fake-pi-rpc.sh")
	script := `#!/bin/sh
printf '%s\n' "$@" > "` + argsPath + `"
IFS= read -r line
printf '%s\n' "$line" > "` + cmdPath + `"
id=$(printf '%s' "$line" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
printf '{"id":"%s","type":"response","command":"compact","success":true}\n' "$id"
sleep 5
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake rpc script: %v", err)
	}
	return scriptPath, argsPath, cmdPath
}

func readRecordedRPCCommand(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read recorded command: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(data), &got); err != nil {
		t.Fatalf("unmarshal recorded command %q: %v", string(data), err)
	}
	return got
}

func TestPiRPCSessionCompactContextSendsCompactCommand(t *testing.T) {
	tmp := t.TempDir()
	fake, _, cmdPath := writeFakePiRPC(t, tmp)
	s, err := newPiRPCSession(context.Background(), fake, tmp, "", "default", "", "", nil)
	if err != nil {
		t.Fatalf("newPiRPCSession: %v", err)
	}
	defer s.Close()

	if err := s.CompactContext(); err != nil {
		t.Fatalf("CompactContext() error = %v", err)
	}
	got := readRecordedRPCCommand(t, cmdPath)
	if got["type"] != "compact" {
		t.Fatalf("RPC command type = %v, want compact; full command=%v", got["type"], got)
	}
	if _, ok := got["message"]; ok {
		t.Fatalf("compact RPC should not include prompt message: %v", got)
	}
}

func TestPiSessionCompactContextUsesRPCResume(t *testing.T) {
	tmp := t.TempDir()
	fake, argsPath, cmdPath := writeFakePiRPC(t, tmp)
	s, err := newPiSession(context.Background(), fake, tmp, "", "default", "", "json-sess-123", nil)
	if err != nil {
		t.Fatalf("newPiSession: %v", err)
	}
	defer s.Close()

	if err := s.CompactContext(); err != nil {
		t.Fatalf("CompactContext() error = %v", err)
	}
	got := readRecordedRPCCommand(t, cmdPath)
	if got["type"] != "compact" {
		t.Fatalf("RPC command type = %v, want compact; full command=%v", got["type"], got)
	}
	argsBytes, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	args := string(argsBytes)
	if !strings.Contains(args, "--mode\nrpc") || !strings.Contains(args, "--session\njson-sess-123") {
		t.Fatalf("transient RPC args = %q, want rpc mode resumed session", args)
	}
}

func TestPiSessionCompactContextRequiresKnownSession(t *testing.T) {
	s, err := newPiSession(context.Background(), "echo", t.TempDir(), "", "default", "", "", nil)
	if err != nil {
		t.Fatalf("newPiSession: %v", err)
	}
	defer s.Close()

	if err := s.CompactContext(); err == nil || !strings.Contains(err.Error(), "session id") {
		t.Fatalf("CompactContext() error = %v, want missing session id error", err)
	}
}

func TestPiSessionCompactionEvents(t *testing.T) {
	s := newTestSession()
	defer s.cancel()

	s.handleEvent(map[string]any{"type": "compaction_start"})
	s.handleEvent(map[string]any{"type": "compaction_end"})
	s.handleEvent(map[string]any{"type": "compaction_end", "errorMessage": "boom"})
	evts := drainEvents(s)

	if len(evts) != 3 {
		t.Fatalf("got %d events, want 3: %+v", len(evts), evts)
	}
	if evts[0].Type != core.EventThinking || !strings.Contains(evts[0].Content, "started") {
		t.Fatalf("start event = %+v", evts[0])
	}
	if evts[1].Type != core.EventThinking || !strings.Contains(evts[1].Content, "completed") {
		t.Fatalf("end event = %+v", evts[1])
	}
	if evts[2].Type != core.EventError || evts[2].Error == nil || !strings.Contains(evts[2].Error.Error(), "boom") {
		t.Fatalf("error event = %+v", evts[2])
	}
}

func TestPiRPCSessionCompactionEvents(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &piRPCSession{events: make(chan core.Event, 64), ctx: ctx, cancel: cancel}
	s.alive.Store(true)

	s.handleAgentEvent(map[string]any{"type": "compaction_start"})
	s.handleAgentEvent(map[string]any{"type": "compaction_end"})
	s.handleAgentEvent(map[string]any{"type": "compaction_end", "errorMessage": "boom"})

	var evts []core.Event
	for {
		select {
		case ev := <-s.events:
			evts = append(evts, ev)
		default:
			goto done
		}
	}
done:
	if len(evts) != 3 {
		t.Fatalf("got %d events, want 3: %+v", len(evts), evts)
	}
	if evts[0].Type != core.EventThinking || !strings.Contains(evts[0].Content, "started") {
		t.Fatalf("start event = %+v", evts[0])
	}
	if evts[1].Type != core.EventThinking || !strings.Contains(evts[1].Content, "completed") {
		t.Fatalf("end event = %+v", evts[1])
	}
	if evts[2].Type != core.EventError || evts[2].Error == nil || !strings.Contains(evts[2].Error.Error(), "boom") {
		t.Fatalf("error event = %+v", evts[2])
	}
}

// ── Full event stream simulation ─────────────────────────────

func TestHandleEvent_FullConversation(t *testing.T) {
	s := newTestSession()
	defer s.cancel()

	// Simulate a full pi conversation: session → thinking → text → tool → tool result → text → done
	events := []map[string]any{
		{"type": "session", "id": "conv-123"},
		{"type": "agent_start"},
		{"type": "turn_start"},
		{"type": "message_start"},
		{"type": "message_update", "assistantMessageEvent": map[string]any{
			"type": "thinking_delta", "delta": "I need to ",
		}},
		{"type": "message_update", "assistantMessageEvent": map[string]any{
			"type": "thinking_delta", "delta": "list files.",
		}},
		{"type": "message_update", "assistantMessageEvent": map[string]any{
			"type": "thinking_end",
		}},
		{"type": "message_update", "assistantMessageEvent": map[string]any{
			"type":         "toolcall_end",
			"contentIndex": float64(1),
			"message": map[string]any{
				"content": []any{
					map[string]any{"type": "thinking"},
					map[string]any{"type": "toolCall", "name": "bash", "arguments": map[string]any{"command": "ls"}},
				},
			},
		}},
		{"type": "message_end", "message": map[string]any{
			"role": "assistant", "stopReason": "toolUse",
		}},
		{"type": "message_end", "message": map[string]any{
			"role": "toolResult", "toolName": "bash",
			"content": []any{map[string]any{"type": "text", "text": "file1.go"}},
		}},
		{"type": "turn_end"},
		{"type": "turn_start"},
		{"type": "message_update", "assistantMessageEvent": map[string]any{
			"type": "text_delta", "delta": "Here are your files.",
		}},
		{"type": "message_end", "message": map[string]any{
			"role": "assistant", "stopReason": "stop",
		}},
		{"type": "turn_end"},
		{"type": "agent_end"},
	}

	for _, ev := range events {
		s.handleEvent(ev)
	}

	evts := drainEvents(s)

	// Expected: thinking, tool_use, tool_result, text
	if len(evts) != 4 {
		var types []string
		for _, e := range evts {
			types = append(types, string(e.Type))
		}
		t.Fatalf("got %d events %v, want 4", len(evts), types)
	}

	if evts[0].Type != core.EventThinking || evts[0].Content != "I need to list files." {
		t.Errorf("evts[0] = %+v", evts[0])
	}
	if evts[1].Type != core.EventToolUse || evts[1].ToolName != "bash" {
		t.Errorf("evts[1] = %+v", evts[1])
	}
	if evts[2].Type != core.EventToolResult || evts[2].Content != "file1.go" {
		t.Errorf("evts[2] = %+v", evts[2])
	}
	if evts[3].Type != core.EventText || evts[3].Content != "Here are your files." {
		t.Errorf("evts[3] = %+v", evts[3])
	}

	if s.CurrentSessionID() != "conv-123" {
		t.Errorf("sessionID = %q", s.CurrentSessionID())
	}
}

// ── readLoop with real process ───────────────────────────────

func TestPiSession_ReadLoopHeartbeatStopsBeforeResult(t *testing.T) {
	oldInterval := piJSONHeartbeatInterval
	piJSONHeartbeatInterval = 20 * time.Millisecond
	t.Cleanup(func() { piJSONHeartbeatInterval = oldInterval })

	sessionEvent := map[string]any{"type": "session", "id": "heartbeat-sess"}
	line, _ := json.Marshal(sessionEvent)
	script := "sleep 0.09; printf '%s\\n' '" + string(line) + "'"

	s, err := newPiSession(context.Background(), "sh", "/tmp", "", "default", "", "", nil)
	if err != nil {
		t.Fatalf("newPiSession: %v", err)
	}

	cmd := exec.CommandContext(s.ctx, "sh", "-c", script)
	cmd.Dir = "/tmp"

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	s.wg.Add(1)
	go s.readLoop(cmd, stdout, &stderrBuf)

	var evts []core.Event
	timeout := time.After(5 * time.Second)
loop:
	for {
		select {
		case ev := <-s.Events():
			evts = append(evts, ev)
			if ev.Type == core.EventResult {
				break loop
			}
		case <-timeout:
			t.Fatal("timeout waiting for heartbeat/result events")
		}
	}

	seenHeartbeat := false
	seenResult := false
	for _, ev := range evts {
		switch ev.Type {
		case core.EventHeartbeat:
			if seenResult {
				t.Fatalf("heartbeat arrived after result in collected events: %+v", evts)
			}
			seenHeartbeat = true
			if !ev.Synthetic {
				t.Fatal("heartbeat should be synthetic")
			}
			if source, _ := ev.Metadata["source"].(string); source != "pi_json_process_alive" {
				t.Fatalf("heartbeat source = %q, want pi_json_process_alive", source)
			}
		case core.EventResult:
			seenResult = true
		}
	}
	if !seenHeartbeat {
		t.Fatalf("missing heartbeat before result; events: %+v", evts)
	}
	if !seenResult {
		t.Fatalf("missing result; events: %+v", evts)
	}

	select {
	case ev := <-s.Events():
		if ev.Type == core.EventHeartbeat {
			t.Fatalf("heartbeat arrived after result: %+v", ev)
		}
	case <-time.After(50 * time.Millisecond):
	}

	s.Close()
}

func TestPiSession_ReadLoopWithEcho(t *testing.T) {
	// Use sh -c to simulate pi JSON output on stdout.
	sessionEvent := map[string]any{"type": "session", "id": "echo-sess"}
	textEvent := map[string]any{
		"type": "message_update",
		"assistantMessageEvent": map[string]any{
			"type":  "text_delta",
			"delta": "hi",
		},
	}
	line1, _ := json.Marshal(sessionEvent)
	line2, _ := json.Marshal(textEvent)

	s, err := newPiSession(context.Background(), "sh", "/tmp", "", "default", "", "", nil)
	if err != nil {
		t.Fatalf("newPiSession: %v", err)
	}

	// sh -c 'echo ...; echo ...' will output our JSON lines.
	// We need to override how Send builds args. Instead, call readLoop directly.
	// Actually, just use Send with sh -c and craft the prompt as the script.
	script := "echo '" + string(line1) + "'; echo '" + string(line2) + "'"

	// Manually build the command since Send adds extra flags for pi.
	s.cmd = "sh"
	s.model = "" // prevent --model flag

	// Directly test readLoop via Send by crafting args that sh understands.
	// Send will run: sh --mode json -p <script> which sh won't understand.
	// Instead, test readLoop directly.
	ctx := s.ctx
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	cmd.Dir = "/tmp"

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	s.wg.Add(1)
	go s.readLoop(cmd, stdout, &stderrBuf)

	// Collect events with timeout.
	var evts []core.Event
	timeout := time.After(5 * time.Second)
loop:
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				break loop
			}
			evts = append(evts, ev)
			if ev.Type == core.EventResult {
				break loop
			}
		case <-timeout:
			t.Fatal("timeout waiting for events")
		}
	}

	s.Close()

	// Should have at least a text event and a result event.
	hasText := false
	hasResult := false
	for _, ev := range evts {
		if ev.Type == core.EventText && ev.Content == "hi" {
			hasText = true
		}
		if ev.Type == core.EventResult {
			hasResult = true
		}
	}
	if !hasText {
		t.Error("missing text event")
	}
	if !hasResult {
		t.Error("missing result event")
	}
	if s.CurrentSessionID() != "echo-sess" {
		t.Errorf("sessionID = %q, want echo-sess", s.CurrentSessionID())
	}
}

func TestPiSession_ReadLoopUsesFinalTextOnlyResultContent(t *testing.T) {
	sessionEvent := map[string]any{"type": "session", "id": "final-sess"}
	finalEvent := map[string]any{
		"type": "message_end",
		"message": map[string]any{
			"role":       "assistant",
			"stopReason": "stop",
			"content": []any{
				map[string]any{"type": "text", "text": "real final from message end"},
			},
		},
	}

	evts := runReadLoopEvents(t, sessionEvent, finalEvent)

	var result *core.Event
	for i := range evts {
		if evts[i].Type == core.EventText {
			t.Fatalf("unexpected text event: %+v", evts[i])
		}
		if evts[i].Type == core.EventResult {
			result = &evts[i]
		}
	}
	if result == nil {
		t.Fatal("missing result event")
	}
	if result.Content != "real final from message end" {
		t.Fatalf("result content = %q", result.Content)
	}
}

func TestPiSession_ReadLoopTextDeltaSuppressesDuplicateResultContent(t *testing.T) {
	sessionEvent := map[string]any{"type": "session", "id": "delta-sess"}
	textEvent := map[string]any{
		"type": "message_update",
		"assistantMessageEvent": map[string]any{
			"type":  "text_delta",
			"delta": "real final from delta",
		},
	}
	finalEvent := map[string]any{
		"type": "message_end",
		"message": map[string]any{
			"role":       "assistant",
			"stopReason": "stop",
			"content": []any{
				map[string]any{"type": "text", "text": "real final from delta"},
			},
		},
	}

	evts := runReadLoopEvents(t, sessionEvent, textEvent, finalEvent)

	var textCount int
	var result *core.Event
	for i := range evts {
		switch evts[i].Type {
		case core.EventText:
			textCount++
			if evts[i].Content != "real final from delta" {
				t.Fatalf("text content = %q", evts[i].Content)
			}
		case core.EventResult:
			result = &evts[i]
		}
	}
	if textCount != 1 {
		t.Fatalf("text event count = %d, want 1", textCount)
	}
	if result == nil {
		t.Fatal("missing result event")
	}
	if result.Content != "" {
		t.Fatalf("result content = %q, want empty to avoid duplicate", result.Content)
	}
}

func runReadLoopEvents(t *testing.T, events ...map[string]any) []core.Event {
	t.Helper()

	var script strings.Builder
	for _, event := range events {
		line, err := json.Marshal(event)
		if err != nil {
			t.Fatalf("marshal event: %v", err)
		}
		script.WriteString("echo '")
		script.WriteString(string(line))
		script.WriteString("';")
	}

	s, err := newPiSession(context.Background(), "sh", "/tmp", "", "default", "", "", nil)
	if err != nil {
		t.Fatalf("newPiSession: %v", err)
	}
	defer s.Close()

	cmd := exec.CommandContext(s.ctx, "sh", "-c", script.String())
	cmd.Dir = "/tmp"
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	s.wg.Add(1)
	go s.readLoop(cmd, stdout, &stderrBuf)

	var evts []core.Event
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev := <-s.Events():
			evts = append(evts, ev)
			if ev.Type == core.EventResult {
				return evts
			}
		case <-timeout:
			t.Fatal("timeout waiting for events")
		}
	}
}
