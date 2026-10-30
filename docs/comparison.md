# muster compared with other ways to reach coding agents from your phone

This page compares muster with Happy, Claude Code's own Remote Control, and
plain SSH plus tmux from a phone terminal app. Each claim about another
product comes from that product's own README, docs or site, listed under
[Sources](#sources) with the date it was read. A cell reads **not stated** when
those sources do not say. Products change quickly; check their pages before you
rely on a cell. Corrections are welcome.

Each claim about muster links to the page of this repository it comes from.

## At a glance

| | muster | Happy | Claude Code Remote Control | SSH + tmux (Blink, Termius) |
|---|---|---|---|---|
| What it is | A self-hosted server that mirrors the tmux sessions on your machines to a phone web app | A desktop app and agent runtime with phone apps; the original `happy` CLI wraps Claude Code or Codex | A Claude Code feature that connects a local session to the Claude app and claude.ai/code | A phone terminal app connected to your machine's SSH (or Mosh) server |
| Hosted or self-hosted | Self-hosted only | Agents run on your hardware; phone sync goes through Happy's relay, which you can self-host | Runs on your machine; traffic and the transcript go through Anthropic's servers | Self-hosted; nothing in between unless you add a VPN |
| Licence | MIT | MIT | Proprietary (Anthropic's Commercial Terms) | Blink: GPL-3.0. Termius: not stated |
| Agents | Any agent in tmux. Claude Code renders as a chat; opencode and Hermes have read views; Codex, Gemini CLI and shells are terminal views | Claude, Codex, Grok; Gemini (via Antigravity), OpenCode and ACP agents in beta | Claude Code | Anything that runs in a terminal |
| Phone client | Installable web app (PWA) | Native iOS and Android apps, and web | The Claude app (iOS, Android) and claude.ai/code | The terminal app |
| Desktop platforms | Linux; macOS hosts are experimental. Windows not listed | macOS, Windows, Linux | Wherever Claude Code runs (CLI, Desktop app, VS Code) | Any machine with an SSH server |
| Several machines | Yes, side by side | Yes | Not stated; sessions are listed at claude.ai/code, named by default after the machine's hostname | One connection per machine |
| How the agent's machine connects | `muster-mirror` dials out to your server; no port opens on the host (optional MCP on loopback only). The server is reached only over your Tailscale tailnet | Through Happy's relay (end-to-end encrypted). A remote Happy Agent is reached over Tailcat, with no open port | Outbound HTTPS only; no inbound ports | The phone connects in to the machine's SSH or Mosh server |
| Push notifications | Web Push (on iOS, after installing to the Home Screen) | Yes, through Expo's push service | Yes, in the Claude app | Blink: an alert when the terminal prints BEL, OSC 9 or OSC 777, while its connection is alive; server-side push: not stated. Termius: not stated |
| Agent asks a question and waits | Yes: `attention_ask` / `attention_wait`, for Claude Code sessions (via the host's loopback MCP endpoint) and dispatched agents | Yes: answer permission requests and questions from the phone | Yes: permission prompts and questions, with push | Only as a prompt in the terminal; no ask/answer queue |
| Voice | Dictation into the message box: a local speech-to-text model on your server, or your own OpenAI-compatible API key. You read it, then press Send | A voice agent (ElevenLabs) that relays requests and approvals. Paid beyond a free allowance, or bring your own ElevenLabs agent | Not stated | Not stated |
| Sessions: wrapper or existing | Picks up any session already in tmux; nothing to wrap. Sessions outside tmux are not seen | Started through `happy claude` / `happy codex`, or run by Happy Desktop. Attaching to an already-running session: not stated | `/remote-control` connects a session already running; or start with `claude remote-control` / `claude --remote-control` | `tmux attach` to any existing session |
| Install | One prompt pasted into your coding agent, the Claude Code plugin (`/plugin marketplace add ZacxDev/muster`), or [INSTALL.md](../INSTALL.md) by hand. Needs Docker, Tailscale and tmux | `npm install -g happy`; the desktop app (download or Homebrew cask); App Store / Google Play | Built into Claude Code | Install the app; run an SSH server |
| Cost | Free; you run the server | Free; voice is the one paid feature | Included with a paid Claude plan (Pro, Max, Team, Enterprise); API keys not supported | Blink: paid, after a free trial. Termius: free tier with SSH and Mosh, and paid tiers |
| Telemetry | Nothing is sent unless you configure it: OTLP tracing and Faro front-end monitoring are both off by default | The app sends product analytics to PostHog; it can be turned off | Claude Code sends usage metrics (never code, prompts or file paths) by default; `DISABLE_TELEMETRY=1` turns them off, and Remote Control keeps working (on recent versions) unless Trusted Devices is required for your account. Error reports are separate and are turned off with `DISABLE_ERROR_REPORTING=1` | Not stated |

Omnara now describes
itself as a managed agent platform: it runs its own agent rather than mirroring
sessions you started, so it is out of scope here (see [Sources](#sources)).

## muster

muster mirrors **your own sessions**: a coding agent you started in tmux on one
of your machines shows up on your phone, and a message from the phone is typed
into that same session ([README](../README.md#how-it-works),
[mirroring.md](mirroring.md#mirrored-sessions-and-dispatched-agents)). Because
it works at the tmux layer, it does not care how you started the agent; the
cost is that it sees only sessions inside tmux. It is one Go server and
Postgres, in Docker on one of your machines, behind `tailscale serve`
([architecture.md](architecture.md)), and it runs as a single replica
([limits.md](limits.md#deployment)). Telemetry settings:
[configuration.md](configuration.md#tracing-muster-server-and-muster-mirror)
(OTLP) and [configuration.md](configuration.md#muster-server-optional-features)
(`MUSTER_FARO_URL`, `MUSTER_FARO_UPSTREAM`). Licence: [LICENSE](../LICENSE).

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

The original `happy` CLI is now in maintenance mode; new features ship in
Happy Desktop, which runs the agents itself and reuses the subscriptions you
already pay for. The relay carries ciphertext only.

Where Happy is better than muster: native phone apps, end-to-end encryption
between your devices, a desktop app on macOS, Windows and Linux, nothing to
host if you use its public relay, teams that share sessions, and a full voice
agent. Where it differs: sessions start through Happy (the CLI wrapper, or
Happy Desktop's own runtime), rather than being picked up from tmux.

## Claude Code Remote Control

`claude remote-control` runs in server mode, which can serve several sessions
at once.

Where Remote Control is better than muster: nothing to install or host if you
already use Claude Code and the Claude app, a native app, first-party support,
deep Claude Code integration (subagent progress, compaction), and organization
controls such as Trusted Devices. Where it differs: it is for Claude Code only,
it routes the transcript through Anthropic's servers, and it does not work with
a custom `ANTHROPIC_BASE_URL`, Amazon Bedrock, Google Cloud or Microsoft
Foundry.

## SSH and tmux from a phone

Blink Shell runs on iOS and iPadOS; Termius runs on iOS, Android and desktop.
Blink's alerts come from the terminal itself, so they depend on the connection
staying alive: iOS can drop a backgrounded connection (Blink's `geo` command
works around that), and some sequences are not supported over Mosh.

Where it is better than muster: nothing to run beyond an SSH server, any
operating system, full terminal control, and no service in between. Where it
differs: the phone has to reach the machine (a port forward or a VPN), you work
in a terminal on a small screen, alerts arrive only while the connection is
alive, and there is no ask/answer queue.

## Sources

All read on **2026-10-29**; the Blink notifications page and the Claude Code
data-usage page on **2026-10-30**.

- **muster**: this repository's [README.md](../README.md),
  [INSTALL.md](../INSTALL.md), [mirroring.md](mirroring.md),
  [attention.md](attention.md#the-agent-tools), [push.md](push.md),
  [dictation.md](dictation.md), [architecture.md](architecture.md),
  [configuration.md](configuration.md), [telemetry.md](telemetry.md),
  [limits.md](limits.md) and [LICENSE](../LICENSE).
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
- **Claude Code Remote Control**:
  - <https://code.claude.com/docs/en/remote-control>
  - <https://code.claude.com/docs/en/data-usage> (telemetry and its opt-outs)
  - <https://github.com/anthropics/claude-code> (licence: LICENSE.md, Anthropic's Commercial Terms of Service)
- **SSH + tmux**:
  - <https://blink.sh/>
  - <https://docs.blink.sh/basics/notifications>
  - <https://github.com/blinksh/blink> (licence GPL-3.0 per the repository)
  - <https://termius.com/> (platforms)
  - <https://termius.com/pricing>
- **Omnara** (out of scope; see the note under the table):
  - <https://github.com/omnara-ai/omnara> (README; licence Apache-2.0 per the repository)
  - <https://www.omnara.com/>
  - <https://docs.omnara.com/llms.txt> (documentation index)

Not included: tools that manage several agents on one machine without a phone
client, since this page is about reaching agents from a phone.
