# pi-connect Installation & Configuration Guide

> **This document is designed to help users install and configure pi-connect for Pi.**

## What is pi-connect?

pi-connect bridges Pi to messaging platforms (Feishu, DingTalk, Telegram, Slack, Discord, LINE, WeChat Work, QQ, **personal WeChat** via the **Weixin (ilink)** platform).

GitHub: https://github.com/tasercake/pi-connect

**IMPORTANT: If you are helping a user install pi-connect, guide them through configuration choices:**
- Platform selection (Feishu, Telegram, Discord, etc.)
- API keys and authentication tokens
- Project paths and preferences

Don't guess values—always ask the user to choose via interactive prompts.

## Step 1: Install pi-connect

### Option A: npm (recommended for most users)

```bash
npm install -g pi-connect
```

After installation, the `pi-connect` binary will be available globally.


### Option B: Homebrew (macOS / Linux)

```bash
brew install pi-connect
```

### Option C: Download binary from GitHub Releases

Go to https://github.com/tasercake/pi-connect/releases and download the binary for your platform.

Typical artifact names (check the release page for exact filenames):

- Linux: `pi-connect-<version>-linux-amd64` (or `.tar.gz`)
- macOS: `pi-connect-<version>-darwin-amd64` / `arm64`
- Windows: `pi-connect-<version>-windows-amd64.exe` (or `.zip`)

```bash
# Example for Linux amd64 (replace URL with the asset link from the release you chose):
curl -L -o pi-connect https://github.com/tasercake/pi-connect/releases/latest/download/pi-connect-linux-amd64
chmod +x pi-connect
sudo mv pi-connect /usr/local/bin/
```

On macOS, you may need to remove the quarantine attribute:

```bash
xattr -d com.apple.quarantine pi-connect
```

### Option D: Build from source

Requires Go 1.22+.

```bash
git clone https://github.com/tasercake/pi-connect.git
cd pi-connect
make build
# Binary will be at ./pi-connect
```

## Step 2: Install Pi

pi-connect in this fork supports the Pi coding agent. Install and authenticate Pi before starting pi-connect. RPC transport requires Pi 0.82.1 or newer because it uses the `agent_settled` lifecycle event; older versions are not given a timer-based fallback that could finalize during retry or compaction.

Verify Pi works:

```bash
pi --version
```

## Step 3: Create config.toml

> **💡 Recommended: Use the Web UI** — After installing, run `pi-connect web` to configure the web admin and open the dashboard in your browser. You can visually create projects, add platforms, manage API providers, and even chat with your agent directly from the browser — no need to edit TOML files by hand. **Note:** `pi-connect web` only configures and opens the browser — you still need to run `pi-connect` separately to start the service.

If you prefer manual configuration, pi-connect looks for config in this order:
1. `-config <path>` flag (explicit)
2. `./config.toml` (current directory)
3. `~/.pi-connect/config.toml` (global, **recommended**)

If no config file exists, running `pi-connect` will auto-create a starter template at `~/.pi-connect/config.toml`.

**Manual config location:**

```bash
mkdir -p ~/.pi-connect
# If you cloned the repo, copy the example:
cp config.example.toml ~/.pi-connect/config.toml
# Or just run pi-connect once — it will create a starter config automatically
```

You can also use a local config in the current directory:

```bash
cp config.example.toml config.toml
```

The configuration has this structure:

```toml
# Optional global settings
# language = "en"  # "en", "zh", or "" (auto-detect)

[log]
level = "info"  # debug, info, warn, error

# Each [[projects]] entry connects one code folder to one or more messaging platforms
[[projects]]
name = "my-project"

[projects.agent]
type = "pi"

[projects.agent.options]
work_dir = "/absolute/path/to/your/project"
mode = "default"

# Add one or more platform sections below
```

## Step 4: Configure a Messaging Platform

Choose one or more platforms to connect. Each platform requires creating a bot/app on the platform's developer console and copying credentials into config.toml.

---

### Feishu (Lark) — No public IP needed

Connection: WebSocket long connection (SDK auto-negotiates)

