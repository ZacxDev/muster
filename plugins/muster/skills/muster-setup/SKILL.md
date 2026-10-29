---
name: muster-setup
description: Install or set up muster, the self-hosted server that shows the coding-agent sessions running in tmux on your machines (Claude Code, Codex, opencode, Gemini CLI) on your phone, over Tailscale. Use when the person asks to set up, install, reinstall or update muster, to add another machine to an existing muster, or to watch or answer their Claude Code sessions from their phone.
---

# Set up muster

muster's installation guide is written for a coding agent to follow, and it is
the only source of truth for the steps. **Do not install muster from memory or
from this file.** Fetch the guide and follow it:

<https://raw.githubusercontent.com/ZacxDev/muster/main/INSTALL.md>

1. Read the whole guide first, including its "How to follow this guide" section.
2. Follow it step by step, exactly as written. It decides which machine is the
   hub, what to ask the person, and how to verify the result.
3. Ask the person every question the guide's Step 1 lists in one message, up
   front, then do the rest without stopping unless a step says to ask.
4. If the person says muster already runs on another machine, this machine
   joins it: the guide covers that case.
5. Finish with the guide's verification step (`muster doctor`) and report its
   result. A run is not done until the doctor's checks pass or each failure is
   explained to the person with the fix the doctor printed.

The guide's own rules apply throughout, among them: never print a secret, ask
for `sudo` once up front, and follow each step's "If this fails" before trying
anything else.

To update or remove an existing install, the same guide has an "Updating and
removing" section.
