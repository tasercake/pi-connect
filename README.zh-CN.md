<p align="center">
  <img src="./docs/images/banner.svg" alt="CC-Connect Banner" width="800"/>
</p>

<p align="center">
  <a href="https://github.com/tasercake/pi-connect/actions/workflows/ci.yml">
    <img src="https://github.com/tasercake/pi-connect/actions/workflows/ci.yml/badge.svg" alt="CI Status"/>
  </a>
  <a href="https://github.com/tasercake/pi-connect/releases">
    <img src="https://img.shields.io/github/v/release/tasercake/pi-connect?include_prereleases" alt="Release"/>
  </a>
  <a href="https://www.npmjs.com/package/pi-connect">
    <img src="https://img.shields.io/npm/dm/pi-connect?logo=npm" alt="npm downloads"/>
  </a>
  <a href="https://github.com/tasercake/pi-connect/blob/main/LICENSE">
    <img src="https://img.shields.io/badge/License-MIT-yellow.svg" alt="License"/>
  </a>
  <a href="https://goreportcard.com/report/github.com/tasercake/pi-connect">
    <img src="https://goreportcard.com/badge/github.com/tasercake/pi-connect" alt="Go Report Card"/>
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
    <img src="https://trendshift.io/api/badge/repositories/23266" alt="tasercake/pi-connect | Trendshift" style="width: 250px; height: 55px;" width="250" height="55"/>
  </a>
</p>


## ❤️ 赞助

> 想在这里展示？联系：chg80333@gmail.com | 微信：mongorz

<details open>
<summary>赞助商</summary>

此 Pi-only fork 已简化赞助商信息。

</details>

---

<br>

<p align="center">
  <b>在任何聊天工具里，远程操控 Pi。随时随地，随心所欲。</b>
</p>

<p align="center">
  pi-connect 把运行在你机器上的 Pi 桥接到你日常使用的即时通讯工具。<br/>
  代码审查、资料研究、自动化任务、数据分析 —— 只要 Pi 能做的事，<br/>
  都能通过手机、平板或任何有聊天应用的设备来完成。
</p>

<p align="center">
  <img src="docs/images/connector.png" alt="CC-Connect 架构图" width="90%"/>
</p>


## 🆕 v1.3.0 更新了什么

- **🌐 Web 管理后台（推荐）** — 内置全功能可视化管理界面，**无需额外依赖**。支持项目增删改查、服务商管理、会话监控、定时任务编辑，还可以**直接在浏览器里和 Pi 对话**。支持 5 种语言 (en/zh/zh-TW/ja/es)。建议通过 Web UI 管理 pi-connect，无需手动编辑 `config.toml`。运行 `pi-connect web` 配置并打开管理后台，然后运行 `pi-connect` 启动服务。
- **生命周期事件钩子** — 新增 `[[hooks]]` 配置，支持在消息收发、会话开始/结束、定时任务触发、权限请求、错误等事件时触发 Shell 命令或 HTTP Webhook。默认异步，失败不阻塞。
- **技能管理** — 新增 `/skills` 页面，支持本地技能浏览和推荐预设。
- **全局服务商管理** — 在 Web UI 中添加/编辑/删除 Provider，支持从 cc-switch 配置导入。
- **个人微信** — 用 **微信个人号（ilink 长轮询）** 和本地 Pi 对话；支持扫码 `weixin setup`、CDN 收发图片/文件，**无需公网 IP**。*[接入说明 → `docs/weixin.md`](docs/weixin.md)*
- **微博私信** — 通过 **微博私信** 与 Pi 对话，WebSocket 连接，无需公网 IP，支持流式文本回复。
- **飞书增强** — 自动解析 `@成员` 提及、多级回复链识别、完成 Emoji 反应。
- **Pi-only fork** — 此 fork 只保留 Pi 作为内置 agent。


## 🧩 平台能力一览

