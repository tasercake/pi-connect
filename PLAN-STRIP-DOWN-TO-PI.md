# Plan: Strip cc-connect Down to Pi-Only Agent

**Scope doc:** `SCOPE-STRIP-DOWN-TO-PI.md` (immutable, read-only)  
**Branch:** `strip-down-to-pi`  
**Worktree:** `/home/exedev/src/cc-connect-pi`  

---

## Phase 1: Bulk Deletions

### 1.1 Delete 10 agent source directories

```
rm -rf agent/acp
rm -rf agent/claudecode
rm -rf agent/codex
rm -rf agent/cursor
rm -rf agent/devin
rm -rf agent/gemini
rm -rf agent/iflow
rm -rf agent/kimi
rm -rf agent/opencode
rm -rf agent/qoder
```

### 1.2 Delete 10 agent plugin import files

```
rm cmd/cc-connect/plugin_agent_acp.go
rm cmd/cc-connect/plugin_agent_claudecode.go
rm cmd/cc-connect/plugin_agent_codex.go
rm cmd/cc-connect/plugin_agent_cursor.go
rm cmd/cc-connect/plugin_agent_devin.go
rm cmd/cc-connect/plugin_agent_gemini.go
rm cmd/cc-connect/plugin_agent_iflow.go
rm cmd/cc-connect/plugin_agent_kimi.go
rm cmd/cc-connect/plugin_agent_opencode.go
rm cmd/cc-connect/plugin_agent_qoder.go
```

### 1.3 Simplify plugin_agent_pi.go

Remove build tag guard — pi is the only agent, no conditional compilation needed:

```go
package main

import _ "github.com/chenhg5/cc-connect/agent/pi"
```

---

## Phase 2: Remove Claude Terminal Observer

The observer subsystem (`core/observer.go`, config, flags in main.go) is Claude-only. Per scope: "no traces of removed agents."

### 2.1 Remove core/observer.go and core/observer_test.go

### 2.2 Remove observe-related config

- `config/config.go:308` — `ObserveConfig` struct.
- `config.example.toml` — observe example block (~line 267).

### 2.3 Remove observe CLI flags and wiring

- `cmd/cc-connect/main.go:142-143` — `--observe` flags.
- `cmd/cc-connect/main.go:322-353` — observe wiring.
- `cmd/cc-connect/main.go:1214-1225` — `resolveClaudeProjectDir`.

### 2.4 Remove engine observe field

- `core/engine.go:254` — `observeProjectDir` field and related usage.

---

## Phase 3: Clean core/ (Engine & Management)

### 3.1 core/progress_compact.go

Remove agent-type dispatch cases for removed agents. Keep the explicit `"pi"` case (line 349-350 returns `"PI"`) — do NOT assume pi falls through to default. Remove cases for: `codex`, `claudecode`/`claude-code`/`cc`, `gemini`, `cursor`, `qoder`, `iflow`, `opencode`.

### 3.2 core/reference_render.go

Remove or empty `supportedReferenceNormalizeAgents` (only lists `codex`, `claudecode`). Delete `core/reference_render_test.go` entirely — it tests removed agent reference normalization.

### 3.3 core/management.go

- Remove `GlobalCodexConfig` field from management config struct.
- Remove provider-parsing branches that map `AppType` → `agent_types` (~lines 1838-1840).
- **Management API `/api/v1/agents` endpoint (~line 221):** Currently returns `{"agents": [...], "platforms": [...]}`. Replace with endpoint that returns `agents: ["pi"]` (static) and platforms (dynamic). Platform discovery must be preserved — frontend uses it for project setup.
- Remove `agent_type` from project PATCH body (~line 714). Remove agent-type validation (~lines 766-776) and mutation path (~line 789). Project agent type is now always `pi`.
- Update `config/config.go` project save path (~line 2841) to not filter providers by agent-type change.

### 3.4 core/session.go

Update comment (~line 689) referencing agent-type switching (`"opencode → pi"`).

### 3.5 core/engine.go

- Remove `observeProjectDir` field (Phase 2 overlap).
- Remove agent-type switching logic in session routing and provider wiring.
- Remove any loop iterating over registered agents to find/switch types.

### 3.6 core/relay.go

Relay binds project/bot names, not agent types. Keep relay intact — it already works for pi-to-pi. Remove only if there are explicit agent-type references (unlikely per audit). Do NOT remove relay infrastructure.

