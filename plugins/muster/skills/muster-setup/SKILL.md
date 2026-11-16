---
name: muster-setup
description: Install or set up muster, the self-hosted server that shows the coding-agent sessions running in tmux on your machines (Claude Code, Codex, opencode, Gemini CLI) in your browser and on your phone. Use when the person asks to set up, install, reinstall or update muster, to add another machine to an existing muster, or to watch or answer their Claude Code sessions from their phone.
---

# Set up muster

muster's installation guide is written for a coding agent to follow, and it is
the only source of truth for the steps. **Do not install muster from memory or
from this file.** Download the guide as a file, with `curl`, and follow that
file; do not have a fetch tool read the page to you, because a summary of it
is not the guide:

`curl -fsSL -o muster-install.md https://github.com/ZacxDev/muster/releases/latest/download/INSTALL.md`

That is the newest release's own copy, so it installs the release it came with.

- Read the whole file first, including its "How to follow this guide (agent: read this first)" section.
- Follow it step by step, exactly as written. It decides which machine is the
  hub, what to ask the person, and how to verify the result.
- Ask the person every question the guide's "Ask the person, once" section
  lists in one message, up front, then do the rest without stopping unless a
  step says to ask.
- If the person says muster already runs on another machine, this machine
  joins it: the guide's "Install muster on this machine" section and its
  "Another machine" section cover that case.
- Finish with the guide's "Check it" section (`muster doctor`). The install is
  done only when the guide's own definition of done there (the sentence that
  starts "Done means") is met, including what the person must confirm on their
  phone; do not report success on anything less. If you are stuck, stop and
  tell the person exactly which step and command failed and what it printed,
  as the guide says.

The guide's own rules apply throughout, among them: never print a secret, ask
for `sudo` once up front, and follow each step's "If this fails" before trying
anything else. Facts the guide does not hold (ports, versions, settings) come
from `muster doctor`, each command's `--help`,
<https://github.com/ZacxDev/muster/blob/main/docs/configuration.md> and the
code at <https://github.com/ZacxDev/muster>, never from memory.

To update or remove an existing install, the same guide has an "Updating and removing" section.
