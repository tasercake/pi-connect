# CC-Connect Development Guide

## Project Overview

CC-Connect is a bridge that connects the Pi coding agent with messaging platforms (Feishu/Lark, Telegram, Discord, Slack, DingTalk, WeChat Work, QQ, LINE). Users interact with Pi through their preferred messaging app.

## Architecture

```
┌─────────────────────────────────────────────────┐
│                   cmd/pi-connect                │  ← entry point, CLI, daemon
├─────────────────────────────────────────────────┤
│                     config/                     │  ← TOML config parsing
├─────────────────────────────────────────────────┤
│                      core/                      │  ← engine, interfaces, i18n,
│                                                 │     cards, sessions, registry
├──────────────────────┬──────────────────────────┤
│      agent/          │      platform/           │
│  └── pi/             │  ├── feishu/             │
│                      │  ├── telegram/           │
│                      │  ├── discord/            │
│                      │  ├── slack/              │
│                      │  ├── dingtalk/           │
│                      │  ├── wecom/              │
│                      │  ├── qq/                 │
│                      │  ├── qqbot/              │
│                      │  ├── line/               │
│                      │  └── weibo/              │
├──────────────────────┴──────────────────────────┤
│                     daemon/                     │  ← systemd/launchd service
└─────────────────────────────────────────────────┘
```

### Key Design Principles

**`core/` is the nucleus.** It defines all interfaces (`Platform`, `Agent`, `AgentSession`, etc.) and contains the `Engine` that orchestrates message flow. The core package must **never** import from `agent/` or `platform/`.

**Plugin architecture via registries.** Pi and platforms register themselves through `core.RegisterAgent()` and `core.RegisterPlatform()` in their `init()` functions. The engine creates instances via `core.CreateAgent()` / `core.CreatePlatform()` using names from config.

**Dependency direction:**
```
cmd/ → config/, core/, agent/pi, platform/*
agent/pi   → core/
platform/* → core/  (never other platforms or agents)
core/      → stdlib only (never agent/ or platform/)
```

### Core Interfaces

- **`Platform`** — messaging platform adapter (Start, Reply, Send, Stop)
- **`Agent`** — Pi agent adapter (StartSession, ListSessions, Stop)
- **`AgentSession`** — a running bidirectional session (Send, RespondPermission, Events)
- **`Engine`** — the central orchestrator that routes messages between platforms and Pi

Optional capability interfaces (implement only when needed):
- `CardSender` — rich card messages
- `InlineButtonSender` — inline keyboard buttons
- `ProviderSwitcher` — multi-model switching
- `DoctorChecker` — agent-specific health checks
- `AgentDoctorInfo` — CLI binary metadata for diagnostics

## Development Rules

### 1. No Hardcoding Platform Names in Core

The `core/` package must remain platform-agnostic. Never write `if p.Name() == "feishu"`; use interfaces and capability checks instead.

### 2. Prefer Interfaces Over Type Switches

When behavior differs across platforms or optional capabilities, define an optional interface in core and let implementations opt in.

### 3. Configuration Over Code

- Features that may vary per deployment should be configurable in `config.toml`
- Use `map[string]any` options for agent/platform factories to stay flexible
- Add new config fields with sensible defaults so existing configs don't break

### 4. High Cohesion, Low Coupling

- `agent/pi/` handles Pi process lifecycle, output parsing, and session management
- Each `platform/X/` package is self-contained: it handles API connection, message receiving/sending, and card rendering for platform X
- Cross-cutting concerns (i18n, cards, streaming, rate limiting) live in `core/`

### 5. Error Handling

- Always wrap errors with context: `fmt.Errorf("feishu: reply card: %w", err)`
- Never silently swallow errors; at minimum log them with `slog.Error` / `slog.Warn`
- Use `slog` (structured logging) consistently; never `log.Printf` or `fmt.Printf` for runtime logs
- Redact tokens/secrets in error messages using `core.RedactToken()`

### 6. Concurrency Safety

- Agent sessions are accessed from multiple goroutines; protect shared state with `sync.Mutex` or `atomic` types
- Use `context.Context` for cancellation propagation
- Channels should have clear ownership; document who closes them
- Prefer `sync.Once` for one-time teardown (`pendingPermission.resolve()`)

### 7. i18n

All user-facing strings must go through `core/i18n.go`:
- Define a `MsgKey` constant
- Add translations for all supported languages (EN, ZH, ZH-TW, JA, ES)
- Use `e.i18n.T(MsgKey)` or `e.i18n.Tf(MsgKey, args...)`

## Code Style

- Follow standard Go conventions (`gofmt`, `go vet`)
- Use `strings.EqualFold` for case-insensitive comparisons
- Avoid `init()` for anything other than platform/agent registration
- Keep functions focused; extract helpers when a function exceeds ~80 lines
- Naming: `New()` for constructors, `Get/Set` for accessors, avoid stuttering (`feishu.FeishuPlatform` → `feishu.Platform`)

## Testing

### Requirements

- All new features must include unit tests
- All bug fixes should include a regression test
- Tests must pass before committing: `go test ./...`

### Running Tests

```bash
# Full test suite
go test ./...

# Specific package
go test ./core/ -v

# Run specific test
go test ./core/ -run TestHandlePendingPermission -v

# With race detector (CI)
go test -race ./...
```

### Test Patterns

- Use stub types for `Platform` and `Agent` in core tests (see `core/engine_test.go`)
- Test card rendering by inspecting the returned `*Card` struct, not JSON
- For agent session tests, simulate event streams via channels

## Selective Compilation

Pi is always compiled in. Platforms are imported via separate `plugin_platform_*.go` files with build tags (e.g. `//go:build !no_feishu`). By default all platforms are compiled in.

### Include only specific platforms

```bash
make build PLATFORMS_INCLUDE=feishu,telegram
```

### Exclude specific platforms

```bash
make build EXCLUDE=discord,dingtalk,qq,qqbot,line
```

### Direct build tag usage (without Make)

```bash
go build -tags 'no_discord no_dingtalk no_qq no_qqbot no_line' ./cmd/pi-connect
```

## Pre-Commit Checklist

1. **Build passes**: `go build ./...`
2. **Tests pass**: `go test ./...`
3. **No new hardcoded platform names in core**: grep for platform names in `core/*.go`
4. **i18n complete**: all new user-facing strings have translations for all languages
5. **No secrets in code**: no API keys, tokens, or credentials in source files

## Adding a New Platform

1. Create `platform/newplatform/newplatform.go`
2. Implement `core.Platform` interface (and optional interfaces as needed)
3. Register in `init()`: `core.RegisterPlatform("newplatform", factory)`
4. Create `cmd/pi-connect/plugin_platform_newplatform.go` with `//go:build !no_newplatform` tag
5. Add `newplatform` to `ALL_PLATFORMS` in `Makefile`
6. Add config example in `config.example.toml`
7. Add unit tests
