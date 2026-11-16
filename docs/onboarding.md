# Setting muster up with one prompt

Paste this into the coding agent on the machine that should run muster (Claude
Code, Codex, opencode, Gemini CLI, or any agent that can run shell commands):

```text
Set up muster for me. Download its install guide with `curl -fsSL -o muster-install.md https://github.com/ZacxDev/muster/releases/latest/download/INSTALL.md`, read that whole file and follow it step by step. Ask me everything you need in one go up front, then do the rest yourself. Where the guide and the machine disagree, trust `muster doctor`, each command's `--help` and the code at https://github.com/ZacxDev/muster over memory.
```

The agent downloads [INSTALL.md](../INSTALL.md), a guide written for agents to
execute, from the newest release (each release attaches its own copy, so the
guide matches the release it installs), and does the rest. To connect another
machine later, paste the same prompt into the agent on that machine and tell it
muster is already running.

The prompt holds no facts of its own, only where they live: the guide, the
code, `muster doctor` and each command's `--help`. So it stays right as muster
changes.

**Without an agent:** the same install is one line,
[quickstart-local.md](quickstart-local.md).

## What you get: your own sessions, not copies

muster shows the sessions you already run in tmux, and a message from muster
goes into that same session. What that means, what a dispatched agent is
instead, and how to bring a session you already have into tmux:
[mirroring.md](mirroring.md#mirrored-sessions-and-dispatched-agents). Why a
window opens in the Terminal view only:
[mirroring.md](mirroring.md#why-a-window-opens-in-the-terminal).

## What you end up with

- **The server** on one machine (the hub), in Docker: muster and its Postgres,
  on this machine's loopback interface only, with every secret generated.
- **Your sessions.** `muster-mirror` runs as a user service and shows every
  tmux window as a card. Claude Code, Codex, opencode and Gemini CLI sessions
  started in tmux appear on their own.
- **A check you can re-run.** `muster doctor` tests each part end to end and
  says how to fix what fails ([cli.md](cli.md#muster-doctor)).
- **If you want them: your phone and other machines.** Tailscale's HTTPS on
  your own tailnet (nothing exposed to the internet), muster installed to the
  phone's home screen with notifications, and each other machine's sessions.

What each machine needs is checked by the guide's first step, which asks for
anything missing up front; [features.md](features.md) lists what each optional
feature needs.

## What the agent asks you

Once, at the start: whether muster already runs on another of your machines,
whether you want your phone and other machines (Tailscale), whether muster may
type into this machine's terminals, what needs `sudo`, and which optional
features you want now. The guide's "Ask the person, once" section is the list.

It never shows you a password or token in the chat; it tells you the command
that prints the sign-in password.

## Afterwards

- Start agents in tmux so they appear: `tmux new -A -s work claude` (or
  `codex`, `opencode`, `gemini`). To bring a conversation you already have:
  [mirroring.md](mirroring.md#bring-a-session-you-already-have-into-tmux).
- Re-check everything at any time with the guide's "Check it" section.
- Every setting is in [configuration.md](configuration.md); mirroring is
  explained in [mirroring.md](mirroring.md), dictation in
  [dictation.md](dictation.md), notifications and the installable app in
  [push.md](push.md).
