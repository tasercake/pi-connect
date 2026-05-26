<p align="center">
  <img src="./docs/images/banner.svg" alt="CC-Connect Banner" width="800"/>
</p>

<p align="center">
  <a href="https://github.com/chenhg5/cc-connect/actions/workflows/ci.yml">
    <img src="https://github.com/chenhg5/cc-connect/actions/workflows/ci.yml/badge.svg" alt="CI Status"/>
  </a>
  <a href="https://github.com/chenhg5/cc-connect/releases">
    <img src="https://img.shields.io/github/v/release/chenhg5/cc-connect?include_prereleases" alt="Release"/>
  </a>
  <a href="https://www.npmjs.com/package/cc-connect">
    <img src="https://img.shields.io/npm/dm/cc-connect?logo=npm" alt="npm downloads"/>
  </a>
  <a href="https://github.com/chenhg5/cc-connect/blob/main/LICENSE">
    <img src="https://img.shields.io/badge/License-MIT-yellow.svg" alt="License"/>
  </a>
  <a href="https://goreportcard.com/report/github.com/chenhg5/cc-connect">
    <img src="https://goreportcard.com/badge/github.com/chenhg5/cc-connect" alt="Go Report Card"/>
  </a>
</p>

<p align="center">
  <a href="https://discord.gg/kHpwgaM4kq">
    <img src="https://img.shields.io/badge/Discord-Join-5865F2?logo=discord&logoColor=white" alt="Discord"/>
  </a>
  <a href="https://t.me/+odGNDhCjbjdmMmZl">
    <img src="https://img.shields.io/badge/Telegram-Group-26A5E4?logo=telegram&logoColor=white" alt="Telegram"/>
  </a>
</p>

<p align="center">
  <a href="./README.md">English</a> | <a href="./README.zh-CN.md">中文</a>
</p>

<p align="center">
  <a href="https://trendshift.io/repositories/23266" target="_blank">
    <img src="https://trendshift.io/api/badge/repositories/23266" alt="chenhg5/cc-connect | Trendshift" style="width: 250px; height: 55px;" width="250" height="55"/>
  </a>
</p>


## ❤️ Sponsor

> Want to appear here? Contact: chg80333@gmail.com | WeChat: mongorz

<details open>
<summary>Sponsors</summary>

Sponsor information has been simplified for this Pi-only fork.

</details>

---

<br>

<p align="center">
  <b>Control Pi from any chat app. Anywhere, anytime.</b>
</p>

<p align="center">
  cc-connect bridges Pi running on your machine to the messaging platforms you already use.<br/>
  Code review, research, automation, data analysis — anything Pi can do,<br/>
  now accessible from your phone, tablet, or any device with a chat app.
</p>

<p align="center">
  <img src="docs/images/connector.png" alt="CC-Connect Architecture" width="90%"/>
</p>


## 🆕 What’s New in v1.3.0

- **🌐 Web Admin UI (Recommended)** — Full management dashboard embedded in the binary — **no extra dependencies**. Create and edit projects, manage providers, monitor sessions, edit cron jobs, and **chat with your agent directly from the browser**. Supports 5 languages (en/zh/zh-TW/ja/es). We recommend managing cc-connect through the web UI instead of editing `config.toml` by hand. Run `cc-connect web` to configure and open the dashboard, then run `cc-connect` to start the service.
- **Lifecycle Event Hooks** — New `[[hooks]]` config triggers shell commands or HTTP webhooks on message, session, cron, permission, and error events. Async by default, fail-open.
- **Skill Management** — New `/skills` page with local skill browser and recommended presets.
- **Global Provider Management** — Add/edit/delete providers in the web UI.
- **Personal WeChat** — Chat with your local agent from **Weixin (personal)** via ilink long-polling; QR `weixin setup`, CDN media, no public IP. *[Setup → `docs/weixin.md`](docs/weixin.md)*
- **Weibo DM** — Chat with your agent via **Weibo private messages** over WebSocket; no public IP needed, text streaming supported.
- **Feishu Enhancements** — Auto-resolve `@name` mentions, multi-level reply chain recognition, done-emoji reactions.
- **Pi-only fork** — This fork focuses on Pi as the only built-in agent.


## 🧩 Platform feature snapshot

High-level view of what each **built-in platform** can do in cc-connect.

**Legend**

| Symbol | Meaning |
|--------|---------|
| ✅ | Works in **stable** cc-connect with typical configuration |
| ⚠️ | Partial, needs extra config (e.g. speech / ASR), or limited by the vendor app or API |
| ❌ | Not supported or not applicable in practice |

† **QQ (NapCat / OneBot)** — unofficial self-hosted bridge; behaviour depends on your NapCat / network setup.

