# Configuration

Every muster binary is configured through environment variables. This page lists
**every variable any Go code in this repository reads**, grouped by the binary
that reads it.

The list is enforced. `TestEveryEnvVarTheCodeReadsIsDocumentedAndViceVersa`
(in `internal/modulegate/envdoc_test.go`) parses the module's Go source, collects
every variable name it reads, and compares that set with the first column of the
tables below. It fails if the code reads a variable that has no row here, and if
a row names a variable that nothing reads.

What that test covers:

- All non-test `.go` files in the module, including build-tagged test fixtures.
  Those fixture variables are listed under [Test and e2e harness](#test-and-e2e-harness).
- Variables read in `_test.go` files are **not** included. They configure tests,
  not anything that ships. The Makefile's `test-liveenv` target documents its own.
- Non-Go code is **not** included: the Playwright harness's `MUSTER_E2E_*`
  variables (`e2e/lib/env.ts`), the Dockerfile and the Makefile.

How values are parsed:

- **Booleans.** In `muster-server`, a boolean is on only for `1`, `true`, `yes`
  or `on` (case-insensitive). Anything else, including a typo, is off. In
  `muster-mirror`, a boolean is on for `1` or `true`.
- **Durations** use Go syntax: `90s`, `30m`, `12h`.
- **Keys** are 32 random bytes, base64-encoded. One way to make one:
  `head -c32 /dev/urandom | base64`.

The server checks its configuration at boot and refuses to start on an invalid
or contradictory combination. After it starts, it logs one line per feature
saying whether that feature is on, off or degraded, and why.

## muster-server: core

| variable | default | required | what it does |
|---|---|---|---|
| `DATABASE_URL` | none | yes | The Postgres DSN. Migrations run at boot. `muster-migrate` and the `muster-server mirror token …` subcommands read it too. |
| `MUSTER_PORT` | `8105` | no | The main listener. |
| `MUSTER_PUBLIC_URL` | none | for invites and passkeys | The external base URL. It is used to build links (invites, the admin claim), and its host is accepted as muster's own origin. |
| `MUSTER_STANDALONE` | off | one of this or the router | Declares that there is no permission router. If neither this nor a router is configured, `/readyz` answers 503 and names the missing setting. |
| `MUSTER_TASK_REAP_AFTER` | `168h` | no | How long a task can sit idle before the reaper tags it `stale`. The reaper runs every 30 minutes. |
| `MUSTER_AGENT_REAP_STOPPED_AFTER` | `336h` | no | How long an agent's stored status can stay `stopped` before the [stopped-agent reaper](provisioner.md#reaping-stopped-agents) destroys it (the agent page's Delete teardown). Never the chief, an agent in any other status (`error` included), a pool worker, or an agent saved for later and never started. A Go duration of at least `1h`; `0` or `off` turns the reaper off. Swept every 30 minutes with the idle-task reaper. |
| `MUSTER_SEND_QUEUE_IDLE` | `30s` | no | How long a mirrored Claude Code session must have been idle (by Claude Code's own `statusUpdatedAt`) before the next queued message is sent to it. Range: `1s`–`1h`; with `MUSTER_SESSION_ROUTING=1`, under `30m`, a [routed message](plugins.md#routing-a-message-to-a-session)'s deadline (it is pasted only after this wait), or it refuses to boot. A route muster makes for a pull-request review (`MUSTER_PR_MAIL_SENDERS`) has a `4h` deadline instead; both are fixed, not settings. |
| `MUSTER_FOCUS_INTERVAL` | `15m` | no | How often the server recomputes each session's [focus](focus.md) with the summarizer. `0` turns the periodic pass off, and with it every refresh. Range otherwise: `1m`–`24h`. |
| `MUSTER_AGENTS_IDLE_AFTER` | `14d` | no | How long a session (a mirrored window, or a dispatched agent) must have had no activity before the sidebar draws it under the collapsed **Idle (N)** group at the bottom of its list (never one waiting on you, the chief, or the page you are on). "Activity" is the time the sidebar orders by and its recency dot shows: a Claude Code window's last message (its transcript, as its host reports it; tmux's activity from an older host), any other window's last activity as its host reports it, an agent's newest transcript message or row update. Pinned entries never collapse; nothing on a host is closed. Whole days (`14d`) or a Go duration (`336h`); `0` or `off` turns the group off. Range otherwise: `1h`–`365d`. The boot banner prints the value in force. |
| `MUSTER_FOCUS_RETENTION` | `168h` | no | How long a mirrored session's [focus](focus.md) is kept after it was last seen on any host. An hourly sweep deletes it, whether or not the refresh pass runs. A Claude session id is never reused, so a week unseen is a finished conversation (and still outlasts a laptop closed over a long weekend). Range: `1h`–`2160h`. |
| `MUSTER_SUGGEST` | on | no | [Suggested replies](suggested-replies.md). `off` is the kill switch: nothing is generated, no call is made, and the composer is told they are off. Unset or `on` is on; each user can still turn them off for themselves. Anything else refuses to boot. |
| `MUSTER_SUGGEST_MODEL` | `openai/gpt-6-luna` | no | The OpenRouter model suggestions are generated with, spelled as OpenRouter's model list names it. It is called with the session owner's OpenRouter key, as focus is. |
| `MUSTER_TAG_AUTODISPATCH` | off | no | Reserved. When on, eligible tasks are logged and counted in a metric. Nothing is dispatched. |

## muster-server: sign-in and machine credentials

See [auth.md](auth.md).

| variable | default | required | what it does |
|---|---|---|---|
| `MUSTER_AUTH_MODE` | `password` | no | `password` means one shared operator password, acting as user 1. `gotrue` means per-user accounts through GoTrue. |
| `MUSTER_UI_PASSWORD` | none | in password mode | The operator password. If it is unset or shorter than 12 characters, every browser page answers 503. This fails closed. |
| `MUSTER_SECURE_COOKIES` | off | behind TLS | Marks the password-mode session cookie `Secure`. The cookies used in gotrue mode are always `Secure`. |
| `MUSTER_GOTRUE_URL` | none | in gotrue mode | The base URL of muster's GoTrue. Set it together with the secret, or set neither. |
| `MUSTER_GOTRUE_JWT_SECRET` | none | in gotrue mode | The HS256 secret GoTrue signs with. At least 32 characters. |
| `MUSTER_BROWSER_PORT` | none | in gotrue mode | A second listener, the only one that serves browser sessions in gotrue mode. On that mode's browser listener, the shared tokens below are refused with 403. |
| `MUSTER_HOOK_TOKEN` | none (machine routes open) | in gotrue mode | A shared machine secret that acts as user 1, the admin. It is enforced only when set. When set, it must be at least 12 characters and not the `.env.example` placeholder `CHANGE-ME`; otherwise muster refuses to boot. |
| `MUSTER_SERVICE_TOKEN` | none (routes answer 503) | no | A service-to-service secret that acts as user 1. Requests must also send an `X-Muster-Actor` header. At least 32 characters. |
| `MUSTER_STEPUP_WINDOW` | `12h` | no | How recent a sign-in must be before you can type into a mirrored session. Range: `1m`–`24h`. |
| `MUSTER_LIVE_HEARTBEAT` | `15s` | no | How often every live socket and event stream sends a heartbeat. A page reconnects a stream that stays silent for about two of them. Range: `250ms`–`5m`. |

## muster-server: optional features