---

## Phase 4: Clean config/config.go (Schema & Defaults)

This is the largest single-file change. All defaults must become pi.

### 4.1 Provider schema flattening

Remove per-agent provider fields (~lines 414-418):
- `AgentTypes` — no longer needed
- Per-agent `Endpoints` — remove
- Per-agent `AgentModels` — remove
- Per-agent `AgentModelLists` — remove
- Codex-specific `Codex` — remove

### 4.2 Remove reference normalization agent list

- `config/config.go:821-822` — `supportedReferenceAgents = ["codex", "claudecode"]`

### 4.3 Simplify provider resolution

- `config/config.go:1135-1155` — `ResolveProviderRefs` / `ResolveForAgent` implement per-agent filtering. Simplify to single pi path.

### 4.4 Fix all defaults to pi

- `config/config.go:1592` — `EnsureProjectWithFeishuOptions.AgentType` default `"codex"` → `"pi"`
- `config/config.go:1692` — new Feishu-created project default `"codex"` → `"pi"`
- `config/config.go:2267` — ACP preset parsing → remove (ACP agent deleted)
- `config/config.go:2280` — default template agent `"codex"` → `"pi"`
- `config/config.go:2301-2316` — `cloneAgentConfig` preserves per-agent overrides → simplify
- `config/config.go:2394` — `patchProjectAgentOption` creates missing agent block with `type = "claudecode"` → `type = "pi"`
- `config/config.go:3066` — `AddPlatformToProject` defaults to `"codex"` → `"pi"`

### 4.5 Note: speech/TTS Gemini/Qwen references

`config/config.go:244,269` — `SpeechConfig.Provider` and `TTSConfig.Provider` reference `gemini` and `qwen` as STT/TTS providers, NOT agent types. These must be preserved.

---

## Phase 5: Clean CLI (cmd/cc-connect/)

### 5.1 Remove cc-switch provider import

`cmd/cc-connect/provider.go` contains cc-switch import logic that is multi-agent:
- Line 212 — `--type` filter says `claude or codex`
- Lines 315, 357-363 — switch supports only `claude` and `codex`
- Line 366 — `convertClaudeProvider`
- Line 397 — `convertCodexProvider`
- Line 420 — `parseCodexConfigTOML`
- Line 508 — web cc-switch provider info

Either remove cc-switch import entirely or redefine for pi providers. If removed, `modernc.org/sqlite` dependency becomes unused.

### 5.2 Remove agent-type from CLI

- Remove any agent-type validation/flags/commands from `main.go`.
- Remove `resolveClaudeProjectDir` (Phase 2 overlap).

### 5.3 Simplify Makefile

```
ALL_AGENTS := pi
```

Remove agent-exclusion build tag logic. Keep platform selection for users who only need specific channels.

---

## Phase 6: Clean config.example.toml

Replace the primary example project `type = "claudecode"` with `type = "pi"`. Delete every project section for other agents:
- ~line 716 — default project `type = "claudecode"` → `pi`
- ~lines 860-875 — Codex/Claude reference normalization config → remove
- ~lines 1203-1690 — Cursor, Gemini, Codex, Qoder, OpenCode, Devin, ACP, iFlow, Kimi, claudecode project sections → delete

Remove `agent_types` from global provider examples (~lines 57-70). Remove observe example block (~line 267).

---

## Phase 7: Clean Web Dashboard

### 7.1 Remove agent selector from project UI

- `web/src/pages/Projects/ProjectList.tsx` — remove agent option list, default to pi.
- `web/src/pages/Projects/ProjectDetail.tsx` — remove agent type selector, agent-type state, `/agents` call, `agent_type` PATCH.

### 7.2 Flatten provider UI

- `web/src/pages/Providers/ProviderList.tsx` — remove per-agent provider checkboxes, Codex-only `wire_api` UI, CC-Switch import UI, app_type badges.

### 7.3 API client cleanup

- `web/src/api/providers.ts` — remove Codex/per-agent provider schema types.
- `web/src/api/projects.ts` — remove `agent_type` fields, `/agents` call. Replace with platform-only discovery.
- `web/src/api/skills.ts` — remove `agent_type`/`agent_types` fields.
- `web/src/api/sessions.ts` — remove `agent_type` field.

### 7.4 i18n cleanup