| Capability | Feishu | DingTalk | Telegram | Slack | Discord | LINE | WeCom | Weibo | **Weixin**<br>*(personal)* | QQ† | QQ Bot |
|------------|:------:|:--------:|:--------:|:-----:|:-------:|:----:|:-----:|:-----:|:-------------------------:|:---:|:------:|
| Text & slash commands | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| Markdown / cards | ✅ | ✅ | ✅ | ✅ | ✅ | ⚠️ | ⚠️ | ❌ | ✅ | ✅ | ✅ |
| Streaming / chunked replies | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| Images & files | ✅ | ✅ | ✅ | ✅ | ✅ | ⚠️ | ✅ | ❌ | ✅ | ✅ | ✅ |
| Voice / STT / TTS | ⚠️ | ⚠️ | ✅ | ⚠️ | ⚠️ | ❌ | ⚠️ | ❌ | ✅ | ⚠️ | ⚠️ |
| Private (DM) | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| Group / channel | ✅ | ✅ | ✅ | ✅ | ✅ | ⚠️ | ✅ | ❌ | ✅ | ✅ | ✅ |

> **WeCom:** Webhook mode needs a **public URL**; long-connection / WS style setups often do not.  
> **Voice row:** many platforms need `[speech]` / TTS providers enabled in `config.toml`; values are a best-effort summary.  
> Per-platform setup: [Platform setup guides](#-platform-setup-guides) below.


## ✨ Why cc-connect?

### 🤖 Pi Agent Support
**Pi-only agent bridge** — This fork focuses on Pi and keeps the platform bridge simple.

### 📱 Platform Flexibility
**11 Chat Platforms** — Feishu, DingTalk, Slack, Telegram, Discord, WeChat Work, Weibo, LINE, QQ, QQ Bot (Official), plus **Weixin (personal ilink)** for **personal WeChat**. Most platforms need **zero public IP**.

### 🔄 Multi-Bot Relay
**Multi-Bot Relay** — Bind multiple Pi-backed bots in a group chat and let them communicate with each other in one conversation.

### 🎮 Complete Chat Control
**Full Control from Chat** — Switch models (`/model`), tune reasoning (`/reasoning`), change permission modes (`/mode`), manage sessions, all via slash commands.

**Directory Switching in Chat** — Change where the next session starts with `/dir <path>` (and `/cd <path>` as a compatibility alias), plus quick history jump via `/dir <number>` / `/dir -`.

### 🧠 Persistent Memory
**Agent Memory** — Read and write agent instruction files (`/memory`) without touching the terminal.

### ⏰ Intelligent Scheduling
**Scheduled Tasks** — Set up cron jobs in natural language. *"Every day at 6am, summarize GitHub trending"* just works.

### 🎤 Multimodal Support
**Voice & Images** — Send voice messages or screenshots; cc-connect handles STT/TTS and multimodal forwarding.

### 📦 Multi-Project Architecture
**Multi-Project** — One process, multiple projects, each with its own agent + platform combo.

### 🌍 Multilingual Interface
**5 Languages** — Native support for English, Chinese (Simplified & Traditional), Japanese, and Spanish. Built-in i18n ensures everyone feels at home.


<p align="center">
  <img src="docs/images/screenshot/cc-connect-lark.JPG" alt="飞书" width="32%" />
  <img src="docs/images/screenshot/cc-connect-telegram.JPG" alt="Telegram" width="32%" />
  <img src="docs/images/screenshot/cc-connect-wechat.JPG" alt="微信" width="32%" />
</p>
<p align="center">
  <em>Left：Lark &nbsp;|&nbsp; Telegram &nbsp;|&nbsp; Right：Wechat</em>
</p>


## 🚀 Quick Start

### 🤖 Install & Configure with Pi (Recommended)

> **The easiest way** — Ask Pi to follow this installation guide and help configure cc-connect:

```bash
Follow https://raw.githubusercontent.com/chenhg5/cc-connect/refs/heads/main/INSTALL.md to install and configure cc-connect.
```


### 📦 Manual Install

**Via npm:**

```bash
npm install -g cc-connect
```

**Via Homebrew (macOS / Linux):**

```bash
brew install cc-connect
```

**Download binary from [GitHub Releases](https://github.com/chenhg5/cc-connect/releases):**

```bash
# Linux amd64 - Stable
curl -L -o cc-connect https://github.com/chenhg5/cc-connect/releases/latest/download/cc-connect-linux-amd64
chmod +x cc-connect
sudo mv cc-connect /usr/local/bin/

```

**Build from source (requires Go 1.22+):**

```bash
git clone https://github.com/chenhg5/cc-connect.git
cd cc-connect
make build
```


### ⚙️ Configure

> **💡 Tip: Use the Web UI to configure** — After installing, run `cc-connect web` to configure the web admin and open the dashboard in your browser. You can visually create projects, add platforms, manage providers, and chat with your agent — no need to manually edit TOML files. **Note:** `cc-connect web` only configures and opens the browser — you still need to run `cc-connect` separately to start the service.

If you prefer manual configuration:

```bash
mkdir -p ~/.cc-connect
cp config.example.toml ~/.cc-connect/config.toml
vim ~/.cc-connect/config.toml
```

Set `admin_from = "alice,bob"` in a project to allow those user IDs to run privileged commands such as `/dir` and `/shell`.
When a user runs `/dir reset`, cc-connect restores the configured `work_dir` and clears the persisted override stored under `data_dir/projects/<project>.state.json`.


### ▶️ Run

```bash
./cc-connect
```


### 🔄 Upgrade

```bash
# npm
npm install -g cc-connect

# Homebrew
brew upgrade cc-connect

# Binary self-update
cc-connect update           # Stable
cc-connect update --pre     # Include pre-releases
```


## 📊 Support Matrix

| Component | Type | Status |
|-----------|------|--------|
| Agent | Pi | ✅ Supported |
| Platform | Feishu (Lark) | ✅ WebSocket — no public IP needed |
| Platform | DingTalk | ✅ Stream — no public IP needed |
| Platform | Telegram | ✅ Long Polling — no public IP needed |
| Platform | Slack | ✅ Socket Mode — no public IP needed |
| Platform | Discord | ✅ Gateway — no public IP needed |
| Platform | Weibo | ✅ WebSocket — no public IP needed |
| Platform | LINE | ✅ Webhook — public URL required |
| Platform | WeChat Work | ✅ WebSocket / Webhook |
| Platform | Weixin (personal, ilink) | ✅— HTTP long polling — no public IP needed |
| Platform | QQ (NapCat/OneBot) | ✅ WebSocket |
| Platform | QQ Bot (Official) | ✅ WebSocket — no public IP needed |


## 📖 Platform Setup Guides

| Platform | Guide | Connection | Public IP? |
|----------|-------|------------|------------|
| Feishu (Lark) | [docs/feishu.md](docs/feishu.md) | WebSocket | No |
| DingTalk | [docs/dingtalk.md](docs/dingtalk.md) | Stream | No |
| Telegram | [docs/telegram.md](docs/telegram.md) | Long Polling | No |
| Slack | [docs/slack.md](docs/slack.md) | Socket Mode | No |
| Discord | [docs/discord.md](docs/discord.md) | Gateway | No |
| Weibo | [docs/weibo.md](docs/weibo.md) | WebSocket | No |
| WeChat Work | [docs/wecom.md](docs/wecom.md) | WebSocket / Webhook | No (WS) / Yes (Webhook) |
| Weixin (personal) | [docs/weixin.md](docs/weixin.md) | HTTP long polling (ilink) | No |
| QQ / QQ Bot | [docs/qq.md](docs/qq.md) | WebSocket | No |


## 🎯 Key Features

### 💬 Session Management

```
/new [name]       Start a new session
/list             List all sessions
/switch <id>      Switch session
/current          Show current session
/dir [path|reset] Show, switch, or reset work directory
```

Project configs rotate to a fresh session automatically after long inactivity. This prevents "context drift" where stale chat history (failed commands, debugging noise) is repeatedly re-ingested via `--continue` and starts to dominate the model's attention. The previous session is preserved and remains accessible via `/list` and `/switch`.

```toml
[[projects]]
reset_on_idle_mins = 30   # default when unset; set to 0 to disable
```

The default is **30 minutes** when unset. Set `reset_on_idle_mins = 0` to opt out and always continue the previous session.

### 🛡️ OS-User Isolation (`run_as_user`)

On Linux/macOS, a project can spawn Pi under a different Unix
user for OS-level file-system isolation from the supervisor user that
runs cc-connect.

```toml
[[projects]]
name = "pi-sandboxed"
run_as_user = "partseeker-coder"
run_as_env = ["PGSSLROOTCERT"]
```

The target user needs passwordless sudo from the supervisor, no sudo
of its own, read+write on `work_dir`, and its own Pi credentials.
See [`docs/usage.md`](./docs/usage.md#running-agents-as-a-different-unix-user-run_as_user)
for the full setup.

Before starting cc-connect, audit the setup with:

```bash
cc-connect doctor user-isolation
```

This runs three go/no-go preflight gates and an isolation probe that
reports what the target user can and cannot read. cc-connect refuses to
start if any gate fails or if the probe detects a cross-user leak.

---

### 🔐 Permission Modes

```
/mode             Show available modes
/mode yolo        # Auto-approve all tools
/mode default     # Ask for each tool
```


### 🔄 Provider Management

```
/provider list              List providers
/provider switch <name>     Switch API provider at runtime
```


### 🤖 Model Selection

```
/model                      List available models (format: alias - model)
/model switch <alias>       Switch to model by alias
```


### 📂 Work Directory

```
/dir                         Show current work directory and history
/dir <path>                  Switch to a path (relative or absolute)
/dir <number>                Switch from history
/dir -                       Switch to previous directory
/cd <path>                   Compatibility alias for /dir <path>
```


### ⏰ Scheduled Tasks

```bash
/cron add 0 6 * * * Summarize GitHub trending
```

### 📎 Agent Attachment Send-Back

When an agent generates a local screenshot, chart, PDF, bundle, or other file, it can send that attachment back to the current chat.

First release supports:
- Feishu
- Telegram

If your agent does not natively inject the system prompt, run this once in chat after upgrading:

```text
/bind setup
```

or:

```text
/cron setup
```

This refreshes the cc-connect instructions in the project memory file so the agent knows how to send attachments back.

You can control this feature globally in `config.toml`:

```toml
attachment_send = "on"  # default: "on"; set to "off" to block image/file send-back
```

This switch is independent from the agent's `/mode`. It only controls `cc-connect send --image/--file`.

Examples:

```bash
cc-connect send --image /absolute/path/to/chart.png
cc-connect send --file /absolute/path/to/report.pdf
cc-connect send --file /absolute/path/to/report.pdf --image /absolute/path/to/chart.png
```

Notes:
- Absolute paths are the safest option.
- `--image` and `--file` can both be repeated.
- `attachment_send = "off"` disables only attachment send-back; ordinary text replies still work.
- This command is for generated attachments, not ordinary text replies.

📖 **Full documentation:** [docs/usage.md](docs/usage.md)


## 📚 Documentation

- [Usage Guide](docs/usage.md) — Complete feature documentation
- [INSTALL.md](INSTALL.md) — Pi-focused installation guide
- [config.example.toml](config.example.toml) — Configuration template
- [CONTRIBUTING.md](CONTRIBUTING.md) — How to report issues and contribute pull requests


## 👥 Community

- [Discord](https://discord.gg/kHpwgaM4kq)
- [Telegram](https://t.me/+odGNDhCjbjdmMmZl)


## ☕ Support the Project

If cc-connect has been helpful to you, consider buying us a coffee! Your support helps us:

- 🛠️ Maintain and improve the project
- 📚 Write better documentation and tutorials
- 🐛 Fix bugs and add new features faster
- ☕ Keep the developers caffeinated

### How to Donate

**Buy Me a Coffee**: [https://buymeacoffee.com/cg33](https://buymeacoffee.com/cg33)

**WeChat Pay / Alipay**:

| WeChat Pay | Alipay |
|:----------:|:------:|
| <img src="docs/images/wechatpay.jpg" alt="WeChat Pay" width="150"> | <img src="docs/images/alipay.jpg" alt="Alipay" width="150"> |

### Thank You, Donors! 🎉

We're grateful to everyone who has supported this project. Leave your GitHub username in the donation message if you'd like to be recognized here!

<!-- Donors will be listed below -->
| Avatar | GitHub Username | Date |
|--------|-----------------|------|
| <img src="https://avatars.githubusercontent.com/u/1762560?v=4" width="40" height="40" style="border-radius: 50%;"> | [@thx0701](https://github.com/thx0701) | 2026-04-29 |


## 🤝 Commercial Cooperation

We accept the following commercial collaborations:

- **Enterprise Customization**: Custom deployment for internal AI tooling (Feishu, DingTalk, WeChat Work, Slack, etc.)
- **Technical Consulting**: Pi bridge integration and architecture design
- **Outsourcing Projects**: AI-related system development

**Contact**: **Email**: chg80333@gmail.com | **WeChat**: mongorz | [Telegram](https://t.me/+odGNDhCjbjdmMmZl) | [Discord](https://discord.gg/kHpwgaM4kq)


## 🙏 Contributors

<a href="https://github.com/chenhg5/cc-connect/graphs/contributors">
  <img src="https://contrib.rocks/image?repo=chenhg5/cc-connect&v=20250313" />
</a>


## ⭐ Star History

<a href="https://www.star-history.com/#chenhg5/cc-connect&Date">
 <picture>
   <source media="(prefers-color-scheme: dark)" srcset="https://api.star-history.com/svg?repos=chenhg5/cc-connect&type=Date&theme=dark" />
   <source media="(prefers-color-scheme: light)" srcset="https://api.star-history.com/svg?repos=chenhg5/cc-connect&type=Date" />
   <img alt="Star History Chart" src="https://api.star-history.com/svg?repos=chenhg5/cc-connect&type=Date" />
 </picture>
</a>


## 📄 License

MIT License


<p align="center">
  <sub>Built with ❤️ by the cc-connect community</sub>
</p>
