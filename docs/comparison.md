# muster compared with other ways to reach coding agents from your phone

This page compares muster with Happy, Omnara, Claude Code's own Remote Control,
and plain SSH plus tmux from a phone terminal app. Each claim about another
product comes from that product's own README, docs or site, listed under
[Sources](#sources) with the date it was read. A cell reads **not stated** when
those sources do not say. Products change quickly; check their pages before you
rely on a cell. Corrections are welcome.

Each claim about muster links to the page of this repository it comes from.

## At a glance

| | muster | Happy | Omnara | Claude Code Remote Control | SSH + tmux (Blink, Termius) |
|---|---|---|---|---|---|
| What it is | A self-hosted server that mirrors the tmux sessions on your machines to a phone web app | A desktop app and agent runtime with phone apps; the original `happy` CLI wraps Claude Code or Codex | A managed agent platform: it runs its own agent loop against a model you configure | A Claude Code feature that connects a local session to the Claude app and claude.ai/code | A phone terminal app connected to your machine's SSH (or Mosh) server |
| Hosted or self-hosted | Self-hosted only | Agents run on your hardware; phone sync goes through Happy's relay, which you can self-host | Omnara Cloud, or self-hosted with Docker Compose | Runs on your machine; traffic and the transcript go through Anthropic's servers | Self-hosted; nothing in between unless you add a VPN |
| Licence | MIT | MIT | Apache 2.0 | Proprietary (Anthropic's Commercial Terms) | Blink: GPL-3.0. Termius: not stated |
| Agents | Any agent in tmux. Claude Code renders as a chat; opencode and Hermes have read views; Codex, Gemini CLI and shells are terminal views | Claude, Codex, Grok; Gemini (via Antigravity), OpenCode and ACP agents in beta | Omnara's own agent, on any model behind an OpenAI- or Anthropic-compatible endpoint | Claude Code | Anything that runs in a terminal |
| Phone client | Installable web app (PWA) | Native iOS and Android apps, and web | Web dashboard, Slack, Discord, GitHub; no phone app described | The Claude app (iOS, Android) and claude.ai/code | The terminal app |
| Desktop platforms | Linux; macOS hosts are experimental. Windows not listed | macOS, Windows, Linux | Daemon on macOS or Linux | Wherever Claude Code runs (CLI, Desktop app, VS Code) | Any machine with an SSH server |
| Several machines | Yes, side by side | Yes | Yes; an agent can use several machines | Not stated; sessions are listed at claude.ai/code, named by default after the machine's hostname | One connection per machine |
| How the agent's machine connects | `muster-mirror` dials out to your server; no port opens on the host (optional MCP on loopback only). The server is reached only over your Tailscale tailnet | Through Happy's relay (end-to-end encrypted). A remote Happy Agent is reached over Tailcat, with no open port | The `omnarad` daemon connects out; no inbound ports | Outbound HTTPS only; no inbound ports | The phone connects in to the machine's SSH or Mosh server |
| Push notifications | Web Push (on iOS, after installing to the Home Screen) | Yes, through Expo's push service | Not stated (webhooks and chat integrations are) | Yes, in the Claude app | Not stated |
| Agent asks a question and waits | Yes: `attention_ask` / `attention_wait`, for Claude Code sessions (via the host's loopback MCP endpoint) and dispatched agents | Yes: answer permission requests and questions from the phone | Yes: a built-in question tool, and per-tool approvals | Yes: permission prompts and questions, with push | Only as a prompt in the terminal |
| Voice | Dictation into the message box: a local speech-to-text model on your server, or your own OpenAI-compatible API key. You read it, then press Send | A voice agent (ElevenLabs) that relays requests and approvals. 20 free minutes per 30 days, then a subscription, or your own ElevenLabs agent | Not stated | Not stated | Not stated |
| Sessions: wrapper or existing | Picks up any session already in tmux; nothing to wrap. Sessions outside tmux are not seen | Started through `happy claude` / `happy codex`, or run by Happy Desktop. Attaching to an already-running session: not stated | Agents are launched in Omnara (CLI, API, dashboard, Slack, Discord, GitHub) | `/remote-control` connects a session already running; or start with `claude remote-control` / `claude --remote-control` | `tmux attach` to any existing session |
| Install | One prompt pasted into your coding agent, the Claude Code plugin (`/plugin marketplace add ZacxDev/muster`), or [INSTALL.md](../INSTALL.md) by hand. Needs Docker, Tailscale and tmux | `npm install -g happy`; the desktop app (download or Homebrew cask); App Store / Google Play | Sign in to Omnara Cloud, or `git clone` and `docker compose up`; `npx omnara` CLI | Built into Claude Code | Install the app; run an SSH server |
| Cost | Free; you run the server | Free; voice is the one paid feature | Free to self-host. Cloud: free with your own keys and machines, or pay as you go | Included with Pro, Max, Team and Enterprise; API keys not supported | Blink: $19.99/year after a 2-week trial. Termius: free Starter tier with SSH and Mosh; paid tiers from $10/month |
| Telemetry | None unless you set an OTLP endpoint | The app sends product analytics to PostHog; it can be turned off | Not stated | The transcript is stored on Anthropic's servers while connected | Not stated |

## muster

muster mirrors **your own sessions**: a coding agent you started in tmux on one
of your machines shows up on your phone, and a message from the phone is typed
into that same session ([README](../README.md#how-it-works),
[mirroring.md](mirroring.md#mirrored-sessions-and-dispatched-agents)). Because
it works at the tmux layer, it does not care how you started the agent, and it
needs no wrapper; the cost is that it sees only sessions inside tmux. Claude
Code gets a rendered chat view; Codex, Gemini CLI and shells get a live
terminal ([README](../README.md#why-muster)).

It is self-hosted only: one Go server and Postgres, in Docker on one of your
machines, behind `tailscale serve`. Each other machine runs `muster-mirror`,
which opens an outbound tunnel to the server
([architecture.md](architecture.md)). It runs as a single replica
([limits.md](limits.md#deployment)). Telemetry is off unless an OTLP endpoint
is set ([configuration.md](configuration.md)). Licence: MIT
([LICENSE](../LICENSE)).

Where others are better:

- **Setup.** You need a Tailscale account, Docker on one machine and tmux on
  every machine. A hosted relay or a built-in feature asks for far less.
- **Native apps.** muster is a PWA. Happy and the Claude app are native phone
  apps.
- **macOS and Windows.** macOS hosts are experimental and have no Terminal view
  yet, so on a Mac only a Claude Code window whose rendered view resolves can be
  viewed ([limits.md](limits.md)). Windows is not listed. Happy's desktop app
  runs on macOS, Windows and Linux.
- **Non-Claude agents.** Codex and Gemini CLI are terminal views only, and the
  message box does not send to opencode yet ([README](../README.md#core)).
- **Voice.** muster's mic is dictation into the message box. Happy's voice
  agent holds a spoken conversation, reports back and takes approvals by voice.

## Happy

Happy describes itself as a harness that runs Claude, Codex and Grok (and, in
beta, Gemini, OpenCode and ACP agents) in one desktop app, reusing the
subscriptions you already pay for, with end-to-end encrypted iOS and Android
apps. The original `happy` CLI, now in maintenance mode, wraps the real
`claude` or `codex` in your terminal so you can continue from the phone; new
features ship in Happy Desktop. Phone sync goes through Happy's relay, which
carries ciphertext only, and the relay can be self-hosted from the same
repository. Push notifications go through Expo's push service. Voice is an
ElevenLabs voice agent: 20 free minutes per 30 days, then a subscription, or
bring your own ElevenLabs agent. The app and Desktop send product analytics to
PostHog, which can be turned off.

Where Happy is better than muster: native phone apps, end-to-end encryption
between your devices, a desktop app on macOS, Windows and Linux, nothing to
host if you use its public relay, teams that share sessions, and a full voice
agent. Where it differs: sessions start through Happy (the CLI wrapper, or
Happy Desktop's own runtime), rather than being picked up from tmux; its
sources do not say whether it can attach to a session you already started
without it.

## Omnara

Omnara's current README describes "the open-source managed agent platform", a
self-hostable alternative to Claude Managed Agents and OpenAI's Agents API. It
runs its own agent loop against any model behind an OpenAI- or
Anthropic-compatible endpoint, so it does not mirror a Claude Code or Codex
session you started yourself. You reach agents through a REST API, a
TypeScript SDK, a CLI, a web dashboard, and Slack, Discord and GitHub. Agents
can ask a person a question, and each tool can be set to run, ask first, or be
blocked. Machines run the `omnarad` daemon, which connects out with no inbound
ports, or agents use sandboxes from eleven providers. Self-host with Docker
Compose for free under Apache 2.0, or use Omnara Cloud. Its README and docs
index describe no phone app, push notifications or voice.

Where Omnara is better than muster: agents that keep running in cloud
sandboxes when your laptop is off, any model, an API to launch agents from your
own product, chat and GitHub integrations, and a hosted option. It solves a
different problem: running agents as a service, not reaching the terminal
sessions you already have.

## Claude Code Remote Control

Remote Control is built into Claude Code. It connects a Claude Code session
running on your machine to the Claude app on iOS and Android and to
claude.ai/code. You can start it with `claude remote-control` (server mode,
which can serve several sessions), `claude --remote-control`, or `/remote-control`
inside a session already running. The local session makes outbound HTTPS
requests only and opens no inbound port. While connected, the session
transcript is stored on Anthropic's servers. Push notifications reach the
Claude app when Claude decides, and for permission prompts and questions. It
needs a Pro, Max, Team or Enterprise plan; API keys, a custom
`ANTHROPIC_BASE_URL`, Amazon Bedrock, Google Cloud and Microsoft Foundry are
not supported.

Where Remote Control is better than muster: nothing to install or host if you
already use Claude Code and the Claude app, a native app, first-party support,
deep Claude Code integration (subagent progress, compaction), and organization
controls such as Trusted Devices. Where it differs: it is for Claude Code only,
and it routes the transcript through Anthropic's servers.

## SSH and tmux from a phone

A phone terminal app such as Blink Shell (iOS and iPadOS) or Termius (iOS,
Android and desktop) connects to your machine's SSH or Mosh server, and
`tmux attach` brings back any session already running. It works with every
terminal agent and tool, and there is no service in between. Blink is GPL-3.0
and costs $19.99 a year after a two-week trial; Termius has a free Starter tier
that includes SSH and Mosh, with paid tiers from $10 a month.

Where it is better than muster: nothing to run beyond an SSH server, any
operating system, and full terminal control. Where it differs: the phone has to
reach the machine (a port forward or a VPN), you work in a terminal on a small
screen, and there are no push notifications or asks of its own.

## Sources

All read on **2026-10-29**.

- **muster**: this repository's [README.md](../README.md),
  [INSTALL.md](../INSTALL.md), [mirroring.md](mirroring.md),
  [attention.md](attention.md#the-agent-tools), [push.md](push.md),
  [dictation.md](dictation.md), [architecture.md](architecture.md),
  [configuration.md](configuration.md), [limits.md](limits.md) and
  [LICENSE](../LICENSE).
- **Happy**:
  - <https://github.com/slopus/happy> (README; licence MIT per the repository)
  - <https://happy.engineering/>
  - <https://happy.engineering/mobile-app/>
  - <https://happy.engineering/mobile/>
  - <https://happy.engineering/how-it-works/>
  - <https://happy.engineering/voice/>
  - <https://happy.engineering/guides/terminal/>
  - <https://happy.engineering/guides/remote-server/>
  - <https://happy.engineering/docs/guides/self-hosting/>
  - The pages' source: <https://github.com/slopus/slopus.github.io/tree/main/content>
- **Omnara**:
  - <https://github.com/omnara-ai/omnara> (README; licence Apache-2.0 per the repository)
  - <https://www.omnara.com/>
  - <https://docs.omnara.com/llms.txt> (documentation index)
- **Claude Code Remote Control**:
  - <https://code.claude.com/docs/en/remote-control>
  - <https://github.com/anthropics/claude-code> (licence: LICENSE.md, Anthropic's Commercial Terms of Service)
- **SSH + tmux**:
  - <https://blink.sh/>
  - <https://github.com/blinksh/blink> (licence GPL-3.0 per the repository)
  - <https://termius.com/> (platforms)
  - <https://termius.com/pricing>

Not included: tools that manage several agents on one machine without a phone
client, since this page is about reaching agents from a phone.