内置各渠道在 pi-connect 里的大致能力对照，方便快速对比。

**图例**

| 符号 | 含义 |
|------|------|
| ✅ | **稳定版** pi-connect + 常规配置下可用 |
| ⚠️ | 部分支持、需额外配置（如语音/STT）或受厂商接口 / 应用类型限制 |
| ❌ | 不支持或实际不可用 |

† **QQ（NapCat / OneBot）** — 非官方自建桥接，体验依赖你的 NapCat 与网络环境。

| 能力 | 飞书 | 钉钉 | Telegram | Slack | Discord | LINE | 企业微信 | 微博 | **微信个人号**<br>（ilink） | QQ† | QQ 官方机器人 |
|------|:----:|:----:|:--------:|:-----:|:-------:|:----:|:--------:|:----:|:--------------------------:|:---:|:------------:|
| 文本与斜杠命令 | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| Markdown / 卡片 | ✅ | ✅ | ✅ | ✅ | ✅ | ⚠️ | ⚠️ | ❌ | ✅ | ✅ | ✅ |
| 流式 / 分片回复 | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| 图片与文件 | ✅ | ✅ | ✅ | ✅ | ✅ | ⚠️ | ✅ | ❌ | ✅ | ✅ | ✅ |
| 语音 / STT / TTS | ⚠️ | ⚠️ | ✅ | ⚠️ | ⚠️ | ❌ | ⚠️ | ❌ | ✅ | ⚠️ | ⚠️ |
| 私聊 | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| 群聊 / 频道 | ✅ | ✅ | ✅ | ✅ | ✅ | ⚠️ | ✅ | ❌ | ✅ | ✅ | ✅ |