Remove agent-type switching strings from all locale JSON files in `web/src/i18n/locales/`.

---

## Phase 8: Clean Tests

### 8.1 Delete test files for removed agents

Agent test files are auto-deleted with Phase 1.1.

### 8.2 Delete reference render tests

`core/reference_render_test.go` — entirely Codex/Claude reference normalization tests. Delete.

### 8.3 Fix core tests

- `core/engine_test.go` — remove test cases referencing other agent types. Remove Codex/Claude/ACP simulation cases.
- `core/multi_workspace_test.go` — remove multi-agent workspace tests.
- `core/management_test.go` — remove per-agent provider override tests (~lines 803-840).

### 8.4 Fix integration tests

- `tests/integration/agent_integration_test.go` — delete (imports deleted agent packages, tests multiple agents).
- `tests/integration/filter_sessions_test.go` — remove Codex/Claude fixture creation.
- `tests/e2e/smoke_test.go` — remove removed agent lists.

### 8.5 Fix release_local tests

`tests/release_local/config_matrix/config_matrix_test.go` — remove `claudecode` references.

### 8.6 Platform test labels — decision needed

`platform/discord/discord_test.go` and `platform/feishu/platform_test.go` use "Codex" as a display label in test assertions. Per scope: "all platforms — these must not be touched." If these are just generic test labels (not agent-type-specific), scope allows fixing them. If touching platform tests is considered a platform change, leave them. **Decision: fix them — they are agent-type references, not platform code.**

---

## Phase 9: Clean Repository Metadata

### 9.1 .github issue templates

- `.github/ISSUE_TEMPLATE/bug_report.yml` — remove Agent Type dropdown (Claude/Codex/Cursor/Gemini).
- `.github/ISSUE_TEMPLATE/feature_request.yml` — remove agent reference.
- `.github/ISSUE_TEMPLATE/platform_agent_request.yml` — remove or update to platform-only.

### 9.2 npm package

- `npm/README.md` — remove references to Claude Code, Cursor, Gemini CLI, Codex.
- `npm/package.json` — update description/keywords.

### 9.3 .gitignore

Remove `.codex` entry.

### 9.4 Root docs

- `README.md` — update to pi-only.
- `CHANGELOG.md` — note the strip-down.
- `CLAUDE.md`, `AGENTS.md` — update if they reference other agents.
- `INSTALL.md`, `CONTRIBUTING.md` — update agent references.

---

## Phase 10: Clean provider-presets.json

The file contains no `pi` agent entries. Pi does not use provider presets (it gets provider config through its own infrastructure). **Decision: remove the file entirely.** If backend code references it, make it gracefully handle absence.

Remove `skill-presets.json` if it references other agent types.

---

## Phase 11: go mod tidy

After all deletions:
```bash
go mod tidy
```

Expected stale deps to drop:
- `github.com/creack/pty` — only used by iflow + claudecode (deleted)
- `modernc.org/sqlite` — only used by cc-switch import (removed in Phase 5.1)

Verify build still succeeds.

---

## Phase 12: Verify

### 12.1 Build

```bash
make build
```

Must succeed with only pi agent compiled in.

### 12.2 Tests

```bash
make test-fast
```

All unit and smoke tests must pass.

### 12.3 Audit grep — all file types

```bash
rg -i "acp|claudecode|claude.code|codex|cursor|devin|gemini-cli|iflow|kimi|opencode|qoder" --type-not binary
```

Must return zero hits except:
- Non-agent uses of `gemini` (STT provider in `core/speech.go`) — verify each hit is STT, not agent
- Non-agent uses of `qwen` (TTS provider) — verify each hit is TTS, not agent
- Historical changelog entries
- Any other whitelisted non-agent reference

### 12.4 Platforms untouched

```bash
git diff --stat main -- platform/
```

Must be empty.

### 12.5 Pi agent untouched

```bash
git diff --stat main -- agent/pi/
```

Must be empty.

---

## Execution Order

1. Phase 1: Bulk deletions
2. Phase 2: Remove observer
3. Phase 8: Delete broken test files (so we can iterate on build)
4. Phase 3-7: Clean core, config, CLI, config.example.toml, web (can be parallelized)
5. Phase 9: Clean repo metadata
6. Phase 10: Remove provider-presets.json
7. Phase 11: go mod tidy
8. Phase 12: Verify
9. Single squashed commit
