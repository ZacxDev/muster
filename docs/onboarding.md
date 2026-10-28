# Setting muster up with one prompt

Paste this into the coding agent on the machine that should run muster (Claude
Code, Codex, opencode, Gemini CLI, or any agent that can run shell commands):

```text
Set up muster for me: read https://raw.githubusercontent.com/ZacxDev/muster/main/INSTALL.md and follow it step by step. Ask me everything you need in one go up front, then do the rest yourself.
```

The agent reads [INSTALL.md](../INSTALL.md), a guide written for agents to
execute, and does the rest. To connect another machine later, paste the same
prompt into the agent on that machine and tell it muster is already running.

## What you get: your own sessions, not copies

muster shows the sessions you already run in tmux, and a message from your
phone goes into that same session. What that means, what a dispatched agent is
instead, and how to bring a session you already have into tmux:
[mirroring.md](mirroring.md#mirrored-sessions-and-dispatched-agents). Why a
window opens in the Terminal view only:
[mirroring.md](mirroring.md#why-a-window-opens-in-the-terminal).

## What you end up with

- **The server** on one machine (the hub), in Docker: muster and its Postgres,
  listening on `127.0.0.1:8105` only.
- **HTTPS on your tailnet.** `tailscale serve` gives the hub a real certificate
  for its Tailscale name. Only devices signed in to your tailnet can reach it;
  nothing is exposed to the internet.
- **Your sessions, from every machine.** `muster-mirror` runs as a user service
  on each machine and shows every tmux window as a card. Claude Code, Codex,
  opencode and Gemini CLI sessions started in tmux appear on their own.
- **Your phone.** muster installed to the home screen, with notifications and a
  microphone on every message box.
- **A check you can re-run.** `muster doctor` tests each part end to end and
  says how to fix what fails ([cli.md](cli.md#muster-doctor)).

## What you need

- A [Tailscale](https://tailscale.com) account, with **MagicDNS** and **HTTPS
  Certificates** turned on (the agent tells you where if they are off), and the
  Tailscale app on your phone.
- On the hub: Docker with Compose v2, and about 10 GB of free disk (20 GB for
  local dictation).
- On every machine: tmux. Linux or macOS, on amd64 or arm64. macOS hosts are experimental: there, only a Claude Code window whose Rendered view resolves can be viewed. The Terminal view cannot attach on macOS yet ([`pty_other.go`](../internal/mirrorhost/pty_other.go)), so shells, Codex and any window without a Rendered view cannot be viewed, and Rendered-view detection is unverified on a real Mac.

## What the agent asks you

Once, at the start:

1. Whether muster already runs on another of your machines (if so, its
   address, and this machine joins it).
2. **How dictation should work.** On a hub with an NVIDIA GPU (6 GB or more),
   the recommended choice is the local Cohere transcribe model: the most
   accurate, and audio never leaves the hub. It needs a Hugging Face token from
   an account that accepted the model's licence. Without a GPU: Whisper on the
   CPU (local, slower), or your own OpenAI-compatible API key (no hardware;
   recordings go to that provider).
3. **Whether muster may type into this machine's terminals.** Yes lets you
   answer your agents from your phone; it also means anyone signed in to your
   muster can type into any tmux window
   there. No keeps the machine read-only.

Anything else (installing a missing package, a `sudo` step) it asks about when
it gets there.

It never shows you a password or token in the chat. The sign-in password stays
in the hub's `~/muster/.env`, and the agent tells you the command that prints
it.

## Afterwards

- Start agents in tmux so they appear: `tmux new -A -s work claude` (or
  `codex`, `opencode`, `gemini`). To bring a conversation you already have:
  [mirroring.md](mirroring.md#bring-a-session-you-already-have-into-tmux).
- Re-check everything at any time with `muster doctor --deploy-env ~/muster/.env`
  on the hub.
- Every setting is in [configuration.md](configuration.md); mirroring is
  explained in [mirroring.md](mirroring.md), dictation in
  [dictation.md](dictation.md), notifications and the installable app in
  [push.md](push.md).
