# Issue #1 fix plan: prevent quiet Pi JSON turns from tripping cc-connect idle watchdog

## Problem

Pi JSON sessions can stay legitimately quiet for longer than `idle_timeout_mins` while the Pi subprocess is still alive (for example, long model/tool/subagent work with no stdout NDJSON). `core.Engine.processInteractiveEvents` treats "no cc-connect-visible agent events" as a hung session and kills the interactive state after the default 2h.

Existing config escape hatch (`idle_timeout_mins = 0`) works, but disables stuck-session protection globally. Minimal code fix should make cc-connect see process-alive progress for Pi JSON turns without changing platform UX.

## Minimal surgical fix

Add a synthetic, non-user-visible heartbeat event emitted by the Pi JSON adapter while a spawned `pi --mode json -p ...` process is alive.

### Files/functions

1. `core/message.go`
   - Add event type:
     ```go
     EventHeartbeat EventType = "heartbeat" // synthetic liveness/progress ping; not user-visible
     ```
   - No new fields needed; optional `Metadata` can carry source details if desired.

2. `agent/pi/session.go`
   - Add an overrideable package variable near the session code:
     ```go
     var piJSONHeartbeatInterval = time.Minute
     ```
   - In `(*piSession).readLoop`, start a heartbeat goroutine/ticker tied to that specific subprocess and stop it when `readLoop` exits.
   - Heartbeat event:
     ```go
     core.Event{
         Type: core.EventHeartbeat,
         Metadata: map[string]any{"source": "pi_json_process_alive"},
         Synthetic: true,
     }
     ```
   - Send with the same non-blocking/context-aware pattern used elsewhere:
     ```go
     select {
     case s.events <- evt:
     case <-s.ctx.Done():
         return
     case <-done:
         return
     }
     ```
   - Ensure the ticker is stopped and its goroutine has exited before `EventResult` is emitted, so no heartbeat can arrive after final result and confuse queued-turn draining.

3. `core/engine.go`
   - Add an explicit no-op switch case in both event loops for readability:
     - `processInteractiveEvents`: `case EventHeartbeat: continue`
     - `processCompressEvents`: `case EventHeartbeat: continue`
   - Current code already resets `idleTimer` before the switch for any received event, so this is mostly documentation/guardrail. Do not send chat messages, append history, update cards, or increment tool/text counters.

## Regression tests

1. `core/engine_test.go`
   - Add `TestEventIdleTimeout_HeartbeatResetsTimer` near existing idle timeout tests.
   - Setup like `TestEventIdleTimeout_ResetOnEvent`.
   - `e.SetEventIdleTimeout(120 * time.Millisecond)`.
   - Send `EventHeartbeat` every ~60ms for several timeout windows, then `EventResult`.
   - Assert no timeout message was sent and process loop completed.
   - This proves heartbeats reset the watchdog and remain user-invisible.

2. `agent/pi/pi_test.go`
   - Add a test for Pi JSON heartbeat emission without waiting a real minute by making interval injectable, e.g. package var:
     ```go
     var piJSONHeartbeatInterval = time.Minute
     ```
     In test, temporarily set to `20 * time.Millisecond` and restore with `t.Cleanup`.
   - Run `readLoop` against a command that sleeps long enough then emits a valid final/session line or exits cleanly.
   - Assert at least one `EventHeartbeat` appears before `EventResult`, and no `EventHeartbeat` appears after `EventResult`.

3. Existing targeted tests to run:
   ```bash
   go test ./agent/pi ./core -run 'TestPiSession|TestEventIdleTimeout|Heartbeat|IdleTimeout' -count=1
   ```
   Before merge, run:
   ```bash
   go test ./...
   ```

## Risks / tradeoffs

- Main risk: a Pi child process that is alive but internally wedged will now keep resetting cc-connect's inter-agent-event watchdog forever. This is narrower than setting `idle_timeout_mins = 0` globally, but it does weaken stuck-child detection for Pi JSON turns.
- Mitigation: heartbeat only in `agent/pi` JSON adapter, not all agents. Operators can still kill sessions manually. Future improvement can add a separate per-turn max wall-clock timeout or process-health watchdog.
- Heartbeat events must remain non-user-visible. Do not map them to `EventThinking` or text; that would spam platforms and alter UX.
- Keep interval comfortably below default 2h; `1m` is low overhead and testable.
- Need to avoid race where heartbeat fires after `EventResult`; stop heartbeat before final result send.

## pi-extensions scope

Out of scope. This issue is in cc-connect's Pi JSON transport + engine watchdog interaction. `pi-extensions` and Pi RPC slash-command extension behavior do not need changes for the minimal fix. RPC transport may later benefit from a similar heartbeat if it has the same quiet-period behavior, but local production evidence points at JSON transport.

## Non-goals

- Do not change default `idle_timeout_mins`.
- Do not globally disable the engine watchdog.
- Do not modify GitHub issues/comments.
- Do not modify pi-extensions.
- Do not change platform rendering or progress-card behavior.
