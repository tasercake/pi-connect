# Slack typed session keys + per-thread sessions

## Objective

Implement Slack session key construction with explicit typed key formats controlled by both existing/new flags:

| `share_session_in_channel` | `session_per_thread` | Session key |
| --- | --- | --- |
| `false` | `false` | `slack:u:<channel>:<user>` |
| `true` | `false` | `slack:c:<channel>` |
| `false` | `true` | `slack:ut:<channel>:<user>:<thread_ts>` |
| `true` | `true` | `slack:t:<channel>:<thread_ts>` |

Only these four Slack key formats are supported after this change. Do **not** preserve old `slack:<channel>` / `slack:<channel>:<user>` support.

## Design decisions

- Use explicit typed tags (`c`, `u`, `t`, `ut`) everywhere for Slack session keys.
- Add Slack platform option `session_per_thread` with default `false`.
- For `session_per_thread=true`, thread key selection:
  - If the Slack event has `thread_ts`, use it.
  - Otherwise use the event `ts` as the root thread timestamp.
  - This applies to app mentions, channel messages, assistant-thread messages, and DMs.
- Slash commands do not expose `thread_ts` in slack-go `SlashCommand`; they must use non-thread typed keys even when `session_per_thread=true`:
  - shared: `slack:c:<channel>`
  - non-shared: `slack:u:<channel>:<user>`
- Do not add `message.channels` / `message.groups` ingestion/filtering in this implementation. Channel thread messages are handled only when Slack sends currently subscribed events, e.g. app mentions. Docs should not claim mention-once continuation unless event/filtering is later added.

## Checklist

### Slack platform implementation

- [ ] In `platform/slack/slack.go`, add `sessionPerThread bool` to `Platform`.
- [ ] Parse `opts["session_per_thread"]` in `New()`.
- [ ] Add centralized helpers for Slack session keys:
  - [ ] build event keys from channel/user/event ts/thread ts using the four-format matrix.
  - [ ] build slash-command keys using only `c`/`u` formats.
  - [ ] choose root thread timestamp (`thread_ts` if present, else `ts`).
  - [ ] parse typed session keys for channel/user/thread components.
- [ ] Replace duplicated session-key `fmt.Sprintf` blocks in AppMentionEvent, MessageEvent, and SlashCommand handling.
- [ ] Set `Message.ChannelKey = ev.Channel` for AppMentionEvent and MessageEvent so live workspace routing is channel-scoped independent of session key shape.
- [ ] Update `ReconstructReplyCtx()` to accept only typed Slack keys:
  - [ ] `slack:c:<channel>` -> channel only.
  - [ ] `slack:u:<channel>:<user>` -> channel only.
  - [ ] `slack:t:<channel>:<thread_ts>` -> channel + thread timestamp.
  - [ ] `slack:ut:<channel>:<user>:<thread_ts>` -> channel + thread timestamp.
  - [ ] malformed or old untyped keys return errors.

### Core parser / workspace implications

- [ ] In `core/engine.go`, replace the current single-character tag heuristic in `extractChannelID()` / `extractUserID()` with explicit typed session key parsing.
- [ ] Ensure Slack typed keys resolve workspace channel key as `slack:<channel>` for all four formats.
- [ ] Ensure `extractUserID("slack:t:<channel>:<thread>") == ""` and `extractUserID("slack:c:<channel>") == ""`.
- [ ] Avoid Slack-specific one-off branches if possible; implement a small generic typed-tag helper for `c`, `u`, `t`, `ut`.

### Tests

- [ ] Add/extend `platform/slack/slack_test.go` tests:
  - [ ] key builder covers all four matrix entries.
  - [ ] thread timestamp source uses `thread_ts` when present, else `ts`.
  - [ ] slash key builder falls back to `c`/`u` even when `session_per_thread=true`.
  - [ ] `ReconstructReplyCtx()` succeeds for all four typed formats.
  - [ ] `ReconstructReplyCtx()` rejects old untyped and malformed Slack keys.
- [ ] Add/extend `core/engine_test.go` tests:
  - [ ] `extractChannelID`, `extractUserID`, and `extractWorkspaceChannelKey` for all four Slack typed formats.
  - [ ] old untyped Slack examples are no longer treated as valid typed Slack forms if covered by tests.

### Config, docs, UI

- [ ] Update `config.example.toml` Slack section with `session_per_thread = false` and typed key semantics if appropriate.
- [ ] Update `docs/slack.md` with `session_per_thread` and four key formats.
- [ ] Update `docs/slack-feature-inventory.md` key-format/config references.
- [ ] Update `docs/slack-app-manifest.json` only if necessary. Do not add `message.channels`/`message.groups` unless also implementing safe filtering.
- [ ] Update management API docs examples that show old Slack key format:
  - [ ] `docs/management-api.md`
  - [ ] `docs/management-api.zh-CN.md`
- [ ] Update `web/src/lib/platformMeta.ts` to expose `session_per_thread` for Slack.

### Verification

- [x] Run `go test ./platform/slack ./core`.
- [x] Build embedded web assets first with `make web`, then run plain `go test ./...`.
- [x] Check `git diff` for accidental unrelated edits.