**CLI shortcut (recommended):**

```bash
# Recommended: unified entry
pi-connect feishu setup --project my-project
pi-connect feishu setup --project my-project --app cli_xxx:sec_xxx

# Force modes (usually unnecessary)
pi-connect feishu new --project my-project

pi-connect feishu bind --project my-project --app cli_xxx:sec_xxx
```

Notes:
- `setup` is the unified entry:
  - no credentials => same as `new`
  - with `--app`/`--app-id` => same as `bind`
- `setup/new` prints a terminal QR code + URL for mobile scanning.
- If `--project` does not exist, pi-connect creates it automatically.
- This flow fills `app_id` / `app_secret`; in QR onboarding flow, Feishu usually pre-configures permissions and event subscriptions.
- Still verify app publish status and availability scope in Feishu Open Platform.

**Setup steps:**
1. Go to https://open.feishu.cn → Console → Create Enterprise App
2. Enable **Bot** capability (App Capabilities → Bot)
3. Go to **Permissions** → add `im:message.receive_v1`, `im:message:send_as_bot`
4. Go to **Event Subscriptions** → select **WebSocket long connection mode** → add event `im.message.receive_v1`
5. Publish the app version
6. Copy App ID and App Secret

**Config:**

```toml
[[projects.platforms]]
type = "feishu"

[projects.platforms.options]
app_id = "cli_xxxxxxxxxxxx"
app_secret = "xxxxxxxxxxxxxxxxxxxxxxxx"
```

**Detailed guide:** [docs/feishu.md](docs/feishu.md)

---

### DingTalk — No public IP needed

Connection: Stream mode (WebSocket)

**Setup steps:**
1. Go to https://open-dev.dingtalk.com → Create App
2. Enable **Bot** capability, select **Stream mode**
3. Configure permissions for messaging
4. Copy Client ID (AppKey) and Client Secret (AppSecret)

**Config:**

```toml
[[projects.platforms]]
type = "dingtalk"

[projects.platforms.options]
client_id = "dingxxxxxxxxxxxxxxxxx"
client_secret = "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
```

**Detailed guide:** [docs/dingtalk.md](docs/dingtalk.md)

---

### Telegram — No public IP needed

Connection: Long Polling

**Setup steps:**
1. Message @BotFather on Telegram → send `/newbot`
2. Follow prompts to set bot name and username (must end with `bot`)
3. Copy the bot token

**Config:**

```toml
[[projects.platforms]]
type = "telegram"

[projects.platforms.options]
token = "1234567890:ABCdefGHIjklMNOpqrsTUVwxyz"
```

**Detailed guide:** [docs/telegram.md](docs/telegram.md)

---

### Slack — No public IP needed

Connection: Socket Mode (WebSocket)

**Setup steps:**
1. Go to https://api.slack.com/apps → Create New App → From scratch
2. Enable **Socket Mode** (Settings → Socket Mode) → generate App-Level Token (`xapp-...`)
3. Subscribe to bot events: `message.im`, `app_mention` (Event Subscriptions)
4. Add Bot Token Scopes: `chat:write`, `im:history`, `im:read`, `im:write`, `app_mentions:read`
5. Install App to Workspace → copy Bot Token (`xoxb-...`)

**Config:**

```toml
[[projects.platforms]]
type = "slack"

[projects.platforms.options]
bot_token = "xoxb-your-bot-token"
app_token = "xapp-your-app-level-token"
```

**Detailed guide:** [docs/slack.md](docs/slack.md)

---

### Discord — No public IP needed

Connection: Gateway WebSocket

**Setup steps:**
1. Go to https://discord.com/developers/applications → New Application
2. Go to **Bot** → Add Bot → copy Token
3. Enable **Message Content Intent** (under Privileged Gateway Intents)
4. Go to **OAuth2** → URL Generator → select scope `bot` → select permissions `Send Messages`, `Read Message History`
5. Open the generated URL to invite bot to your server

**Config:**

```toml
[[projects.platforms]]
type = "discord"

[projects.platforms.options]
token = "your-discord-bot-token"
```

