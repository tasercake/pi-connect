# Scope: Strip cc-connect Fork Down to Pi-Only Agent

**Branch:** `strip-down-to-pi`  
**Worktree:** `/home/exedev/src/cc-connect-pi`  

## Goal

Strip the cc-connect fork down to support **only the `pi` agent**. Remove all other agent integrations.

## What We're Removing

10 agent types: acp, claudecode, codex, cursor, devin, gemini, iflow, kimi, opencode, qoder.

Also any related configuration (including agent selector(s)) and routing logic and tests.

## What We're Keeping

- **Pi agent infrastructure**
- **All platforms** — Telegram, Slack, Discord, etc. – these *must not be touched*.
- **All infrastructure** — cron, webhooks, TTS/speech, web dashboard, session management, relay (scoped to pi-only).

## Success Criteria

- Pi agent works across all 12 platforms.
- Build, tests, and existing platform integrations continue to function.
- No traces of removed agents in code, config, docs, or UI.
- All pi agent code, pi lifecycle management, pi tests, pi-related docs (if any), etc. remain unchanged