| variable | default | required | what it does |
|---|---|---|---|
| `MUSTER_MIRROR` | off | no | Turns on [mirrored sessions](mirroring.md). |
| `MUSTER_PLUGINS` | off | no | Turns on [plugins](plugins.md): owners mint plugin tokens on `/plugins`, and plugins post messages that reach the owner's chief agent. Needs a database. Off, every `/plugin/*` route and `/plugins` answer 404. |
| `MUSTER_PLUGIN_TASK_NAMESPACES` | none | no | Comma-separated tag namespaces a plugin may file tasks by, through [`POST /plugin/v1/tasks`](plugins.md#post-pluginv1tasks-one-task-per-upstream-record). Unset, that route answers 503. A namespace muster already uses (`gate`, `auto`, `initiative`, `project`, `superseded-by`) refuses to boot. |
| `MUSTER_SESSION_ROUTING` | off | no | `1` turns on [session routing](plugins.md#routing-a-message-to-a-session): the chief may hand a plugin message to one of its owner's sessions, and the session's answer becomes the reply. Needs `MUSTER_PLUGINS=1`. |
| `MUSTER_TASK_ROUTING` | off | no | `1` turns on [task routing](plugins.md#routing-a-task-to-the-chief): a plugin filing a task with `route_to_chief` hands its notice on by kind (an assignment to the owner's chief; an update to the chief only while a live agent works the task; a mention to that agent or the attention queue), which puts it in the attention queue (`task_route_attention`) or forwards it to the agent working it (`task_forward`). Needs `MUSTER_PLUGINS=1`. Off, `route_to_chief` is ignored and those tools are not offered. |
| `MUSTER_CHIEF_DISPATCH_OWNERS` | none | no | The user ids, comma-separated, who opted in to their chief starting agents. Non-empty (with `MUSTER_TASK_ROUTING=1`) offers the chief `task_dispatch`: start an agent on an open, ungated task, at most 5 of them live and 10 a day, once per task (again after you destroy the agent it started), for the listed owners only. Unset, `task_dispatch` is not offered. A value that is not a list of positive integers refuses to boot. |
| `MUSTER_CHIEF_SESSION_RUNTIMES` | none | no | The runtimes a [chief](chief.md#sessions-on-more-than-one-runtime) chat session may be started on, comma-separated. The one runtime is `claude-code` (a Claude Code CLI of its own, with the Claude model picked for it; needs `claude-code` in `MUSTER_AGENT_KINDS` and a claude-code chief). Unset, there is no picker and every session is on the chief's own runtime. It does not turn the [default session](chief.md#sessions-on-more-than-one-runtime) on or off: plugin deliveries and `chief ask` go to the chief's default session either way. `gateway`, a runtime that has been removed, is ignored with one warning at boot. Any other unknown or repeated entry, or `claude-code` without that kind, refuses to boot. |
| `MUSTER_ROUTE_RELAY_ALLOW` | none | no | The senders whose routed answers are sent **without** the owner's approval, per user and per plugin: `<user id>:<plugin name>:<address>` or `<user id>:<plugin name>:*@<domain>`, or for agent mail `<user id>:<plugin name>:<owner>/<handle>` or `<user id>:<plugin name>:<owner>/*`, comma-separated, ASCII addresses only ([grammar](plugins.md#routing-a-message-to-a-session)). An entry matches only a message that plugin delivered **and** marked `sender_verified` ([plugins](plugins.md#sender_verified)). Everyone else's answer waits for approval like any reply draft. A value that does not parse (including the older `<user id>:<address>` form) refuses to boot. |
| `MUSTER_PR_MAIL_SENDERS` | none | no | The agent-mail senders, `<owner>/<handle>` comma-separated, whose mail is read as a **pull-request review** and routed to the mirrored session that created or pushed that pull request ([plugins](plugins.md#a-pull-request-review-mail)). Only a sender the agent-mail plugin marked `sender_verified` matches. Such a route's deadline is 4 hours, not the 30 minutes of every other route (fixed, not a setting). Needs `MUSTER_SESSION_ROUTING=1`. Who authored what is reported by the host's PostToolUse hook to `POST /api/pr-sessions`. Unset: off, and that route answers 404. A value that does not parse refuses to boot. |
| `MUSTER_MAIL_RESUME_ALLOW` | none | no | The senders whose agent mail may **resume an ended session**: mail muster would route itself (a reply on a thread, a pull-request review) to a session no window shows asks that session's host to start `claude --resume` in a new tmux window, then routes the message there ([plugins](plugins.md#resuming-an-ended-session)). `MUSTER_ROUTE_RELAY_ALLOW`'s grammar: `<user id>:<plugin name>:<owner>/<handle>` or `<user id>:<plugin name>:<owner>/*`, comma-separated; only a sender the plugin marked `sender_verified` matches. Needs `MUSTER_SESSION_ROUTING=1` and a host running `muster-mirror -resume-spawn`. Unset: off, and such mail goes to the chief. A value that does not parse refuses to boot. |
| `MUSTER_MAIL_RESUME_OFF` | off | no | `1`: the mail resume's kill switch. Nothing is resumed whatever `MUSTER_MAIL_RESUME_ALLOW` lists; the banner says so. |
| `MUSTER_STT_URL` | none | with the token | The base URL (`http(s)://host[:port]`) of the speech-to-text service used for [dictation](dictation.md). |
| `MUSTER_STT_TOKEN` | none | no | The bearer token for that service. Setting it turns dictation on. A token without a URL refuses to boot. |
| `MUSTER_STT_MODEL` | none | for OpenAI and whisper servers | The `model` form field sent with each recording, e.g. `whisper-1` for OpenAI or `Systran/faster-whisper-small` for a faster-whisper server. Unset sends no `model` field, which is what the bundled stt-cohere server expects. |
| `MUSTER_S3_ENDPOINT` | none | for [file attachments](uploads.md) | The S3-compatible store attached files are kept in, as `http(s)://host[:port]` (path-style; MinIO works). Attachments are on only when this, `MUSTER_S3_BUCKET`, both keys, `MUSTER_UPLOAD_SIGNING_KEY`, `MUSTER_PUBLIC_URL` and a database are all set. A partial or invalid set leaves them **off**, says why in the boot banner, and never refuses to boot. |
| `MUSTER_S3_BUCKET` | none | for file attachments | The bucket. Objects are written under `uploads/<random id>`. |
| `MUSTER_S3_ACCESS_KEY` | none | for file attachments | An access key that may `PutObject`, `GetObject`, `DeleteObject` and `AbortMultipartUpload` under `uploads/` in that bucket, plus `ListBucket`, `ListBucketMultipartUploads` and `GetLifecycleConfiguration` on the bucket ([policy](uploads.md#configuration)). |
| `MUSTER_S3_SECRET_KEY` | none | for file attachments | Its secret key. |
| `MUSTER_S3_REGION` | `us-east-1` | no | The signing region. |
| `MUSTER_UPLOAD_SIGNING_KEY` | none | for file attachments | At least 32 bytes. The key the links handed to agents are signed with (HMAC-SHA256). Rotating it ends every link already given out. |
| `MUSTER_UPLOAD_MAX_BYTES` | `26214400` (25 MiB) | no | The largest file that can be attached, in bytes (1 to 104857600). Files are streamed to the store, so this bounds the bucket, not memory. |
| `MUSTER_UPLOAD_LINK_TTL` | `24h` | no | How long a link handed to an agent works (a Go duration, `1m`–`168h`). |
| `MUSTER_VAPID_PUBLIC_KEY` | none | no | [Web push](push.md). The three VAPID variables are all-or-none. Generate a keypair with `muster-server vapid generate`. |
| `MUSTER_VAPID_PRIVATE_KEY` | none | no | The private half of that keypair. It must match the public key. |
| `MUSTER_VAPID_SUBJECT` | none | no | A `mailto:` or `https:` contact address that is sent to push services. |
| `MUSTER_ANDROID_ALLOW_PIN_KEYS` | off | no | `1` lets a phone with no strong biometric pair the [Android app](design/android.md#the-step-up-key) with a biometric-or-PIN step-up key. Off, such a phone pairs read-only. A biometric-or-PIN key survives a new fingerprint being enrolled, so anyone who knows the PIN can use it. |
| `MUSTER_ANDROID_PACKAGE` | none | with the certificates | The [Android app](design/android.md)'s package name, for `/.well-known/assetlinks.json`, which lets the app use this site's passkeys ([auth.md](auth.md#passkeys-in-the-android-app)). Set both Android variables or neither: one alone, or a malformed value, stops the server from starting. Unset, that path is 404. |
| `MUSTER_ANDROID_CERT_SHA256` | none | with the package | The SHA-256 fingerprints of the app's signing certificates, separated by commas: 64 hex digits each, colons optional, any case. They are served as `AA:BB:…`. |
| `MUSTER_PUSH_EXTRA_HOSTS` | none | no | Your own push hosts, such as a self-hosted ntfy for the [Android app](design/android.md). Paired phones may register on them; browsers may not. Exact host names, separated by commas or spaces: no scheme, port, path or wildcard. Each is reached on `https` and the default port only. A bad entry stops the server from starting. See [push.md](push.md#which-push-services). |
| `MUSTER_MEMO_URL` | none | with the token | The base URL of the cairn memo listener, for the [memo relay](memo-relay.md). |
| `MUSTER_MEMO_TOKEN` | none | no | A read-only cairn credential. The relay runs only while it is set, and also needs a database and an agent gateway. |
| `MUSTER_MEMO_SCOPE_MAP` | none | no | `repo=scope,scope;…`. Use it when a repository's memos are filed under other scope names. |
| `MUSTER_FLOW_AGENT_DENY_WORDS` | none (logs a warning) | no | Comma-separated names (your hosts, clusters or projects) that an agent may not put into [browser-flow](browser-flows.md) text, or into a [plugin](plugins.md) reply draft. |
| `MUSTER_CONNECTOR_ENCRYPTION_KEY` | none | no | A 32-byte base64 key. Setting it turns on the [connector store](connectors.md), and with it members' own stored credentials. |
| `MUSTER_CONNECT_PORT` | none | with connectors | A listener that serves only the agent-facing connector, approval and MCP routes. |
| `MUSTER_AGENT_CONNECT_URL` | none | with connectors | The URL agents are given (as `MUSTER_CONNECT_URL`) for reaching the connect port. |
| `MUSTER_GITHUB_APP_ID` | none | with the key and the accounts | The numeric App ID of a [GitHub App](github-app.md). Setting it, the key and the accounts makes the App the GitHub identity of muster's **admins**: their repository picker and connected-repository checks list its installation repositories, and the `github` connector gives each approved agent of theirs a 1-hour token limited to its own repository (other repositories are reached anonymously; an agent with no repository gets no github access). Members have no GitHub identity (no connected repositories). It needs `MUSTER_CONNECTOR_ENCRYPTION_KEY`. |
| `MUSTER_GITHUB_APP_PRIVATE_KEY` | none | with the ID | The App's private key: the PEM file GitHub generated, as the value (mount it from a Secret). A key that does not parse refuses to boot. |
| `MUSTER_GITHUB_APP_ACCESS` | `write` | no | What every agent token the GitHub App mints may do: `write` (Contents and Pull requests read and write, Checks and Commit statuses read) or `read` (all four read; a push or pull-request write through the proxy is refused by GitHub). See [Read-only mode](github-app.md#read-only-mode). Any other value, or setting it without the App, refuses to boot. |
| `MUSTER_GITHUB_APP_ACCOUNTS` | none | with the App | Comma-separated `<login>:<id>` entries, one per GitHub account (user or organization), e.g. `example-org:12345,your-user:67890`. Find an id with `gh api users/<login> -q .id`. Matching uses the **numeric id**, because a login can be renamed and then registered by someone else; the login is a label. An installation whose id matches under another login (a rename) is still used and logged. One whose login matches but whose id does not is ignored. Only the App's installations on these accounts are used; any other installation (anyone can install a public App) is ignored and logged once. Empty, an entry with no id, or an id listed twice refuses to boot. Only User and Organization accounts match (an Enterprise installation's id is from another numbering space). |
| `MUSTER_CHECKOUT_WAIT_TIMEOUT` | `24h` | no | How long a claude-code agent's first turn waits for its repository checkout (the owner's `github` approval) before the agent is marked error. A Go duration, `1h`–`720h`. It is measured from the agent's last status change, so raising it also extends agents already waiting. |
| `MUSTER_FARO_URL` | none | no | A Grafana Faro collector URL. Setting it turns on front-end real-user monitoring. |
| `MUSTER_FARO_API_KEY` | none | no | The Faro API key. It is sent to the browser, so it is public. |
| `MUSTER_FARO_TRACING` | off | no | Turns on Faro tracing as well: the page's same-origin requests carry a `traceparent`, and each session open is a span joined to the server's trace ([telemetry.md](telemetry.md)). |
| `MUSTER_FARO_UPSTREAM` | none | no | The Faro receiver's collect URL (for example an Alloy `faro.receiver`). Setting it turns front-end monitoring on through muster's own relay: the page posts to `POST /telemetry/faro` on muster's origin, and muster forwards each batch here, adding `MUSTER_FARO_API_KEY` itself. Signed-in users only, 256 KiB per batch, 120 batches a minute per browser tab and 600 per user, `Retry-After: 5` past that. It takes precedence over `MUSTER_FARO_URL`. Not an `http(s)` URL refuses to boot. See [telemetry.md](telemetry.md). |
| `MUSTER_HOST_ICONS` | none | no | Comma-separated `<host>=<icon>` entries giving a mirrored host its icon by name, whatever order its token was minted in (e.g. `laptop=laptop,tower=server`). The icons: `laptop`, `server`, `terminal`, `chip`, `phone`, `home`, `globe`, `box`, `bolt`, `star`, `moon`, `drive`. No other host draws an icon named here. A name that is not a host name (lower-case letters, digits and inner hyphens), `cluster`, an unknown icon, or one icon for two hosts refuses to boot. |

## muster-server: permission router and approvals

muster can also run beside a separate permission router: a service that records
Claude Code sessions and routes their permission prompts. That router is not
part of this repository. Without one, set `MUSTER_STANDALONE=1`.

| variable | default | required | what it does |
|---|---|---|---|
| `MUSTER_ROUTER_URL` | none | no | The router's base URL. It provides transcript links, notifications and the directory picker. It refuses to boot together with `MUSTER_STANDALONE`. |
| `MUSTER_ROUTER_TOKEN` | none | with the URL | The router's credential. |
| `MUSTER_ROUTER_ACTOR` | `muster` | no | The actor name muster sends to the router. |
| `MUSTER_ROUTER_ACTOR_HEADER` | `X-Router-Actor` | no | The header name the actor is sent in, to the router and to the approval gate. Set it to whatever header your router reads. |
| `MUSTER_LEGACY_PROVENANCE_HEADER_PREFIX` | none | no | For installed clients that still send the task-provenance headers under an older prefix: muster then also reads `<prefix>-Source`, `<prefix>-Session-Id` and `<prefix>-Host` (the `X-Muster-*` header wins when both are sent). Unset, only `X-Muster-*` is read. A value that is not a header-name prefix is refused at boot. |
| `MUSTER_ROUTER_TOKEN_HEADER` | `X-Router-Token` | no | The header name the token is also sent in, beside `Authorization: Bearer`, to the router and to the approval gate. |
| `MUSTER_APPROVAL_GATE_URL` | none | no | The router's gate endpoint. Agents' permission prompts are relayed there. Unset, a prompt falls back to the agent's own terminal. |
| `MUSTER_APPROVAL_GATE_TOKEN` | none | with the URL | The credential for that gate. |

## muster-server: agent provisioning

Provisioning is off by default. See [provisioner.md](provisioner.md) for what
each setting does and which combinations are valid.

| variable | default | required | what it does |
|---|---|---|---|
| `MUSTER_AGENT_PROVISIONER` | `none` | to dispatch | `none`, `noop` (records specs and creates nothing), or `kubernetes` (uses in-cluster credentials). |
| `MUSTER_AGENT_GATEWAY` | `none` | to chat / kick off | `hooks-sha256`. Turns on agent chat and first-turn delivery. |
| `MUSTER_AGENT_GATEWAY_MODEL` | none | with a gateway | The value the agent runtime expects in the wire `model` field. |
| `MUSTER_AGENT_GATEWAY_PORT` | `18789` | no | The port the agent's gateway listens on. Each instance's Service declares it. |
| `MUSTER_AGENT_RUNTIME_CONFIG` | none | with a gateway | The agent runtime's config file contents. |
| `MUSTER_AGENT_RUNTIME_INSTALL` | none | with a gateway | The script that installs that config file in the instance. |
| `MUSTER_AGENT_IMAGE_REPO` | none | with a provisioner | The image repository for `gateway`-kind agents. |
| `MUSTER_AGENT_IMAGE_TAG` | `latest` | no | The tag for that image. |
| `MUSTER_AGENT_API_URL` | none | with a provisioner | The muster URL agents use to call back. |
| `MUSTER_AGENT_MODEL` | none (the runtime decides) | no | The default model for agents. |
| `MUSTER_AGENT_OPENROUTER_KEY` | none | no | The admin's OpenRouter key for agents. Members bring their own keys. |
| `MUSTER_AGENT_NAMESPACE_PREFIX` | `devpod-` | no | Each agent's namespace is this prefix plus the agent name. |
| `MUSTER_AGENT_NAMESPACE_SHARED` | off | no | Puts every agent in `MUSTER_AGENT_NAMESPACE` instead of its own namespace. |
| `MUSTER_AGENT_NAMESPACE` | none | when shared | The shared namespace. |
| `MUSTER_AGENT_WORKSPACE_PERSIST` | off | no | Gives each agent a workspace volume that survives restarts. |
| `MUSTER_AGENT_WORKSPACE_STORAGE_CLASS` | the cluster default | no | The StorageClass for that volume. |
| `MUSTER_AGENT_ENDPOINT_TEMPLATE` | the driver's (`{{.Name}}.{{.Group}}.svc` on Kubernetes) | no | How muster builds an agent's address from its name and group. |
| `MUSTER_AGENT_CAIRN_URL` | none | no | Cairn coordinates given to the chief only. Set it together with the token, or set neither. |
| `MUSTER_AGENT_CAIRN_TOKEN` | none | no | The credential for that cairn. |
| `MUSTER_AGENT_KINDS` | `gateway` | no | A comma list of the agent kinds a dispatch may ask for: `gateway`, `claude-code`. Any other entry (`hermes` included: that kind was removed) refuses to boot. |
| `MUSTER_AGENT_CC_IMAGE` | none | with `claude-code` | The pinned image for `claude-code` agents, built from `images/claude-code-agent`. |
| `MUSTER_AGENT_CC_ACCOUNTS` | none | with `claude-code` | A comma list of Claude account names (`[a-z0-9-]`, 1–32 characters each). |
| `MUSTER_AGENT_CC_TOKEN_<NAME>` | none | for each account | That account's `claude setup-token`. `<NAME>` is the account name upper-cased, with `-` written as `_`. |
| `MUSTER_AGENT_CC_STORAGE_SIZE` | `10Gi` | no | The size of each `claude-code` agent's `/data` volume. |
| `MUSTER_AGENT_CC_RATE_LIMIT_COOLDOWN` | `1h` | no | How long a Claude account rests after a `rate_limited` turn whose message states no reset time muster can read (a Go duration, 1m–168h). When the message states one (`resets 9:45pm (UTC)`, `resets Oct 9, 9am (UTC)`), the account rests until then instead. While it rests, the job API places no worker-pool job on it; a new agent may still be given it, ranked last. |
| `MUSTER_AGENT_CC_MAX_CONCURRENT_JOBS` | none (no cap) | no | Opt-in: how many jobs one Claude account runs at once, counted as its workers with a job queued or running, in any pool (1–64). A worker-pool job that would exceed it is skipped; when every worker is skipped for this or a cooldown, the job API answers 429. Unset, there is no per-account cap. |
| `MUSTER_AGENT_CC_BYPASS_POOLS` | none | no | A comma list of worker-pool names whose `claude-code` agents run their **job lanes** with permissions **bypassed**: such an agent's pod gets `CCD_JOB_PERMISSION_MODE=bypassPermissions`, `ccd seed` lifts `disableBypassPermissionsMode` and records the bypass disclaimer's consent, and ccd starts each job lane with `--permission-mode bypassPermissions`. "Job lanes" means **sha jobs only** (a job with a `sha` runs in its own worktree); a pool job without a `sha` runs in the agent's working directory, in an ordinary lane, and still prompts. The agent's main CLI and its chat lanes keep default permissions; an unpooled agent and the chief never get it, and neither does an agent holding an **approved grant on a connector that reaches GitHub** (the `github` connector or a `github.com` host): it is started without bypass and a log line names it (the approval doors already refuse such a grant to every pool worker). Names are lower-cased as read (pool names are lower-case only, so `Pr-Review` names the pool `pr-review`) and must then meet the pool-name rules (1–63 of `[a-z0-9-]`, not starting with `-`). Refused at boot without `claude-code` in `MUSTER_AGENT_KINDS`, and for a name that fails those rules; a repeated name is harmless. This is configuration, applied whenever an agent's spec is applied — dispatch, Start, or a profile reapply (`ReapplyProfiles`) — so a running agent keeps the mode it was started with until then. 🔴 **Leaving bypass is DESTROY AND RECREATE**, not Stop → Start: whatever a bypassed lane wrote to the agent's volume survives (hooks or `statusLine` in user settings, MCP servers in `.claude.json`, the project's local settings file, shell rc files), and de-listing plus Stop → Start does not remove it. 🔴 A listed pool's job lanes run any command on the code they are handed, without approval, and can read anything in the pod — that worker's own Claude setup-token and its `MUSTER_HOOK_TOKEN` (so it can call muster's API as that agent) included; every process in the pod runs as the same user, so treat the whole pod as unapproved. Only for pools that review trusted members' PRs. |
| `MUSTER_AGENT_CC_POOL_DEPLOY_KEYS` | none | no | Worker pools that serve a **private** repository: a comma list of `<pool>=<owner>/<name>:<secret>`, e.g. `private-review=example-org/private-app:private-app-deploy-key`. Each listed pool's workers clone, and its jobs fetch, that ONE repository over SSH (`ssh.github.com:443`, GitHub's pinned host keys) with the private key in the Secret `<secret>`, key `ssh-privatekey` (a `kubernetes.io/ssh-auth` Secret), mounted read-only at `/etc/muster/deploy-key/ssh-privatekey` into each worker's **key holder** only: a second container under its own uid (1001), which clones a mirror the agent's container reads, and fetches each job's commits into it. The operator makes the Secret **in muster's own namespace** (`MUSTER_AGENT_NETPOL_FROM_NAMESPACE`) — ⚠ upgrading from v0.110.0, whose docs put it in each worker's namespace: move it there, and delete every hand-made copy in the worker namespaces, which muster refuses rather than overwrite. On the Kubernetes driver muster copies it — the mounted `ssh-privatekey` and the type only, under muster's ownership labels — into each worker's namespace whenever the worker's spec is applied (dispatch, Start, reapply), so a changed key reaches a worker at its next Stop → Start. A source that is missing, or has no `ssh-privatekey`, fails the dispatch or Start naming the Secret, and no pod is started. muster never changes or deletes the source; the copy goes with the worker's namespace. A listed pool is also **egress-locked** (`MUSTER_AGENT_CC_EGRESS_LOCKED_POOLS`). 🔴 The key must be added to the repository as a deploy key **without write access**: before each job's fetch the holder opens GitHub's receive-pack with it (a forced dry-run push, which sends nothing) and the job is refused if GitHub accepts. A listed pool's jobs get none of the repository's agent configuration (`.claude/`, the root `CLAUDE.md`, `AGENTS.md`, `.mcp.json`), and a job with a `config_ref` on it is refused (409). A worker of a listed pool working on another repository is refused at dispatch/Start, and a job naming another repository fails `checkout_failed`. Pools not listed — every pool, by default — clone and fetch anonymously, unchanged. Pool names are lower-cased as read. Refused at boot without `claude-code` in `MUSTER_AGENT_KINDS`, and for a malformed entry or a pool listed twice. Applied when a worker's spec is applied (dispatch, Start, reapply); a job's fetch follows the configuration at once. The agent's session — ccd, the CLI, every lane's tools — cannot read the key or use it: see [jobs](jobs.md#private-repositories-a-pools-deploy-key) for the guarantee and what remains. |
| `MUSTER_AGENT_CC_EGRESS_LOCKED_POOLS` | none | no | A comma list of worker pools whose workers are **egress-locked**, on top of every pool `MUSTER_AGENT_CC_POOL_DEPLOY_KEYS` lists (locked whether listed or not). A locked worker's NetworkPolicy has **no DNS rule and no public rule** (the officer lock: ingress from muster, and muster's connect port when the connector store is offered, nothing else), its pod carries `muster.agent/pool=<pool>` and `muster.agent/egress=locked`, and it gets `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`. 🔴 **What it may reach is the operator's cluster policy**, which selects `muster.agent/egress=locked` — muster names no host. Without one a locked worker resolves and reaches nothing public, so every turn fails (closed, loud). See [jobs](jobs.md#a-pools-egress-lock) for an example policy. Other pools are unchanged. Names are lower-cased as read and must be valid pool names that are also label values (a locked pool's name is the `muster.agent/pool` value, so it may not end in `-`; the same holds for a deploy-key pool); refused at boot without `claude-code` in `MUSTER_AGENT_KINDS`. Applied when a worker's spec is applied (dispatch, Start, reapply). |
| `MUSTER_AGENT_NETPOL_FROM_NAMESPACE` | none | `claude-code` on Kubernetes | The namespace muster runs in. The agent's NetworkPolicy only admits muster. It is also where a deploy-key pool's Secret is kept and copied from. |
| `MUSTER_AGENT_NETPOL_FROM_POD_LABELS` | none | `claude-code` on Kubernetes | Comma-separated `key=value` labels on muster's pods. |

The install script receives each `gateway`-kind agent's settings as JSON in
`MUSTER_CONFIG`. It is the script's job to copy them into the runtime's own
configuration:

| key | value | the runtime's setting |
|---|---|---|
| `workspace` | always: the agent's workspace (`/data/workspace` by default), where muster puts its `AGENTS.md` | the agent workspace (`agents.defaults.workspace`). Without it the runtime uses a directory under its `HOME` and never reads that `AGENTS.md`. |
| `model` | only when a model is named: `{"primary": …, "fallbacks": […]}` | the default model (`agents.defaults.model`) |

For example, in a `jq` filter that reads `MUSTER_CONFIG` as `$mc`:
`| if ($mc[0].workspace // null) then .agents.defaults.workspace = $mc[0].workspace else . end`.

## muster-server: officers

The officer runtime ([officers.md](officers.md)). Off unless `MUSTER_OFFICERS`
declares an officer; then it needs the `claude-code` kind and an agent
provisioner, and the server refuses to boot without them.

| variable | default | required | what it does |
|---|---|---|---|
| `MUSTER_OFFICERS` | none | no | A JSON array of declared officers: `id` (`<type>:<area>`), `hq_url` (HQ's API base, with `/api/v1`), and optionally `max_instances` (1–8, default 1), `idle_scale_to_zero_after` (default `30m`), `poll_interval` (default `1m`, at least `10s`), `owner` (the muster user id the instance agents belong to, default 1), `cc_account` (pin the Claude account), `model` (the instances' Claude Code model: `opus`, `sonnet` or a model id; unset = the account default), `ship` (a shipping officer's policy: push to its own branches, merge its own pull requests and tag releases through muster's gates — [officers.md](officers.md#shipping-officers)) and `repo` (`owner/name`: the repository the instances may read through the GitHub App's `github` connector — the App mints for exactly that repository; not checked out; an officer's App token is always read-only, whatever `MUSTER_GITHUB_APP_ACCESS` says, except a shipping officer's, which is its own App's behind muster's push gate; a grant is re-opened when `repo` changes; see [officers.md](officers.md#reading-a-repository-repo)). Instances always run on `claude-code`, and officers need the connector door (`MUSTER_AGENT_CONNECT_URL` with `MUSTER_CONNECT_PORT` and the connector store's key) — the server refuses to boot without it. Unknown fields are refused, as is an id whose instance names would not fit a namespace under `MUSTER_AGENT_NAMESPACE_PREFIX`, and a `repo` that is not `owner/name`. See [officers.md](officers.md#configuration). |
| `MUSTER_OFFICER_GITHUB_APP_KEY_<NAME>` | none | for each officer with `ship` | The PEM private key of that shipping officer's OWN GitHub App (`ship.github_app`), never the deployment's. Read by the server only. See [officers.md](officers.md#shipping-officers). |
| `MUSTER_OFFICER_ESCALATE_WEBHOOK_<NAME>` | none | for each officer with `ship.rollout` | An https webhook outside HQ (a Discord-style `{"content": …}` endpoint) that a failed rollout escalates to. Read by the server only. |
| `MUSTER_OFFICER_TOKEN_<NAME>` | none | for each officer | That officer's HQ token (scope `officer`). `<NAME>` is the officer id upper-cased, with every other character written as `_`: `release:example-web` → `MUSTER_OFFICER_TOKEN_RELEASE_EXAMPLE_WEB`. Read by the server only; never passed to an instance's pod. |

## Tracing (muster-server and muster-mirror)

Both binaries export OpenTelemetry traces over OTLP/HTTP when an endpoint is set,
and do nothing at all when none is ([telemetry.md](telemetry.md)). These are the
variables muster reads to decide that; the exporter and the SDK also read the
standard `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES`,
`OTEL_EXPORTER_OTLP_HEADERS` and `OTEL_EXPORTER_OTLP_TIMEOUT` themselves.
`OTEL_TRACES_SAMPLER` is not read: muster's own sampler always keeps opening a
session (a mirrored view or terminal, an agent's page, and their sockets) and
anything under a sampled parent in the same process (or muster's own, on a
host), and keeps every other request — a page's poll carrying the browser's
trace context included — at `MUSTER_TRACE_SAMPLE_RATIO`.

| variable | default | required | what it does |
|---|---|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | none (off) | no | The OTLP/HTTP endpoint, e.g. `http://tempo:4318` (`/v1/traces` is appended). Setting it turns tracing on. Unset, the process installs no tracer, wraps no handler and adds no header. |
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | none | no | The full traces URL, instead of the endpoint above. Setting either turns tracing on. |
| `OTEL_SDK_DISABLED` | `false` | no | `true` keeps tracing off whatever endpoint is set. |
| `MUSTER_TRACE_SAMPLE_RATIO` | `0.05` | no | The share of traces kept for requests that do not open a session (list refreshes, the event stream, polls), from `0` to `1`. Opens and their sockets are always kept; a page's other requests are kept at this ratio even when they carry the browser's (sampled) trace context. A value outside `0`–`1` keeps tracing off and logs why. |

## muster-mirror (host agent)

Each setting is also a flag (`-url`, `-token-file`, …). See
[mirroring.md](mirroring.md).

| variable | default | required | what it does |
|---|---|---|---|
| `MUSTER_MIRROR_URL` | none | yes, except with `-list` | The muster base URL to connect to. |
| `MUSTER_MIRROR_TOKEN_FILE` | none | this or the token | A file holding this host's mirror token. If both are set, the file wins. |
| `MUSTER_MIRROR_TOKEN` | none | this or the file | The mirror token itself. |
| `MUSTER_MIRROR_HOST` | the short hostname | no | This host's name. It must match the name the token was minted for. |
| `MUSTER_MIRROR_INTERACTIVE` | off | no | Allows typing and sending messages. The default is read-only. |
| `MUSTER_MIRROR_HERMES_SEND` | off | no | With `MUSTER_MIRROR_INTERACTIVE`: lets muster send to Hermes windows, through the gated send (screen check, Escapes, a database-confirmed submit; [hermes.md](design/hermes.md)). Turn it on per host only after `make test-livehermes` is green against that host's Hermes. The host reads `display.vim_mode` from the Hermes home's `config.yaml` (that section alone) and sends nothing while it is on or unreadable; `-list -hermes` shows it. |
| `MUSTER_MIRROR_HERMES_ANSWER` | off | no | With `MUSTER_MIRROR_INTERACTIVE`: lets muster answer a Hermes **approval** (`once` or `deny`) through Hermes's gateway viewer, which the host attaches only to a session waiting on an approval and only to the pinned Hermes (`-list -hermes` names its gateway). Answering `once` lets the command run. Never a key into the pane; sudo prompts and clarify questions are answered in the terminal. |
| `MUSTER_MIRROR_RESUME_SPAWN` | off | no | With `MUSTER_MIRROR_INTERACTIVE`: lets muster **resume an ended Claude Code session** on this host, in a new detached tmux window running `claude --resume <id>` in the working directory its transcript records, when agent mail arrives for it ([plugins](plugins.md#resuming-an-ended-session)). It starts a process on the host. Refused for a session running here already, a transcript this host does not hold, or a directory that is gone; at most one resume per session per 10 minutes and 6 an hour. |
| `MUSTER_MIRROR_RESUME_SESSION` | `muster-resume` | no | The tmux session resumed windows start in; created when absent. |
| `MUSTER_MIRROR_RESUME_CLAUDE` | `claude` | no | The Claude Code binary a resume runs. A bare name is looked up on the tmux **server's** `PATH`, so an absolute path is safer. |
| `MUSTER_MIRROR_TMUX_SOCKET` | tmux's default | no | Uses a different tmux server: `tmux -L <name>`. |
| `MUSTER_MIRROR_TMUX_SOCKET_PATH` | none | no | `tmux -S <path>`. It wins over the socket name. |
| `MUSTER_MIRROR_LABELS_FILE` | none | no | A JSON object `{"<session>": "<codename>"}` of display labels. |
| `MUSTER_MIRROR_PANE_MAP_DIR` | none | no | A directory of per-pane Claude Code records, kept current by a hook. |
| `MUSTER_MIRROR_SNIPPETS_FILE` | `$XDG_CONFIG_HOME/muster-mirror/snippets.json` | no | A JSON array of `{trigger, label, body}`: the prewritten messages the Rendered view's message bar offers after `:`. Re-read when it changes; missing or invalid = none. |
| `MUSTER_MIRROR_MCP_LISTEN` | none (off) | no | A loopback address (`127.0.0.1:<port>` or `[::1]:<port>`) on which to serve this host's Claude Code sessions muster's attention and task tools as an MCP endpoint (`http://<addr>/mcp`); any other address is refused. Optional: if it cannot start, the host logs why and mirrors without it. See [attention.md](attention.md#mirrored-claude-code-sessions). |
| `MUSTER_MIRROR_MCP_SECRET_FILE` | `$XDG_CONFIG_HOME/muster-mirror/mcp-secret` | no | The local MCP endpoint's secret, which every call must present. Created (0600, in a 0700 directory) when missing; a file readable by others is refused. |
| `XDG_CONFIG_HOME` | `~/.config` | no | Where the default snippets file is looked for. |
| `MUSTER_MIRROR_ALLOW_HTTP` | off | no | Allows a plain-`http` muster URL. Use it for tests only. |
| `MUSTER_MIRROR_WRITE_HOOK_CONFIG` | off | no | Set it on the host's managed `muster-mirror` service only (or pass `-write-hook-config`). It makes the daemon record its URL and token file path for `muster-mirror ask` when it starts. |
| `MUSTER_MIRROR_HOOK_CONFIG` | `~/.muster/mirror-hook.json` | no | Where `muster-mirror` records its URL and token file path (never the token) for `muster-mirror ask`, and where the hook reads them. |
| `MUSTER_MIRROR_HERMES_HOME` | `$HERMES_HOME`, else `~/.hermes` | no | The Hermes home a Hermes window's session is resolved from, read-only: only `state.db`, `terminal-sessions/tty-*` and `shell-hooks-allowlist.json`. Also where `muster-mirror hermes-allowlist` writes. See [mirroring.md](mirroring.md#hermes-windows). |
| `HERMES_HOME` | `~/.hermes` | no | Hermes's own home variable, the default of `MUSTER_MIRROR_HERMES_HOME`. |
| `MUSTER_MIRROR_HERMES_STATUS_DIR` | `~/.muster/hermes-status` | no | Where `muster-mirror hermes-hook` writes one status file per Hermes session (0700 directory, 0600 files), and where the daemon reads them. Recorded for the hook with `-write-hook-config`. |
| `MUSTER_MIRROR_OPENCODE_TOKEN_FILE` | `~/.muster/opencode-token` | no | The opencode pair token, `{url, token}`, mode 0600: written by `muster-mirror opencode-pair`, read and renewed by the daemon. A file others can read, a symlink or a non-loopback url is refused. Missing = opencode windows have no rendered view. See [mirroring.md](mirroring.md#opencodes-token-muster-mirror-opencode-pair). |
| `XDG_STATE_HOME` | `~/.local/state` | no | Where `muster-mirror opencode-pair` looks for the opencode service's registration file (`opencode/service-prod.json`, then `opencode/service.json`). Read by the pairing step only. |
| `CLAUDE_CONFIG_DIR` | `~/.claude` | no | Claude Code's config directory, where `sessions/` and `projects/` are read from, and the user's `skills/`, `commands/` and enabled plugins the composer's `/` menu lists. For a window's transcript, a Claude Code process's OWN absolute `CLAUDE_CONFIG_DIR` (from its environment) wins over this one ([mirroring.md](mirroring.md#why-a-window-opens-in-the-terminal)). `ccd` reads it too. |

## muster (CLI)

See [cli.md](cli.md). The CLI looks for each setting in three places, from
lowest to highest priority: `~/.muster/muster.env`, then the environment, then
flags.

| variable | default | required | what it does |
|---|---|---|---|
| `MUSTER_API_URL` | none | yes | The muster base URL. It has no default. |
| `MUSTER_TOKEN` | none | yes | A personal API token (`mst_…`), or a shared machine secret. |
| `HOME` | the system's | no | Used to find `~/.muster/muster.env`. |
| `CLAUDE_CODE_SESSION_ID` | none | no | Sent with task writes as the session that made them. |
| `CLAUDE_SESSION_ID` | none | no | A fallback spelling of the variable above. |

`MUSTER_HOOK_TOKEN` is also accepted as an older name for `MUSTER_TOKEN`. If both
are set in the same place, `MUSTER_TOKEN` wins.

## ccd (inside the claude-code agent image)

`ccd` runs inside the `claude-code` agent container. muster sets these
variables when it provisions an agent, so you normally never set them yourself.

| variable | default | required | what it does |
|---|---|---|---|
| `MUSTER_GATEWAY_BEARER` | none | this or `HOOKS_TOKEN` | The bearer token `ccd`'s `/v1/responses` accepts. muster mints it per `claude-code` agent at random (`agents.gateway_secret`, migration 0042); it is not derived from `MUSTER_HOOK_TOKEN`. |
| `HOOKS_TOKEN` | none | this or the bearer | A token the bearer is derived from (`sha256("gw-"+token)`). If both are set, they must agree. muster no longer ships it to `claude-code` agents. |
| `MUSTER_CONNECT_URL` | none | no | muster's connect listener. `ccd approve` and `ccd connect-git` use it. |
| `CLAUDE_CODE_OAUTH_TOKEN` | none | this or a `/login` file | The Claude credential, taken from the agent's Secret. `ccd` treats either one as "has a credential". |
| `CCD_LISTEN` | `:18789` | no | The gateway address. |
| `CCD_HOOK_SOCKET` | none | no | Set by `ccd` itself in the environment of each Claude Code CLI it starts: the unix socket its hook listener is on, in a directory only `ccd`'s user can enter. `ccd hook` posts each hook event there. Not for an operator to set. |
| `CCD_HOOK_SECRET` | none | no | Set by `ccd` itself beside `CCD_HOOK_SOCKET`: the per-process credential every hook must carry. A hook without it is refused and changes nothing. It keeps other containers in the pod out; it does not stop a process of `ccd`'s own user (every tool the CLI runs inherits it). Not for an operator to set. |
| `CCD_HOOK_DISABLED` | off | no | With `1`, `ccd hook` does nothing. |
| `CCD_HOOK_LOG` | none | no | Appends every raw hook payload to this file, for debugging. It includes prompt text. |
| `CCD_TMUX_SOCKET` | tmux's default | no | `tmux -L` socket name. |
| `CCD_TMUX_CONF` | tmux's default | no | `tmux -f` config file. |
| `CCD_TMUX_TARGET` | `cc` | no | The tmux session the CLI runs in. |
| `CCD_WORKSPACE` | the current directory | no | The CLI's working directory. muster sets it to the checkout (`/data/workspace/<name>`) for an agent dispatched with a repository, and leaves the image's `/data/workspace` otherwise. |
| `CCD_REPO` | none | no | `owner/name`. `ccd` clones `https://github.com/<it>` into `CCD_WORKSPACE` and starts the CLI only once the checkout exists ([provisioner.md](provisioner.md#a-claude-code-agents-repository)). muster sets it for an agent dispatched with a repository. |
| `CCD_REPO_BRANCH` | the remote's default | no | The branch `ccd` clones. |
| `CCD_REPO_MIRROR` | none | no | The path of a bare mirror of `CCD_REPO` (`/var/lib/muster-mirror/repo.git`) that the pod's deploy-key holder makes. Set, `ccd` clones from it — locally, with no credential — instead of from `https://github.com/<CCD_REPO>`; it must be a clean absolute path, or `ccd` refuses to start. muster sets it for a worker of a pool `MUSTER_AGENT_CC_POOL_DEPLOY_KEYS` lists ([jobs](jobs.md#private-repositories-a-pools-deploy-key)). No key, and no path to one, is ever in ccd's environment. |
| `CCD_SUPERVISE` | off | no | With `1`, `ccd` creates the tmux session and keeps `claude` running in it. |
| `CCD_CLAUDE_BIN` | `claude` | no | The path to the Claude Code CLI. |
| `CCD_LANE` | none | no | Set by `ccd` itself in the environment of each extra Claude Code CLI it starts (one per chief session on the `claude-code` runtime), so `ccd hook` reports that CLI's hooks as that session's. Not for an operator to set. |
| `CCD_MAX_SESSIONS` | `3` | no | How many Claude Code CLIs `ccd` runs **beside** the original one, one per chief session started on the `claude-code` runtime (each its own tmux session `cc-<id>`, started with that session's `--model`). At the cap, the least recently used idle one is stopped; its conversation resumes on its next turn. When all are mid-turn, a new session's turn is refused with a typed `capacity` 503. `0` turns this off: such a turn is `not_ready`. Each CLI costs roughly 250–550 MiB of the pod's 3 GiB. |
| `CCD_JOB_PERMISSION_MODE` | none | no | Unset: every CLI runs at default permissions with bypass disabled. `bypassPermissions`: `ccd seed` lifts `disableBypassPermissionsMode` and records the bypass disclaimer's consent (`skipDangerousModePermissionPrompt`), and `ccd` starts each **job** lane (a sha job's worktree) with `--permission-mode bypassPermissions`; the original CLI and chat lanes are unchanged. Any other value is refused by `ccd seed` and `ccd serve`, so the pod does not start. muster sets it only for an agent in a pool `MUSTER_AGENT_CC_BYPASS_POOLS` lists. |
| `CCD_ALLOW_MCP_TOOLS` | none | no | A comma list of **muster MCP tool names** `ccd seed` pre-allows in the primary CLI's `settings.json`, each written as `permissions.allow` entry `mcp__muster__<name>` on top of the image's permission posture, re-imposed on every pod start (not on an in-pod CLI restart). Only bare names (`[a-z][a-z0-9_]*`, no `__`) are accepted, and the prefix is forced, so it can never allow a built-in tool, a rule or another server's tool; any other value is refused and the pod does not start. muster sets it only for an [officer](officers.md) instance, to that officer's own tools and, when the connector store is configured, `connectors_list`, `connector_request` and `connector_status`. |
| `CCD_PERMISSION_MODE` | none | no | The permission mode every Claude Code session in the pod starts in. Its one value is `bypassPermissions`: `ccd seed` sets `permissions.defaultMode` to it, drops the image's `disableBypassPermissionsMode` and records the bypass disclaimer's consent, so **no tool call ever stops on a permission prompt**. Unset keeps the image's posture (default mode, bypass disabled). Any other value is refused and the pod does not start. muster sets it only for an [officer](officers.md#the-instances-claude-code-session) instance. Distinct from `CCD_JOB_PERMISSION_MODE`, which bypasses job lanes only. |
| `CCD_MODEL` | none | no | The model `ccd seed` writes as the primary CLI's settings `model` (an alias such as `opus`, or a model id). Unset removes a model `ccd seed` itself wrote earlier (recorded in `.claude.json`), and otherwise leaves the file's model alone (a choice made in the pod). A value that is not a model name is refused and the pod does not start. muster sets it only for an [officer](officers.md) instance whose officer names a `model`. |
| `CCD_SUBMIT_TIMEOUT` | `20s` | no | How long to wait for a pasted prompt to be accepted. |
| `CCD_TURN_TIMEOUT` | `30m` | no | The deadline for a whole turn. |
| `CCD_APPROVAL_DEADLINE` | `170s` | no | How long `ccd approve` waits for a relayed decision before falling back to the terminal prompt. |
| `CCD_LIVECHECK_HOOKS_TOKEN` | none | for `livecheck` | The token for `cmd/ccd/livecheck`, a manual probe of a live agent. |

**Turn credentials and the processes an agent's session can reach.** The turn
credentials are `MUSTER_GATEWAY_BEARER`, `HOOKS_TOKEN` and
`CCD_GATEWAY_UPSTREAM_BEARER` (`agentspec.TurnCredentialEnv`). They are kept out
of every process the session can reach, by four routes:

- `ccd` reads all three, then unsets them before it starts anything, so the
  Claude Code CLI, its hooks and every tool it runs start without them.
- `ccd` marks itself non-dumpable, so a process of its own user cannot read them
  back from `/proc/<ccd>/environ`.
- Every exec muster runs into the container starts as
  `env -u MUSTER_GATEWAY_BEARER -u CCD_GATEWAY_UPSTREAM_BEARER -u HOOKS_TOKEN …`.
  An exec inherits the CONTAINER's environment, not `ccd`'s, so without this the
  terminal's long-lived `tmux attach` client (readable at `/proc/<pid>/environ`)
  and anything a job's git ran would hold them. This is structural: the
  provisioning adapter holds the driver only as a wrapper
  (`internal/agentprovision/scrubexec`) that adds the prefix to every exec, and
  the raw driver is unreachable from the adapter. Two go/types tests pin this.
  One checks the structure. The other resolves every exec entry in the module.
- The job scripts' git (job worktree prepare and remove, untracked result
  removal) runs with repository-configured commands neutralised by name:
  - `core.fsmonitor`;
  - every hook in a hooks directory (`core.hooksPath=/dev/null`);
  - config-based hooks (`hook.<event>.enabled=false` for every event; the image's
    git 2.39 predates them);
  - `core.alternateRefsCommand`;
  - `ext::` remotes;
  - `core.sshCommand`;
  - `remote.origin.uploadpack`;
  - `credential.helper`;
  - `core.askPass`;
  - submodule recursion;
  - and `core.gitProxy`, by setting `GIT_PROXY_COMMAND` empty.

`ccd` also refuses to start when its bearer equals, or is the
`sha256("gw-"+…)` derivation of, `MUSTER_HOOK_TOKEN`. A pod provisioned before
muster minted the turn credential is in that state, and Stop then Start fixes it.

**What can still run during a job's git, and what guards it.**

- Filter drivers (`filter.<name>.smudge` / `.process`, selected by
  `.gitattributes` or `.git/info/attributes`) still run: the session chooses
  their names, so no override can name them.
- Anything else git runs from configuration that the list above does not name
  can also still run.

These run as the agent's own user, which the session already is. They see no
turn credential **only because of the `env -u` prefix**. That prefix is THE
credential guard for them; weakening it re-opens a route the git overrides
cannot close.

**What is still open:**

- A process of the agent's user can type into the CLI's pane with
  `tmux send-keys`.
- `CCD_HOOK_SECRET` stays in the CLI's environment, because `ccd hook` needs it.
  It authenticates the hook channel only; `/v1/responses` refuses it.
- `MUSTER_HOOK_TOKEN` stays, because it is the agent's muster identity.

## Test and e2e harness

These variables are read by non-test code that exists only for testing. That
code is `internal/dbtest`, plus binaries built with the `e2eterm` or `e2emirror`
build tag, which the Playwright suite uses. The shipped binaries do not read them.

| variable | default | required | what it does |
|---|---|---|---|
| `MUSTER_TEST_DATABASE_URL` | none (DB tests skip) | for store tests | The Postgres template database for the Go suite. See [testing.md](testing.md). |
| `MUSTER_TEST_REQUIRE_DB` | off | in CI | Turns a missing database into a failure instead of a skip. |
| `MUSTER_E2E_SUB` | none | for `e2e/mkmember` | A GoTrue user id for the e2e member fixture. |
| `MUSTER_E2E_EMAIL` | none | for `e2e/mkmember` | That fixture user's email. |
| `MUSTER_E2E_AGENT` | none | for `e2e/mkchat` | The name of the agent the mirror-chat spec's chat fixture creates (or deletes). |
| `MUSTER_E2E_DELETE` | off | no | (`e2e/mkchat`) `1` deletes that agent instead of creating it. |
| `MUSTER_E2E_FOCUS` | none | no | (`e2e/mkchat`) A JSON array of `{subject, title, description, history}` (or `{subject, error}`, a failed refresh's reason; `history` writes that many earlier focuses first, for the focus history): session focus to write for the agent it creates (`"self"`) or for a mirrored session (`"mirror:<claude session id>"`), used by the focus spec. Deleting also deletes the mirrored ones it names. |
| `MUSTER_E2E_DISPLAY_NAME` | none | no | (`e2e/mkchat`) The agent's display name; `chief` makes it the chief panel's agent. |
| `MUSTER_E2E_ACTIVE_MINUTES_AGO` | none | no | (`e2e/mkchat`) Backdates the agent's activity (its row's `created_at`/`updated_at` and every message) to that many minutes ago, so a spec knows where it sorts in the sidebar and the session switcher. |
| `MUSTER_E2E_TASK` | none | no | (`e2e/mkchat`) Also creates a `ready_for_review` task with this title as the agent's source task (its page then shows Mark complete). Deleting with it set deletes that task too. |
| `MUSTER_E2E_THREADS` | off | no | (`e2e/mkchat`) `1` reads several threads (title, age, messages) from stdin instead of one message list. |
| `MUSTER_E2E_APPEND` | off | no | (`e2e/mkchat`) `1` appends stdin's messages to the latest session of the EXISTING agent instead of creating one (a message stored while a page is open). |
| `MUSTER_E2E_KIND` | the default kind | no | (`e2e/mkchat`) The agent's kind, e.g. `claude-code`. |
| `MUSTER_E2E_REPO` | none | no | (`e2e/mkchat`) The agent's repository, `owner/name`. |
| `MUSTER_E2E_CC_ACCOUNT` | none | no | (`e2e/mkchat`) The Claude account a `claude-code` agent names. Only a name: nothing is provisioned. |
| `MUSTER_E2E_HOLD_READY_DIR` | none | no | (`e2eterm`) Holds fake instances not-ready until a file named after each instance appears in this directory. |
| `MUSTER_E2E_TERM_WINDOW` | none | no | (`e2eterm`) The `CxR` window size the fake agent pod reports. |
| `MUSTER_E2E_GITHUB_API` | none | no | (`e2eterm`) Points every GitHub API call at this base URL (a fake GitHub), so the dispatch dialog's repo picker can be driven with fixture repos. |
| `MUSTER_E2E_OPENROUTER_MODELS` | none | no | (`e2eterm`) Points the model search at this URL instead of OpenRouter's public model list (a fake catalogue). |
| `MUSTER_E2E_SUGGEST_URL` | none | no | (`e2eterm`) Points the suggested replies' model at this chat-completions URL, on a fixed invented key, instead of OpenRouter (the chip row spec's fake model). |
| `MUSTER_E2E_AGENT_ENDPOINT` | none | no | (`e2eterm`) Points every agent's gateway endpoint at this loopback URL (`http://127.0.0.1:<port>`, anything else refused): the answer-delivery spec's fake runtime. |
| `MUSTER_E2E_GOTRUE_SKEW` | none (`30s`, `gotrue.DefaultSkew`) | no | (`e2eterm`) The GoTrue access-token verifier's clock skew, a Go duration, so the specs that outlive an access token wait seconds instead of 35 s. |
| `MUSTER_MIRROR_E2E_REFRESH` | none (`5s`) | no | (`e2emirror`) How often the fake host re-reads its windows (`mirrorhost.Config.Refresh`), a Go duration. |
| `MUSTER_MIRROR_E2E_SESSIONS` | none | no | (`e2emirror`) The fake tmux server's session names, comma-separated. |
| `MUSTER_MIRROR_E2E_WINDOWS_FILE` | none | no | (`e2emirror`) A per-window fake. Each line is `session\|@id\|index\|active\|command\|activity\|transcript`. |
| `MUSTER_MIRROR_E2E_HERMES_HOME` | none | no | (`e2emirror`) A scratch Hermes home: the fake makes `state.db` there from Hermes's pinned schema and inserts each new line of `rows.jsonl` as a message, as Hermes's gateway would. |
| `MUSTER_MIRROR_E2E_OPENCODE_DIR` | none | no | (`e2emirror`) Turns on a fake opencode v2 server inside the fake host, with a token it issued; each new line of `ops.jsonl` here puts a session or a message, sets a status, or rotates the server's password. |
| `MUSTER_MIRROR_E2E_OPENCODE_WINDOWS_FILE` | none | no | (`e2emirror`) opencode windows added to a per-window fake. Each line is `session\|@id\|index\|active\|activity\|pane title`; the session is matched from the title by the real matcher. |
| `MUSTER_MIRROR_E2E_HERMES_WINDOWS_FILE` | none | no | (`e2emirror`) Hermes windows added to a per-window fake. Each line is `session\|@id\|index\|active\|activity\|hermes session id\|status`; an empty session id is a window with no rendered view. |
| `MUSTER_MIRROR_E2E_QUERIES` | unset | no | (`e2emirror`) `1`: every attach writes the terminal queries a real tmux sends (Device Attributes, a cursor position report) and turns focus reporting on, so the page's terminal answers them on its own. |
| `MUSTER_MIRROR_E2E_CLAUDE` | none | no | (`e2emirror`) `<session>=<transcript path>`: plays that session as a Claude Code pane. |
| `MUSTER_MIRROR_E2E_NO_PROMPT_INDEX` | unset | no | (`e2emirror`) `1`: the host sends no prompt index and no prompt places, as one that predates them (`mirrorhost.Config.NoPromptIndex`). |
| `MUSTER_MIRROR_E2E_NO_CATALOG` | unset | no | (`e2emirror`) `1`: the host advertises no composer-menu catalog and answers no request, as one that predates it (`mirrorhost.Config.NoCatalog`). |
| `MUSTER_MIRROR_E2E_NO_SUBAGENTS` | unset | no | (`e2emirror`) `1`: the host advertises no subagent reporting — no subagent summaries on a view's stream, no tail request answered — as one that predates the subagent cards (`mirrorhost.Config.NoSubagents`). |
| `MUSTER_MIRROR_E2E_NO_WORK` | unset | no | (`e2emirror`) `1`: the host sends no `work` and no `colors` message, as one that predates the live work counts and identity colours (`mirrorhost.Config.NoWork`). |
| `MUSTER_MIRROR_E2E_CWD` | unset | no | (`e2emirror`) The working directory of every window of the per-window fake: where its project `.claude` is found. |
| `MUSTER_MIRROR_E2E_PASTE_FILE` | none | no | (`e2emirror`) Where pastes into non-Claude panes are recorded. |
| `MUSTER_MIRROR_E2E_ACTIVE_FILE` | none | no | (`e2emirror`) `session=index` lines: each session's active window. |
| `MUSTER_MIRROR_E2E_ACTIVITY_FILE` | none | no | (`e2emirror`) `session=<unix seconds>` lines: each session's last activity. |
| `MUSTER_MIRROR_E2E_WINDOW` | none | no | (`e2emirror`) The `CxR` window size the fake reports. |
| `MUSTER_MIRROR_E2E_SCROLL_FILE` | none | no | (`e2emirror`) Turns on copy-mode scrolling and records each scroll (`<pane> <lines> pos=<n>`) and cancel (`<pane> cancel`). While `<file>.mode` exists every pane is in another mode, and a scroll is refused as one. Unset, the fake is an older host that cannot scroll. |
| `MUSTER_MIRROR_E2E_NO_STATUS` | none | no | (`e2emirror`) `1`: the per-window fake does not advertise Claude Code status, as an older muster-mirror does. (Its status and time otherwise come from `<transcript>.status`, `{"status", "statusUpdatedAt"}`; without that file it reports a status with no time.) |
