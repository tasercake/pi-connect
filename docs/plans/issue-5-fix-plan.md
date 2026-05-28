# Issue #5 Fix Plan: Pi Context Safety for Long pi-connect Sessions

## Scope
Implement the fix in `tasercake/pi-connect` only. The concrete failure path is pi-connect's Pi adapter and engine integration: Pi sessions are resumed indefinitely, Pi does not satisfy `core.ContextCompressor`, Pi usage telemetry is not surfaced as cumulative context load, and the adapter emits terminal errors before Pi's own overflow compaction/retry can recover.

## Root causes addressed
1. Pi agent does not implement `core.ContextCompressor`, so `/compress` and `[projects.auto_compress]` cannot compact Pi sessions.
2. Pi usage events include `cacheRead`/`totalTokens`, but pi-connect only sees `input/output` or zero, so logs/context indicators/auto-compress miss cumulative context load.
3. Engine auto-compress uses pi-connect's text-history estimate; this misses tool-heavy Pi sessions where provider context is dominated by cached/tool messages.
4. Existing deployments with `agent.type = "pi"` and no `[projects.auto_compress]` remain unprotected.
5. Pi may emit a provider `context_length_exceeded` assistant error before its built-in overflow compaction record/retry; the adapter currently forwards that immediately as terminal.

## Implementation

### 1. Pi compression capability
File: `agent/pi/pi.go`
- Add `func (a *Agent) CompressCommand() string { return "/compact" }`.

Tests: `agent/pi/pi_test.go`
- Add `TestAgent_CompressCommand`.

### 2. Pi cumulative usage telemetry
Files: `agent/pi/session.go`, `agent/pi/session_rpc.go`
- Add a small shared Pi usage parser for `usage.input`, `usage.output`, `usage.cacheRead`, `usage.cacheWrite`, and `usage.totalTokens`.
- Parse usage from all Pi event shapes pi-connect may receive: JSON `message_end.message.usage`, JSON `message.usage` records, RPC `turn_end.message.usage`, and RPC `agent_end.messages[].usage`.
- Compute current context load as `totalTokens` when present; otherwise `input + cacheRead + cacheWrite + output`.
- Cache latest current context usage in each Pi session behind a mutex; `GetContextUsage()` must return the latest current load, not a forever max, so compaction can lower future readings. If a max is needed for a final event, keep it separate and turn-scoped.
- Implement `GetContextUsage() *core.ContextUsage` for both JSON and RPC sessions, returning a copy.
- Final `EventResult` should carry useful fallback telemetry from the latest turn: `InputTokens = UsedTokens`, `OutputTokens = output`.

Tests: `agent/pi/pi_test.go`
- JSON message usage fixture matching issue evidence (`message.usage.totalTokens/cacheRead`) parses and `GetContextUsage` returns a clone.
- JSON `message_end` fallback computes context load when `totalTokens` is absent.
- RPC turn-end and/or agent-end usage parses `totalTokens/cacheRead` without summing cumulative `totalTokens` across messages; `GetContextUsage` returns a clone.

### 3. Engine auto-compress uses runtime context load
File: `core/engine.go`
- In `processInteractiveEvents` `EventResult` handling, set the auto-compress estimate to the max of:
  - existing `estimateTokensWithPendingAssistant` result,
  - `ContextUsageReporter.GetContextUsage().UsedTokens` for the active agent session,
  - plausible `event.InputTokens`.

Tests: `core/engine_test.go`
- Add a regression where session history is tiny but `ContextUsageReporter.UsedTokens` exceeds threshold; auto-compress must send `/compact`.

### 4. Safe Pi default auto-compress
Files: `cmd/pi-connect/main.go`, `config.example.toml`, config tests if needed
- Preserve explicit config: `auto_compress.enabled = false` disables; `true` enables.
- If `auto_compress.enabled` is omitted and `project.agent.type` is `pi`, enable auto-compress by default with a conservative high threshold (default `max_tokens = 200000`, `min_gap_mins = 30`).
- Keep non-Pi default disabled.

Tests: add/adjust focused command/config wiring tests only if an existing test harness covers this path cheaply; otherwise verify with `go test ./cmd/pi-connect`.

### 5. Suppress recoverable Pi overflow errors
Files: `agent/pi/session.go`, `agent/pi/session_rpc.go`
- Detect assistant `errorMessage` containing `context_length_exceeded` or `exceeds the context window`.
- Store it as pending instead of immediately emitting `EventError`; this is required because pi-connect cannot send `/compact` while a Pi JSON/RPC turn is still running, and the observed failure happened mid-turn before Pi's own overflow compaction/retry.
- Clear the pending overflow error when a later assistant message/turn completes successfully after compaction/retry.
- At process/read-loop termination, if a pending overflow error remains and no successful final text was produced, emit the error before final result.

Tests: `agent/pi/pi_test.go`
- Overflow error is suppressed initially.
- Overflow error followed by later successful assistant message/turn clears the pending error and no terminal `EventError` reaches the caller.
- Pending overflow error is emitted on EOF/finalization when no recovery occurred (unit via helper if needed).

## Verification
Run:
```bash
gofmt -w agent/pi/pi.go agent/pi/session.go agent/pi/session_rpc.go agent/pi/pi_test.go core/engine.go core/engine_test.go cmd/pi-connect/main.go
go test ./agent/pi ./core ./cmd/pi-connect
go test ./...
```

## Non-goals
- Do not modify GitHub issues/comments/labels.
- Do not change pi-extensions in this PR.
- Do not implement broad session rotation or provider-specific context windows.