**Detailed guide:** [docs/discord.md](docs/discord.md)

---

### LINE — Requires public URL

Connection: HTTP Webhook (you need ngrok, cloudflared, or a server with public IP)

**Setup steps:**
1. Go to https://developers.line.biz/console/ → Create Messaging API channel
2. Copy Channel Secret and Channel Access Token (long-lived)
3. Set webhook URL in LINE console: `https://<your-public-domain>:<port>/callback`
4. Expose local port using ngrok/cloudflared: `ngrok http 8080` or `cloudflared tunnel --url http://localhost:8080`

**Config:**

```toml
[[projects.platforms]]
type = "line"

[projects.platforms.options]
channel_secret = "your-channel-secret"
channel_token = "your-channel-access-token"
port = "8080"
callback_path = "/callback"
```

---

### WeChat Work (企业微信) — Requires public URL

Connection: HTTP Webhook (you need ngrok, cloudflared, or a server with public IP)

**Setup steps:**
1. Log in to https://work.weixin.qq.com/wework_admin/frame
2. **App Management** → Create custom app → note AgentId and Secret
3. **My Enterprise** → note Corp ID
4. In the app → **Receive Messages** → Set API Receive:
   - URL: `https://<your-public-domain>:<port>/wecom/callback`
   - Token: any random string
   - EncodingAESKey: click "Random Generate" (43 chars)
   - **Start pi-connect FIRST, then save** (to pass URL verification)
5. **Trusted IP** → add your server's outbound public IP
6. (Optional) **WeChat Plugin** → scan QR to link personal WeChat

**Config:**

```toml
[[projects.platforms]]
type = "wecom"

[projects.platforms.options]
corp_id = "wwxxxxxxxxxxxxxxxxx"
corp_secret = "your-app-secret"
agent_id = "1000002"
callback_token = "your-callback-token"
callback_aes_key = "your-43-char-encoding-aes-key"
port = "8081"
callback_path = "/wecom/callback"
api_base_url = "https://qyapi.weixin.qq.com"  # optional: override WeChat Work API base URL (for private deployments)
enable_markdown = false  # true = Markdown messages (WeChat Work app only; personal WeChat shows "unsupported")
# proxy = "http://your-vps-ip:8888"  # optional: forward proxy if your IP is dynamic
```

**Detailed guide:** [docs/wecom.md](docs/wecom.md)

### Weixin (personal, ilink) — No public IP needed

Personal WeChat uses Tencent’s **ilink bot HTTP API** (same family as OpenClaw `openclaw-weixin`). The recommended flow is CLI QR login, which writes `token` (and related fields) into `config.toml`.

1. Run:

   ```bash
   pi-connect weixin setup --project my-project
   ```

2. Scan the QR code (or open the printed URL) in WeChat and confirm.

3. Restart pi-connect, then send a message from WeChat once so `context_token` is cached.

If you already have a Bearer token, use `pi-connect weixin bind --project my-project --token '<token>'`.

**Detailed guide (Chinese):** [docs/weixin.md](docs/weixin.md)

### QQ (via NapCat / OneBot v11) — No public IP needed

QQ integration requires a third-party OneBot v11 implementation (e.g., NapCat) as a bridge.

1. Deploy NapCat (recommended via Docker):
   ```bash
   docker run -d --name napcat -e ACCOUNT=<QQ号> -p 3001:3001 -p 6099:6099 --restart unless-stopped mlikiowa/napcat-docker:latest
   ```
2. First launch: check `docker logs -f napcat` for a QR code, scan with QQ mobile app to log in
3. Open NapCat WebUI at `http://localhost:6099`, enable **Forward WebSocket** on port 3001
4. Add to `config.toml`:

```toml
[[projects.platforms]]
type = "qq"

[projects.platforms.options]
ws_url = "ws://127.0.0.1:3001"  # NapCat Forward WebSocket URL
token = ""                       # optional: access_token (must match NapCat config)
allow_from = "*"                 # allowed QQ user IDs: "12345,67890" or "*" for all
```

**Detailed guide:** [docs/qq.md](docs/qq.md)