> **企业微信：** Webhook 模式需要**公网 URL**；长连接等模式多数**不需要**。  
> **语音行：** 多数平台要在 `config.toml` 里配置 `[speech]` / TTS 等，表中为经验性归纳。  
> 分平台接入步骤见下文 [平台接入指南](#-平台接入指南)。


## ✨ 为什么选择 pi-connect？

### 🤖 Pi Agent 支持
**Pi-only agent bridge** — 此 fork 专注于 Pi，并保持平台桥接层简单。

### 📱 平台灵活性
**11 大聊天平台** — 飞书、钉钉、Slack、Telegram、Discord、企业微信、微博、LINE、QQ、QQ 官方机器人，以及 **微信个人号（ilink）**。大部分平台**无需公网 IP**。

### 🔄 多机器人中继
**多机器人中继** — 在群聊中绑定多个 Pi-backed 机器人，让它们在同一个会话中协作。

### 🎮 完整的聊天控制
**聊天即控制** — 切换模型 (`/model`)、切换推理强度 (`/reasoning`)、切换权限模式 (`/mode`)、管理会话，全部通过斜杠命令完成。

**聊天切换工作目录** — 使用 `/dir <路径>` 切换下一次会话启动目录（`/cd <路径>` 为兼容别名），并支持 `/dir <序号>` / `/dir -` 快速在历史目录间跳转。

### 🧠 持久化记忆
**Agent 记忆** — 在聊天中直接读写 Agent 指令文件 (`/memory`)，无需回到终端。

### ⏰ 智能定时任务
**定时任务** — 自然语言创建 cron 任务。"每天早上6点总结 GitHub trending" 即刻生效。

### 🎤 多模态支持
**语音 & 图片** — 发语音或截图，pi-connect 自动处理 STT/TTS 和多模态转发。

### 📦 多项目架构
**多项目管理** — 一个进程同时管理多个项目，各自独立的 Pi + 平台组合。

### 🌍 多语言界面
**5 种语言** — 原生支持英语、中文（简体/繁体）、日语和西班牙语。内置 i18n 让每个人都能得心应手。


<p align="center">
  <img src="docs/images/screenshot/pi-connect-lark.JPG" alt="飞书" width="32%" />
  <img src="docs/images/screenshot/pi-connect-telegram.JPG" alt="Telegram" width="32%" />
  <img src="docs/images/screenshot/pi-connect-wechat.JPG" alt="微信" width="32%" />
</p>
<p align="center">
  <em>左：飞书 &nbsp;|&nbsp; Telegram &nbsp;|&nbsp; 右：微信</em>
</p>


## 🚀 快速开始

### 🤖 通过 Pi 安装配置（推荐）

> **最简单的方式** — 把这段话发给 Pi，它会帮你完成整个安装和配置过程：

```bash
请参考 https://raw.githubusercontent.com/tasercake/pi-connect/refs/heads/main/INSTALL.md 帮我安装和配置 pi-connect
```


### 📦 手动安装

**通过 npm：**

```bash
# npm install -g pi-connect
```

**通过 Homebrew（macOS / Linux）：**

```bash
brew install pi-connect
```

**从 [GitHub Releases](https://github.com/tasercake/pi-connect/releases) 下载：**

```bash
# Linux amd64 - 稳定版
curl -L -o pi-connect https://github.com/tasercake/pi-connect/releases/latest/download/pi-connect-linux-amd64
chmod +x pi-connect
sudo mv pi-connect /usr/local/bin/

```

**从源码编译（需要 Go 1.22+）：**

```bash
git clone https://github.com/tasercake/pi-connect.git
cd pi-connect
make build
```


### ⚙️ 配置

> **💡 推荐使用 Web UI 配置** — 安装完成后，运行 `pi-connect web` 配置 Web 管理后台并在浏览器中打开。可以可视化创建项目、添加平台、管理服务商、直接和 Agent 聊天，无需手动编辑 TOML 文件。**注意：** `pi-connect web` 仅用于配置和打开浏览器，并不会启动 pi-connect 服务本身，你仍需单独运行 `pi-connect` 来启动。

如果你更喜欢手动配置：

```bash
mkdir -p ~/.pi-connect
cp config.example.toml ~/.pi-connect/config.toml
vim ~/.pi-connect/config.toml
```

在项目配置里设置 `admin_from = "alice,bob"` 后，只有这些用户 ID 才能执行 `/dir`、`/shell` 等特权命令。
执行 `/dir reset` 时，pi-connect 会恢复配置中的 `work_dir`，并清除保存在 `data_dir/projects/<project>.state.json` 里的目录覆盖状态。


### ▶️ 运行

```bash
./pi-connect
```


### 🔄 升级

```bash
# npm
npm install -g pi-connect

# Homebrew
brew upgrade pi-connect

# 二进制自更新
pi-connect update           # 稳定版
pi-connect update --pre     # 含预发布版本
```


## 📊 支持状态

| 组件 | 类型 | 状态 |
|------|------|------|
| Agent | Pi | ✅ 已支持 |
| Platform | 飞书 (Lark) | ✅ WebSocket — 无需公网 IP |
| Platform | 钉钉 | ✅ Stream — 无需公网 IP |
| Platform | Telegram | ✅ Long Polling — 无需公网 IP |
| Platform | Slack | ✅ Socket Mode — 无需公网 IP |
| Platform | Discord | ✅ Gateway — 无需公网 IP |
| Platform | 微博 | ✅ WebSocket — 无需公网 IP |
| Platform | LINE | ✅ Webhook — 需要公网 URL |
| Platform | 企业微信 | ✅ WebSocket / Webhook |
| Platform | 微信个人号（ilink） | ✅— HTTP 长轮询 — 无需公网 IP |
| Platform | QQ (NapCat/OneBot) | ✅ WebSocket |
| Platform | QQ 官方机器人 | ✅ WebSocket — 无需公网 IP |


## 📖 平台接入指南

| 平台 | 指南 | 连接方式 | 需要公网 IP? |
|------|------|---------|-------------|
| 飞书 (Lark) | [docs/feishu.md](docs/feishu.md) | WebSocket | 不需要 |
| 钉钉 | [docs/dingtalk.md](docs/dingtalk.md) | Stream | 不需要 |
| Telegram | [docs/telegram.md](docs/telegram.md) | Long Polling | 不需要 |
| Slack | [docs/slack.md](docs/slack.md) | Socket Mode | 不需要 |
| Discord | [docs/discord.md](docs/discord.md) | Gateway | 不需要 |
| 微博 | [docs/weibo.md](docs/weibo.md) | WebSocket | 不需要 |
| 企业微信 | [docs/wecom.md](docs/wecom.md) | WebSocket / Webhook | 不需要 (WS) / 需要 (Webhook) |
| 微信个人号（ilink） | [docs/weixin.md](docs/weixin.md) | HTTP 长轮询（ilink） | 不需要 |
| QQ / QQ 机器人 | [docs/qq.md](docs/qq.md) | WebSocket | 不需要 |


## 🎯 核心功能

### 💬 会话管理

```
/new [名称]            创建新会话
/list                  列出所有会话
/switch <id>           切换会话
/current               查看当前会话
/dir [路径|reset]      查看、切换或重置工作目录
```

项目配置也可以开启“长时间空闲后自动切到新会话”：

```toml
[[projects]]
reset_on_idle_mins = 60
```


### 🛡️ 系统用户隔离 (`run_as_user`)

在 Linux/macOS 上，项目可以用另一个 Unix 用户身份启动 Pi，从而在操作系统层面实现文件系统隔离。

```toml
[[projects]]
name = "pi-sandboxed"
run_as_user = "partseeker-coder"
run_as_env = ["PGSSLROOTCERT"]
```

目标用户需要：supervisor 对其配置免密 sudo、自身不拥有 sudo、对 `work_dir` 有读写权限、拥有自己的 Pi 凭据。
详见[环境传播清单](./docs/usage.md#environment-propagation-what-moves-into-the-target-users-home)。
完整设置说明见 [`docs/usage.md`](./docs/usage.md#running-agents-as-a-different-unix-user-run_as_user)。

启动 pi-connect 之前，可用以下命令审核配置：

```bash
pi-connect doctor user-isolation
```

该命令会执行三项前置检查和一次隔离探测，报告目标用户能/不能读取的内容。如果任一检查失败或探测到跨用户泄漏，pi-connect 将拒绝启动。

---

### 🔐 权限模式

```
/mode             查看可用模式
/mode yolo        # 自动批准所有工具
/mode default     # 每次工具调用前询问
```


### 🔄 Provider 管理

```
/provider list              列出 Provider
/provider switch <名称>     运行时切换 API Provider
```


### 🤖 模型选择

```
/model                      列出可用模型（格式：alias - model）
/model switch <alias>       按别名切换模型
```


### 📂 工作目录

```
/dir                         查看当前工作目录与历史
/dir <路径>                  切换到指定目录（相对或绝对路径）
/dir <序号>                  按历史序号切换
/dir -                       返回上一个目录
/cd <路径>                   `/dir <路径>` 的兼容别名
```


### ⏰ 定时任务

```bash
/cron add 0 6 * * * 帮我总结 GitHub trending
```

### 📎 Agent 回传图片和文件

当 Agent 在本地生成了截图、图表、PDF、日志包等文件时，可以主动把附件发回当前聊天。

首版支持：
- 飞书
- Telegram

如果当前 Agent 不是原生注入 system prompt 的类型，升级后请先在聊天里执行一次：

```text
/bind setup
```

或：

```text
/cron setup
```

这样会把最新的 pi-connect 指令写入项目记忆文件，Agent 才会知道如何回传附件。

你也可以在 `config.toml` 里全局控制这项能力：

```toml
attachment_send = "on"  # 默认 "on"；设为 "off" 会禁用图片/文件回传
```

这个开关与 agent 的 `/mode` 独立，只控制 `pi-connect send --image/--file` 这条附件回传路径。

回传方式：

```bash
pi-connect send --image /absolute/path/to/chart.png
pi-connect send --file /absolute/path/to/report.pdf
pi-connect send --file /absolute/path/to/report.pdf --image /absolute/path/to/chart.png
```

要点：
- 使用绝对路径最稳妥。
- `--image` 和 `--file` 都可以重复传多个。
- `attachment_send = "off"` 只会关闭附件回传，普通文本回复仍然正常。
- 这个命令是给“生成后的附件回传”用的，不是给普通文本回复用的。

📖 **完整文档：** [docs/usage.zh-CN.md](docs/usage.zh-CN.md)


## 📚 文档

- [使用指南](docs/usage.zh-CN.md) — 完整功能文档
- [INSTALL.md](INSTALL.md) — AI Agent 友好的安装指南
- [config.example.toml](config.example.toml) — 配置模板
- [CONTRIBUTING.md](CONTRIBUTING.md) — Issue / PR 提交流程与贡献说明


## 👥 社区

- [Discord](https://discord.gg/kHpwgaM4kq)
- [Telegram](https://t.me/+odGNDhCjbjdmMmZl)


## ☕ 支持项目

如果 pi-connect 对你有帮助，请考虑请我们喝杯咖啡！你的支持帮助我们：

- 🛠️ 维护和改进项目
- 📚 编写更好的文档和教程
- 🐛 更快修复 bug 和添加新功能
- ☕ 让开发者保持精力充沛

### 捐赠方式

**Buy Me a Coffee**：[https://buymeacoffee.com/cg33](https://buymeacoffee.com/cg33)

**微信支付 / 支付宝**：

| 微信支付 | 支付宝 |
|:----------:|:------:|
| <img src="docs/images/wechatpay.jpg" alt="微信支付" width="150"> | <img src="docs/images/alipay.jpg" alt="支付宝" width="150"> |

### 感谢捐赠者！🎉

感谢每一位支持这个项目的朋友。捐赠时留言你的 GitHub 用户名，我们会在这里展示！

<!-- 捐赠者名单 -->
| 头像 | GitHub 用户名 | 日期 |
|------|-----------------|------|
| <img src="https://avatars.githubusercontent.com/u/1762560?v=4" width="40" height="40" style="border-radius: 50%;"> | [@thx0701](https://github.com/thx0701) | 2026-04-29 |


## 🤝 商业合作

我们接受以下商业合作：

- **企业定制**：为企业定制内部 AI 工具入口（飞书、钉钉、企业微信、Slack 等）
- **技术咨询**：AI agent 集成方案设计与架构咨询
- **外包项目**：AI 相关系统开发

**联系方式**：**邮箱**：chg80333@gmail.com | **微信**：mongorz | [Telegram](https://t.me/+odGNDhCjbjdmMmZl) | [Discord](https://discord.gg/kHpwgaM4kq)


## 🙏 贡献者

<a href="https://github.com/tasercake/pi-connect/graphs/contributors">
  <img src="https://contrib.rocks/image?repo=tasercake/pi-connect&v=20250313" />
</a>


## ⭐ Star History

<a href="https://www.star-history.com/#tasercake/pi-connect&Date">
 <picture>
   <source media="(prefers-color-scheme: dark)" srcset="https://api.star-history.com/svg?repos=tasercake/pi-connect&type=Date&theme=dark" />
   <source media="(prefers-color-scheme: light)" srcset="https://api.star-history.com/svg?repos=tasercake/pi-connect&type=Date" />
   <img alt="Star History Chart" src="https://api.star-history.com/svg?repos=tasercake/pi-connect&type=Date" />
 </picture>
</a>


## 📄 License

MIT License


<p align="center">
  <sub>由 pi-connect 社区用 ❤️ 构建</sub>
</p>
