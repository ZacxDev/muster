# Glossary

The words muster's documents use, one or two lines each. Each entry links to
the page that owns the subject; that page, and the code, win over this one.

**Agent ask tool.** The `attention_ask` and `attention_wait` tools a mirrored
Claude Code session uses to ask you a question and wait for the answer, served
by the machine's own `muster-mirror`. [features.md](features.md#agent-ask-tool).

**Ask.** A question an agent puts to you. Asks wait on the Asks page and are
pushed with their text. [attention.md](attention.md).

**Ask card.** Claude Code's own multiple-choice question (`AskUserQuestion`),
drawn as a form you answer from muster; needs a hook on each machine.
[features.md](features.md#ask-cards).

**Ask log.** The record of every ask, its answer and how long it took.
[asks.md](asks.md).

**Asks page (the attention queue).** Everything waiting on you, in one list:
asks, waiting and stalled sessions, reply drafts. [attention.md](attention.md).

**Chief.** An optional fleet agent you chat with: it knows what every session is
working on and can route a request or a task to the session already on it.
[chief.md](chief.md).

**Compose profile.** A named group of optional services in
`deploy/docker-compose.yml` (`stt`, `stt-cohere`, `s3`, `chief`), started only
when `COMPOSE_PROFILES` in `.env` names it. [features.md](features.md).

**Connector.** A credential stored, sealed, in muster, which an agent uses
through muster's proxy without seeing it. [connectors.md](connectors.md).

**Core.** What runs with no optional dependency: the Compose stack, and
`muster-mirror` and tmux on each machine. [features.md](features.md#core).

**Dispatched agent.** An agent muster itself starts, in its own Kubernetes pod;
the opposite of a mirrored session. [provisioner.md](provisioner.md),
[mirroring.md](mirroring.md#mirrored-sessions-and-dispatched-agents).

**Episode.** One stop of a session (waiting on you, or stalled), pushed once.
[features.md](features.md#session-state).

**Features page.** `/account/features`: every optional feature as on, off or
misconfigured, as `muster doctor` lists them. [features.md](features.md).

**Focus.** A short title and summary of what each session is doing now, made by
a model on an OpenRouter key. [focus.md](focus.md).

**GoTrue.** Supabase's auth server, which muster can use for per-user accounts
instead of one shared password. [auth.md](auth.md#gotrue).

**Host (machine).** A machine whose `muster-mirror` is connected to the hub; its
tmux windows are your mirrored sessions. [mirroring.md](mirroring.md#how-it-connects).

**Hook token.** `MUSTER_HOOK_TOKEN`, the machine credential the CLI and hooks
present; it acts as the admin. [features.md](features.md#machine-token).

**Hub.** The machine that runs the muster server (in Docker Compose, or the
cluster on Kubernetes). Every other machine connects to it.
[INSTALL.md](../INSTALL.md).

**Interactive.** A host whose `muster-mirror` lets muster type into its tmux
windows (`-interactive`); otherwise it is read-only.
[mirroring.md](mirroring.md#read-only-interactive-and-sending).

**Mirror token.** A host's own credential, minted on the hub, that lets its
`muster-mirror` connect and nothing else. [auth.md](auth.md#machine-credentials).

**`muster doctor`.** The CLI's end-to-end check of an install; it says how to fix
what fails and lists every feature's state. [cli.md](cli.md#muster-doctor).

**`muster-mirror`.** The program on each host that dials out to the hub and
mirrors its tmux windows. [mirroring.md](mirroring.md).

**Mirrored session (mirrored window).** A tmux window on one of your hosts,
shown in muster; a message from muster is typed into that same window.
[mirroring.md](mirroring.md#mirrored-sessions-and-dispatched-agents).

**Officer.** A standing Claude Code agent with a job description and a queue,
woken when work arrives. [officers.md](officers.md),
[officers-builtin.md](officers-builtin.md).

**Page.** A small page an agent publishes for you (a showcase, a form, uploads),
shareable by a link you approve. [pages.md](pages.md).

**Plugin.** An out-of-process program that brings another system's messages
(an inbox, ClankMail) to muster, or, as a viewer, serves its own origin to you.
[plugins.md](plugins.md).

**Preview.** A web server an agent runs on a host, served to you through the
preview gateway. [previews.md](previews.md).

**PWA (the installable app).** muster installed to a phone's home screen from
the browser; on iOS it is how notifications work.
[push.md](push.md#installable-app-pwa).

**Rendered view.** A Claude Code window drawn as a chat with a message box,
read from its transcript. [mirroring.md](mirroring.md#rendered-and-terminal-views).

**Server switcher.** Moving between several muster servers in one browser.
[push.md](push.md#several-servers-the-server-switcher).

**Step-up ("Confirm it's you").** The fresh sign-in muster asks for before you
type into a session. [sending-and-step-up.md](sending-and-step-up.md#step-up-confirm-its-you).

**Team.** A group of people on one server sharing sessions, officers, tasks and
a chief. [auth.md](auth.md#teams).

**Team host.** A host connected with a team's mirror token, whose sessions the
team's members see. [mirroring.md](mirroring.md#team-hosts).

**Tenant.** One of several muster servers a single machine's sessions go to,
one tmux server each. [multi-tenant-hosts.md](multi-tenant-hosts.md).

**Terminal view.** Any tmux window as a live terminal you can read and, on an
interactive host, type into. [mirroring.md](mirroring.md#rendered-and-terminal-views).

**VAPID.** The key pair that identifies muster to the browsers' push services;
the Compose stack generates it. [push.md](push.md#web-push).