---

## Step 5: Run pi-connect

**Open the Web UI (recommended):**

```bash
pi-connect web    # configure web admin & open browser (does NOT start pi-connect)
pi-connect        # start the service
```

> **Note:** `pi-connect web` only configures the web admin and opens the dashboard in your browser — it does **not** start the pi-connect service itself. You still need to run `pi-connect` (or `pi-connect --config <path>`) separately to actually start the bridge. Think of it as two steps: configure first, then run.

**Normal startup:**

```bash
# Run with config.toml in current directory
pi-connect

# Or specify config path
pi-connect -config /path/to/config.toml

# Check version
pi-connect --version
```

You should see logs like:

```
level=INFO msg="platform started" project=my-project platform=feishu
level=INFO msg="engine started" project=my-project agent=pi platforms=1
level=INFO msg="pi-connect is running" projects=1
```

## Step 6: Chat Commands

Once running, send messages to your bot on the configured platform. Available slash commands:

```
/new [name]      — Start a new session
/list            — List agent sessions
/switch <id>     — Resume an existing session
/current         — Show current active session
/history [n]     — Show last n messages (default 10)
/mode [name]     — View/switch permission mode (default/edit/plan/yolo)
/quiet           — Toggle thinking/tool progress messages
/allow <tool>    — Pre-allow a tool (next session)
/provider [...]  — Manage API providers (list/add/remove/switch)
/stop            — Stop current execution
/help            — Show available commands
```

During a session, Pi may ask for tool permissions. Reply:
- `allow` or `允许` — approve this request
- `deny` or `拒绝` — reject this request
- `allow all` or `允许所有` — auto-approve all remaining requests this session

## Step 7: Enable Natural Language Scheduling

pi-connect supports scheduled tasks (cron jobs). You can always create them via slash commands (`/cron add ...`) or CLI (`pi-connect cron add ...`). To let Pi **understand natural language** like "every day at 6am, summarize trending repos", add the following instructions to the Pi project instructions in your `work_dir`.

**Content to add**:

```markdown
# pi-connect Integration

This project is managed via pi-connect, a bridge to messaging platforms.

## Scheduled tasks (cron)
When the user asks you to do something on a schedule (e.g. "every day at 6am",
"every Monday morning"), use the Bash/shell tool to run:

  pi-connect cron add --cron "<min> <hour> <day> <month> <weekday>" --prompt "<task description>" --desc "<short label>"

Environment variables CC_PROJECT and CC_SESSION_KEY are already set — do NOT
specify --project or --session-key.

Examples:
  pi-connect cron add --cron "0 6 * * *" --prompt "Collect GitHub trending repos and send a summary" --desc "Daily GitHub Trending"
  pi-connect cron add --cron "0 9 * * 1" --prompt "Generate a weekly project status report" --desc "Weekly Report"

To list, edit, or delete cron jobs:
  pi-connect cron list
  pi-connect cron edit <job-id> <field> <value>
  pi-connect cron del <job-id>

Use `cron edit` to modify a single field instead of delete-and-recreate.
Common editable fields: cron_expr, prompt, exec, description, enabled (true/false), mute (true/false), timeout_mins (int).
Run `pi-connect cron edit --help` for the full field list.

Examples:
  pi-connect cron edit abc123 cron_expr "0 9 * * *"
  pi-connect cron edit abc123 enabled false
  pi-connect cron edit abc123 prompt "Updated daily summary task"

## Send message to current chat
To proactively send a message back to the user's chat session (use --stdin heredoc for long/multi-line messages):

  pi-connect send --stdin <<'CCEOF'
  your message here (any special characters are safe)
  CCEOF

For short single-line messages:

  pi-connect send -m "short message"
```

After adding this file, the agent will be able to translate natural language scheduling requests into `pi-connect cron add` commands automatically.

> **Tip:** You may want to add local instruction files to your `.gitignore` if you don't want pi-connect instructions committed to version control.

## Multi-Project Setup

A single pi-connect process can manage multiple projects. Each project uses Pi with its own work directory and platforms:

```toml
[[projects]]
name = "backend"

[projects.agent]
type = "pi"

[projects.agent.options]
work_dir = "/path/to/backend"
mode = "default"

[[projects.platforms]]
type = "feishu"

[projects.platforms.options]
app_id = "cli_xxx"
app_secret = "xxx"

# Second project — also using Pi
[[projects]]
name = "frontend"

[projects.agent]
type = "pi"

[projects.agent.options]
work_dir = "/path/to/frontend"
mode = "default"

[[projects.platforms]]
type = "telegram"

[projects.platforms.options]
token = "xxx"
```

## Upgrade

### Check current version

```bash
pi-connect --version
```

### npm users

```bash
npm update -g pi-connect
```

### Binary users

Check the latest release at https://github.com/tasercake/pi-connect/releases and compare with your local version. To upgrade:

```bash
# Linux/macOS — replace with your platform suffix
curl -L -o /usr/local/bin/pi-connect https://github.com/tasercake/pi-connect/releases/latest/download/pi-connect-$(uname -s | tr '[:upper:]' '[:lower:]')-$(uname -m | sed 's/x86_64/amd64/' | sed 's/aarch64/arm64/')
chmod +x /usr/local/bin/pi-connect
```

### Source users

```bash
cd pi-connect
git pull
make build
```

After upgrading, restart the running pi-connect process.

## Step 8: Run as Background Service (Optional)

You can run pi-connect as a daemon managed by the OS init system (Linux systemd user service, macOS launchd LaunchAgent, Windows Task Scheduler task).

### Install the daemon

```bash
pi-connect daemon install --config ~/.pi-connect/config.toml
```

You can also point the daemon at the directory that contains `config.toml`:

```bash
pi-connect daemon install --work-dir ~/.pi-connect
```

Optional flags: `--config PATH`, `--log-file PATH`, `--log-max-size N` (MB), `--work-dir DIR`, `--force` (overwrite existing unit). `--config` points to a config file, while `--work-dir` points to the directory containing `config.toml`.

### Control the service

```bash
pi-connect daemon start
pi-connect daemon stop
pi-connect daemon restart
pi-connect daemon status
```

### View logs

```bash
pi-connect daemon logs           # tail current log
pi-connect daemon logs -f         # follow (like tail -f)
pi-connect daemon logs -n 100     # last 100 lines
pi-connect daemon logs --log-file /path/to/log  # custom log file
```

Logs auto-rotate at the configured max size and keep one backup.

On Windows, `daemon install` creates a native Task Scheduler task named `pi-connect`.
The task runs at user logon and is also started immediately after installation. The
installer writes a small PowerShell launcher under `~/.pi-connect` so the scheduled
task uses the selected config directory, log file, PATH, and proxy environment.

### Uninstall

```bash
pi-connect daemon uninstall
```

## Additional Features

The following additional features are available:

- **Pi Agent**: Pi coding agent integration
- **Voice Messages (STT)**: Speech-to-text via Whisper API (OpenAI / Groq / SiliconFlow). Requires `ffmpeg` and `[speech]` config.
- **Voice Reply (TTS)**: Text-to-speech via Qwen TTS / OpenAI TTS. Requires `ffmpeg` and `[tts]` config.
- **Image Messages**: Send images to Pi for multimodal analysis
- **API Provider Management**: Runtime switching between API providers via `/provider` command or CLI
- **CLI Send**: `pi-connect send` to inject messages into active sessions from external processes

## Troubleshooting

- **"session already in use"** — A previous Pi process may still be running. Use `/new` to start a fresh session.
- **No response from bot** — Check `pi-connect` logs. Set `level = "debug"` in `[log]` for verbose output.
- **WeChat Work can't send messages** — Ensure your outbound IP is in the Trusted IP whitelist. If using a proxy, check the proxy is reachable.
- **LINE/WeChat Work can't receive messages** — Ensure your webhook URL is publicly accessible (ngrok/cloudflared running).
- **macOS binary won't open** — Run `xattr -d com.apple.quarantine pi-connect` to remove quarantine flag.
