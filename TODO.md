# TODO: Strip cc-connect Down to Pi-Only Agent

**Scope:** `SCOPE-STRIP-DOWN-TO-PI.md` (immutable)  
**Plan:** `PLAN-STRIP-DOWN-TO-PI.md` (immutable)  

---

## Batch A: Bulk Deletions (Phase 1)

- [ ] **A1.** Delete 10 agent source directories (`agent/acp`, `agent/claudecode`, `agent/codex`, `agent/cursor`, `agent/devin`, `agent/gemini`, `agent/iflow`, `agent/kimi`, `agent/opencode`, `agent/qoder`)
- [ ] **A2.** Delete 10 agent plugin import files (`cmd/cc-connect/plugin_agent_*.go` except `plugin_agent_pi.go`)
- [ ] **A3.** Simplify `cmd/cc-connect/plugin_agent_pi.go` — remove build tag guard

## Batch B: Observer + Broken Tests (Phases 2 + 8)

- [ ] **B1.** Remove Claude terminal observer: delete `core/observer.go`, `core/observer_test.go`, observe config in `config/config.go`, observe flags in `cmd/cc-connect/main.go`, engine observe field, `config.example.toml` observe block
- [ ] **B2.** Fix core tests: remove agent-specific test cases from `core/engine_test.go`, `core/multi_workspace_test.go`, `core/management_test.go`; delete `core/reference_render_test.go`
- [ ] **B3.** Fix integration/e2e tests: delete `tests/integration/agent_integration_test.go`, fix `tests/integration/filter_sessions_test.go`, fix `tests/e2e/smoke_test.go`, fix `tests/release_local/config_matrix/config_matrix_test.go`
- [ ] **B4.** Fix platform test labels: `platform/discord/discord_test.go` and `platform/feishu/platform_test.go` — remove "Codex" display labels

## Batch C: Core Engine Cleanup (Phase 3)

- [ ] **C1.** `core/progress_compact.go` — remove agent dispatch cases except `pi`
- [ ] **C2.** `core/reference_render.go` — remove/empty `supportedReferenceNormalizeAgents`
- [ ] **C3.** `core/management.go` — remove `GlobalCodexConfig`, fix `/api/v1/agents` endpoint (platforms dynamic, agents static `["pi"]`), remove `agent_type` from PATCH, remove agent-type validation
- [ ] **C4.** `core/session.go` — update agent-switching comment
- [ ] **C5.** `core/engine.go` — remove agent-type switching logic
- [ ] **C6.** Verify `core/relay.go` is untouched (no agent-type references found)

## Batch D: Config & CLI (Phases 4 + 5)

- [ ] **D1.** `config/config.go` — flatten provider schema (remove AgentTypes, per-agent Endpoints/AgentModels/AgentModelLists, Codex), fix all defaults to `pi`, remove `supportedReferenceAgents`, simplify `ResolveProviderRefs`/`ResolveForAgent`
- [ ] **D2.** `cmd/cc-connect/provider.go` — remove cc-switch import (claude/codex convert functions, cc-switch UI, SQLite import)
- [ ] **D3.** `cmd/cc-connect/main.go` — remove agent-type validation/flags; remove `resolveClaudeProjectDir` (observer overlap)
- [ ] **D4.** `Makefile` — `ALL_AGENTS := pi`, remove agent-exclusion build tag logic

## Batch E: Config File & Web Dashboard (Phases 6 + 7)

- [ ] **E1.** `config.example.toml` — default project to `pi`, delete non-pi project sections, remove reference normalization config, remove `agent_types` from providers
- [ ] **E2.** Web: remove agent selector from project UI (`ProjectList.tsx`, `ProjectDetail.tsx`)
- [ ] **E3.** Web: flatten provider UI (`ProviderList.tsx` — per-agent checkboxes, wire_api, cc-switch import, app_type badges)
- [ ] **E4.** Web: clean API clients (`providers.ts`, `projects.ts`, `skills.ts`, `sessions.ts`)
- [ ] **E5.** Web: clean i18n locale JSON files

## Batch F: Metadata & Presets (Phases 9 + 10)

- [ ] **F1.** `.github/ISSUE_TEMPLATE/` — remove agent-type dropdowns and references
- [ ] **F2.** `npm/README.md` and `npm/package.json` — remove agent references
- [ ] **F3.** `.gitignore` — remove `.codex` entry
- [ ] **F4.** Root docs — update `README.md`, `CHANGELOG.md`, `CLAUDE.md`, `AGENTS.md`, `INSTALL.md`, `CONTRIBUTING.md`
- [ ] **F5.** Delete `provider-presets.json` and `skill-presets.json` (if agent-specific)

## Batch G: Finalize (Phases 11 + 12)

- [ ] **G1.** `go mod tidy` — drop stale deps (pty, sqlite)
- [ ] **G2.** `make build` — must succeed
- [ ] **G3.** `make test-fast` — all tests pass
- [ ] **G4.** Audit grep — zero hits for removed agents (whitelist: STT/TTS gemini/qwen, changelog)
- [ ] **G5.** Verify platforms untouched: `git diff --stat main -- platform/`
- [ ] **G6.** Verify pi agent untouched: `git diff --stat main -- agent/pi/`

## Batch H: PR & Review

- [ ] **H1.** Commit all changes to `strip-down-to-pi` branch
- [ ] **H2.** Open PR via GitHub
- [ ] **H3.** Reviewer subagent leaves comments on PR
