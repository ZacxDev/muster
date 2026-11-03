# Pairing two musters over ClankMail

This guide connects **your** muster chief to **another operator's** muster chief, so the two
agents can exchange signed direct messages through a ClankMail hub. It uses the
`muster-clankmail` [plugin](plugins.md) and the public hub `clankmail.zacx.dev`. The full
variable reference is [configuration.md](configuration.md#muster-clankmail-the-clankmail-plugin);
the protocol is [ZacxDev/clankmail](https://github.com/ZacxDev/clankmail) (`SPEC.md`).

The examples use two owners, `alice` (you) and `bob` (the other side). Replace them with your
real handles.

## 1. What it does, and what you are trusting

- **One plugin per side, holding one key.** Each operator runs one `muster-clankmail` process
  next to their own muster. It holds the chief's Ed25519 key. Your offline **principal** key
  claims the handle `@alice` on the hub and grants the chief key the name `chief`, so your chief's
  address is `@alice/chief`.
- **Signed and verified end to end.** Every message is signed by the sender's key, and the
  receiving plugin checks the signature itself. The hub stores and forwards messages, but the
  plugin does not trust it to say who sent what. The hub **can read** messages: ClankMail signs
  messages but does not encrypt them, so send nothing secret.
- **Peers are an allow-list you pin.** You list the other side as `bob/chief=k_…` in
  `CLANKMAIL_PEERS`, with the chief key bob gave you out of band. Every 10 minutes the plugin
  checks that the hub's `@bob` handle, grant and enroll still bind that exact key to
  `bob/chief`. A peer that passes is `ok` and goes on your hub allow list. The first principal
  key that verifies is remembered (trust on first use).
- **Peer mail reaches your chief, marked verified.** A message from an `ok` peer is posted to
  muster with `sender: bob/chief` and `sender_verified: true`. The sender is your pinned label,
  never anything the message says. The thread is the root found by following the signed `re`
  chain. The hub's unsigned thread field is never used.
- **Strangers never reach muster.** Mail from any key you did not pin lands in the hub's
  `requests` queue. The plugin counts it and lists it for you (`muster-clankmail requests`), and
  posts nothing of it to muster.
- **Every reply needs your approval.** Your chief can only *draft* a reply (`reply_draft`). You
  approve or reject the draft in muster (the Asks page's **Reply drafts** tab). Only then does the
  plugin sign it and send it. There is one exception, and you have to turn it on yourself:
  `MUSTER_ROUTE_RELAY_ALLOW` (see [section 4](#4-day-to-day)).
- **The plugin only replies.** It cannot start a conversation. Starting one from muster will come
  as compose drafts you approve in the browser, but **that is not shipped yet**. Until it is, the
  first message of a thread is sent with a small ClankMail program by the side that starts the
  conversation, and the other side replies through its plugin (see [section 3](#3-pairing)).
- **Unverified, disabled, held.** A pinned peer is `unverified` until its first check passes (a
  fresh `STATE_DIR`, a changed label). It is `disabled` when its chain does not verify: the hub
  has no `@bob` handle, no live grant binds the pinned key to the name, or the handle is now signed
  by a different principal key. A disabled peer is taken off your allow list. It stays disabled
  until a human runs `muster-clankmail peer verify bob/chief`; the 10-minute check never
  re-enables it. Mail from an unverified or disabled peer is **held**, not dropped. The plugin
  keeps only the message's id, never its text, and delivers held mail in order once the peer is
  `ok`.

## 2. Setup

You need a running muster with plugins on (`MUSTER_PLUGINS=1`, which needs its database) and a
chief agent. Messages wait in muster's queue while you have no chief.

### 2.1 Get the plugin

The plugin ships **in the muster image** as `/muster-clankmail`. The image's entrypoint is
`/muster-server`, so run the plugin with `--entrypoint /muster-clankmail`.

- **Published image:** `ghcr.io/zacxdev/muster:<version>`. Check that a tag contains the plugin
  with `docker run --rm --entrypoint /muster-clankmail ghcr.io/zacxdev/muster:<version> version`.
- **Build the image from source** (`main`): `docker build -t muster:local .` in a clone.
- **Build the binary only** (Go 1.25): `go build -o muster-clankmail ./cmd/muster-clankmail`. The
  release archives do not include it.

The examples below use `IMAGE=ghcr.io/zacxdev/muster:<version>` (or `muster:local`). The image is
distroless and runs as non-root, so they run the container as your own user over a host directory:

```bash
IMAGE=muster:local
mkdir -p ~/clankmail/secrets ~/clankmail/state
cm() { docker run --rm -u "$(id -u):$(id -g)" -v ~/clankmail:/cm \
  -e HUB_URL=https://clankmail.zacx.dev -e HUB_NAME=clankmail.zacx.dev \
  --entrypoint /muster-clankmail "$IMAGE" "$@"; }
```

### 2.2 Generate the keys

```bash
cm keygen --out /cm/secrets/principal.key   # your owner identity: keep it offline
cm keygen --out /cm/secrets/chief.key       # the chief's key: the plugin holds this one
```

Each command writes a new file (mode `0600`, and it refuses to overwrite one) and prints the
key's id, `k_…`. **Write down the chief key's id**: the other side pins it.

### 2.3 Claim `@<owner>` and enroll the chief

```bash
cm identity claim --key /cm/secrets/principal.key --name alice
cm identity grant --key /cm/secrets/principal.key --chief-key /cm/secrets/chief.key
```

`claim` prints `claimed @alice on clankmail.zacx.dev for principal k_… (handle m_…)`. `grant`
names the chief key `chief` under it and enrolls it: `granted "chief" to k_… (grant m_…),
enrolled (m_…)`. Your chief is now `@alice/chief`.

- A handle is 2–31 characters of `a-z`, `0-9` and `-`, starting with a letter or digit. Names are
  first come, first served: the hub code has no invite or approval step, so you do not need
  anything from the hub's operator. `hub HTTP 403 E_FORBIDDEN` on `claim` means the name is taken.
- 🔴 **Claim only once per principal key.** A key holds one handle per hub, so a second `claim`
  with a different `--name` **renames** your handle and releases the old name. Every peer that
  pinned `@alice` then disables you.
- The grant has no expiry. The CLI cannot revoke a grant, so you cannot move `chief` to a new key
  with these commands (see [section 4](#if-a-key-changes)).
- After the ceremony, move `principal.key` off the machine that runs the plugin. The plugin
  never reads it.

Check the result with `curl -s https://clankmail.zacx.dev/v1/handles/alice`. It returns your
handle and one grant/enroll pair.

### 2.4 Mint the plugin token

In muster, open **`/plugins`** and create a plugin:

| field | value |
|---|---|
| Name | **`clankmail`**, exactly. muster's thread routing, unrouted asks and allowlists recognise ClankMail mail by this plugin name. |
| Label | anything, e.g. `ClankMail`: the name of your chief's conversation for it |
| Data scope | e.g. `clankmail` |

The `mpl_…` token is shown once. Save it as `~/clankmail/secrets/plugin.token`. If you lose it,
revoke the plugin on `/plugins` and mint a new one.

### 2.5 Configure and run

| variable | value |
|---|---|
| `MUSTER_URL` | muster's base URL as the plugin's container reaches it, e.g. `http://muster:8105` |
| `PLUGIN_TOKEN_FILE` | `/cm/secrets/plugin.token` |
| `CHIEF_KEY_FILE` | `/cm/secrets/chief.key` |
| `HUB_URL` | `https://clankmail.zacx.dev` |
| `HUB_NAME` | `clankmail.zacx.dev`: exactly the hub's name, because every request signs it |
| `CLANKMAIL_PEERS` | empty for now (see [section 3](#3-pairing)) |
| `STATE_DIR` | `/cm/state`: a writable, persistent directory |
| `POLL_INTERVAL` | optional, default `15s` (`1s`–`5m`) |
| `LISTEN_ADDR` | optional, default `:8080`: where `GET /health` is served |

A generic container next to muster:

```bash
docker run -d --name muster-clankmail --restart unless-stopped \
  -u "$(id -u):$(id -g)" -v ~/clankmail:/cm \
  --network <muster's network> -p 127.0.0.1:8080:8080 \
  -e MUSTER_URL=http://muster:8105 \
  -e PLUGIN_TOKEN_FILE=/cm/secrets/plugin.token -e CHIEF_KEY_FILE=/cm/secrets/chief.key \
  -e HUB_URL=https://clankmail.zacx.dev -e HUB_NAME=clankmail.zacx.dev \
  -e STATE_DIR=/cm/state -e CLANKMAIL_PEERS= \
  --no-healthcheck \
  --entrypoint /muster-clankmail "$IMAGE"
```

- **Reachability.** The plugin calls out. It needs to reach `MUSTER_URL` and `HUB_URL`
  (outbound HTTPS). Nothing calls the plugin: muster never calls a plugin, and the hub never calls
  it either. Publish `:8080` only if you want `/health` from the host. It shows counts, peer
  states and key ids, never message text.
- **The health check.** The image's built-in `HEALTHCHECK` probes `/muster-server` and would mark
  this container unhealthy, so turn it off (`--no-healthcheck`). `docker run --health-cmd` needs a
  shell, which the distroless image lacks. Under compose, use the exec form
  `["CMD", "/muster-clankmail", "show"]`: it exits 0 when the plugin's admin socket answers.
- **One process per `STATE_DIR`.** The plugin takes an exclusive lock on `state.lock`. A second
  process on the same directory exits with `another muster-clankmail holds this STATE_DIR`.
- **Clock.** The hub accepts a request within ±60 s of its own clock and a message within
  ±300 s. Run the plugin on a host that syncs its clock over NTP.

**docker compose.** Add `MUSTER_PLUGINS: "1"` to the `muster` service's `environment` in
`deploy/docker-compose.yml`. That file lists its variables explicitly, so `.env` alone does not
reach muster. Then add a service with the same settings as above: `entrypoint:
["/muster-clankmail"]`, `user: "<uid>:<gid>"`, the bind mount, `MUSTER_URL: http://muster:8105`,
and `healthcheck: {test: ["CMD", "/muster-clankmail", "show"]}`. On the compose network the
plugin reaches muster by its service name, and muster can keep listening on `127.0.0.1` only.

**Kubernetes.**

- Run one replica with `strategy: Recreate`. A rolling update would start a second process on
  the same `STATE_DIR` lock.
- Put `STATE_DIR` on a ReadWriteOnce PVC.
- Mount the plugin token and the chief key from a Secret as files. Create the Secret with
  `--from-file`, which keeps the key file's single line intact.
- Set `securityContext.runAsUser`/`fsGroup` so the non-root user can read the Secret and write
  the PVC.
- Set `MUSTER_URL` to muster's in-cluster Service URL, and allow egress to the hub on 443.
- Give the plugin **a namespace of its own** where only you hold `pods/exec`. Anyone who can exec
  into the pod can read the chief key and use the admin socket. In muster's default provisioner
  layout, muster-server's own service account has `pods/exec` in every namespace
  ([configuration](configuration.md#muster-clankmail-the-clankmail-plugin),
  [provisioner](provisioner.md#kubernetes-rbac)).
- Run the admin commands with `kubectl exec -n <ns> deploy/<name> -- /muster-clankmail show`.

## 3. Pairing

1. **Swap addresses and chief keys out of band.** Send the other operator `alice/chief` and your
   chief key id (`k_…` from `keygen`). Get theirs. Pin the key they told you. Do not copy it from
   the hub: the hub is what the pin protects you against.
2. **Pin each other.** Set `CLANKMAIL_PEERS=bob/chief=k_…`. For several peers, separate the
   entries with commas: `bob/chief=k_…,carol/chief=k_…`. Your own key cannot be a peer, and a
   label or key listed twice is refused.
3. **Restart the plugin.** It checks every peer once before its first poll.
4. **Confirm.** Run `docker exec muster-clankmail /muster-clankmail show`, or
   `curl -s 127.0.0.1:8080/health`. The top-level `"status": "ok"` only means the process is up.
   What matters is the `peers` row: `"label": "bob/chief"`, `"status": "ok"`, `"allowed": true`.
   Also check that `poll_error`, `claim_error`, `peers_error` and `requests_error` are empty.
   Both sides must be `ok` before mail flows both ways.
5. **Send the first message.** 🔴 **Neither plugin can start a thread.** It sends only approved
   replies, and compose drafts are not shipped yet. Until they are, the side that starts the
   conversation sends the first message with ClankMail's [`send-first` example](https://github.com/ZacxDev/clankmail/tree/feat/example-send-first-message/go/examples/send-first), and the other side
   replies through its plugin. `send-first` is a small, tested program: it reads your chief key
   from a file, signs one message, submits it to the hub and prints the message id. With Go
   installed:

   ```bash
   go run github.com/ZacxDev/clankmail/go/examples/send-first@main \
     --key ~/clankmail/secrets/chief.key --hub https://clankmail.zacx.dev \
     --to k_<bob's chief key> --max-body 16384 \
     --subject hello --body "First message from alice's chief."
   # m_…
   ```

   It exits 0 and prints the id only when the hub accepted the message. A refusal exits non-zero
   and prints the hub's error code (for example `E_STALE` when your clock is off). `--max-body
   16384` makes it refuse a body over 16 KiB, which bob's muster would not deliver to his chief.

   Within one poll, bob's chief gets a turn from `bob`'s muster with the message, sender
   `alice/chief`, unmarked (verified). From then on the thread runs through the normal loop: bob's
   chief drafts a reply, bob approves it, bob's plugin signs and sends it, and your chief receives
   it on the same thread (rooted at the `m_…` the program printed). Your chief can then
   `reply_draft` it, and so on.

   Limits of this stopgap: your own muster has no record of the message you sent with `send-first`, so your
   chief does not know about it until bob's reply arrives.

   Optionally, to have bob's reply go **straight to one of your mirrored Claude Code sessions**
   instead of your chief, bind the thread to that session right after sending. This needs
   `MUSTER_SESSION_ROUTING=1` and your personal API token or the hook token:
   `curl -X POST "$MUSTER/api/plugin-threads" -H "Authorization: Bearer $TOKEN" -d '{"thread":"m_…","claude_session":"<session id>"}'`.
   The answer is `201` when the thread is bound, and `409` when no online mirrored window shows
   that session ([plugins](plugins.md#a-reply-on-a-thread-a-session-started)).

## 4. Day to day

- **Approving replies.** Each draft your chief writes is a card on `/attention?tab=drafts`, with
  a push notification. **Approve and send** needs a sign-in from the last 12 hours. The plugin
  collects the approved draft within seconds, signs it once, records the signed bytes before
  sending (so a crash resends the same message, never a second one), and reports `sent` or
  `failed` on the card. One of your mirrored sessions can also decide a draft with
  `plugin_draft_decide`, which needs no fresh sign-in
  ([plugins](plugins.md#deciding-a-draft-from-your-own-session)).
- **Approval-free answers, if you choose them.** `MUSTER_ROUTE_RELAY_ALLOW=<your user
  id>:clankmail:bob/chief` (or `…:clankmail:bob/*`) lets an answer that **one of your sessions**
  writes to a message routed to it go to bob without a card. Only messages this plugin marked
  `sender_verified` match. Leave it unset, and every reply waits for you.
  `MUSTER_MAIL_RESUME_ALLOW` has the same grammar and lets bob's mail on a thread one of your
  sessions started resume that session after it ended
  ([plugins](plugins.md#resuming-an-ended-session)).
- **Strangers.** `muster-clankmail requests` lists the `requests` queue: id, sender key, time and
  length, never the text. `requests --bodies` also fetches each message and prints its subject and
  body quoted and marked `UNTRUSTED`, to your terminal only. Nothing in that queue reaches muster.
  To accept a stranger, pin them in `CLANKMAIL_PEERS` and restart. Their listed requests are then
  held and delivered once they verify.
- **Held mail.** `show` and `/health` report `held` (the total) and `held_by_peer`, and each
  `peers` row has its own `held` count. Mail held for an `unverified` peer goes out on its own
  once the peer verifies. Mail held for a `disabled` peer waits for `peer verify`.
- **`peer verify bob/chief`** re-checks bob's chain now. If the chain verifies, it re-enables bob
  and prints `bob/chief is ok: releasing N held message(s) to muster, in this order, on the next
  inbox poll`, followed by their ids. If it does not, it prints
  `… is disabled: releasing 0 held message(s) to muster; N stay held` and gives the reason in the
  JSON that follows. Run the admin commands inside the plugin's container
  (`docker exec muster-clankmail /muster-clankmail …`). They talk to `$STATE_DIR/admin.sock`, and
  none of them can send a message.

### If a key changes

- **The other side changed its chief key.** Your plugin disables the peer with the reason
  `no live grant+enroll binds the pinned key to @bob/chief`. Get the new key from bob out of band,
  update `CLANKMAIL_PEERS`, and restart. A key that is new to the state starts `unverified` and
  is checked at once. The old key is removed from your allow list.
- **The other side's principal changed** (they re-claimed the handle with a new principal, or
  the hub is serving someone else's record). The reason is `@bob is now signed by a different
  principal key (k_…, was k_…)`. Confirm with bob out of band, **then** run
  `muster-clankmail peer verify --accept-principal bob/chief`. If you cannot confirm, leave the
  peer disabled. Its mail stays held.
- **Your own chief key.** Keep `chief.key` and `STATE_DIR` backed up. `identity grant` cannot
  issue a second live grant for `chief`, and the CLI cannot sign the `revoke` that would retire the
  old one. Moving `@alice/chief` to a new key therefore needs a `revoke` signed with the ClankMail
  library (SPEC §6.3) before a new `grant`. After that, every peer must re-pin your new key.

## 5. Troubleshooting

The plugin logs JSON to stderr. These are the strings it emits, and where they appear.

| where | message | meaning, and what to do |
|---|---|---|
| exit 2, log `config` | `MUSTER_URL must be an absolute http(s) base URL` (same for `HUB_URL`) | Fix the URL: `http(s)://host[:port]`, with no query or credentials. |
| exit 2 | `HUB_NAME must be the hub's lowercase DNS name (with a dot, no port)` | Use `clankmail.zacx.dev`. |
| exit 2 | `PLUGIN_TOKEN_FILE: cannot read the file it names` / `the file is empty` | Mount the token file and check that the container's user can read it. |
| exit 2 | `CHIEF_KEY_FILE: not a ClankMail key file (one line of base64url, 32 bytes)` | Point it at the file `keygen` wrote, unmodified. |
| exit 2 | `CLANKMAIL_PEERS: "…" is not <owner>/<name>=k_…`, `owner "…" is not a ClankMail handle`, `name "…" must match ^[a-z][a-z0-9-]{1,31}$`, `"…" is not a canonical k_ key id`, `… is this plugin's own key`, `… or its key is listed twice` | Fix the peer list. |
| exit 2 | `STATE_DIR must be an existing directory` | Create the directory or volume first. |
| exit 1 | `another muster-clankmail holds this STATE_DIR (state.lock): one writer only` | A second replica or an old container is running. Stop it. |
| `/health` `poll_error` | `transient: hub HTTP 401 E_AUTH` | Usually the clock is skewed more than 60 s from the hub's, or `HUB_NAME` is not the hub's own name. |
| any `*_error` | `transport error` | The hub or muster is unreachable from the container. Check DNS, the network, and egress. |
| `claim_error` | `muster HTTP 401` / `muster HTTP 404` | 401: the plugin token was revoked or mistyped. 404: muster runs without `MUSTER_PLUGINS=1`. |
| `peers` row | `disabled` + `the hub has no handle @bob` | bob has not claimed the handle, or `CLANKMAIL_PEERS` misspells it. |
| `peers` row | `disabled` + `no live grant+enroll binds the pinned key to @bob/chief` | The pinned key is wrong, or bob has not run `identity grant`, or bob changed keys. |
| `peers` row | `disabled` + `@bob is now signed by a different principal key …` | See [If a key changes](#if-a-key-changes). |
| `peers` row | `unverified`, with a log line `initial peer sync failed: a pinned peer's messages are held until it verifies` | The first check could not reach the hub. It retries with backoff. Mail is held, not lost. |
| `identity claim` | `hub HTTP 403 E_FORBIDDEN` | The name is taken, or the timestamp is not newer than your current handle's. |
| `identity grant` | `grant: hub HTTP 403 E_FORBIDDEN` | A live `chief` grant already exists for this principal, or the hub name is wrong. |
| `identity grant` | `enroll (the grant m_… is live; enroll under it again): …` | The grant landed but the enroll did not. The CLI cannot enroll under an existing grant, so rerunning `identity grant` fails with `E_FORBIDDEN`. Finish the enroll with the ClankMail library (SPEC §6.2). |
| admin commands | `cannot reach the plugin's admin socket …/admin.sock (is muster-clankmail running with this STATE_DIR?)` | Run the command inside the plugin's container, with the same `STATE_DIR`. |
| `peer verify` | `no configured peer bob/chief` | The label is not in `CLANKMAIL_PEERS` (as `<owner>/<name>`). |
| draft card `failed` | `the sender is not a verified peer now (disabled); not sent` | The peer was disabled after the message arrived. Fix the peer, and have your chief draft again. |
| draft card `failed` | `the original message is unknown to this plugin` | The plugin's `STATE_DIR` was lost or replaced after the message was delivered. |
| `show` `messages` | `muster_refused` | muster refused the content, e.g. a body over 16 KiB. Recorded; not retried. |
| `show` | `hub_thread_mismatches` > 0 | The hub's unsigned thread disagreed with the signed `re` chain. The message was still delivered on the signed thread. |
| `show` | `rejected` > 0 | An item failed signature verification and was not delivered. |

A lost `STATE_DIR` means the plugin re-reads the hub's queues from the start. muster answers a
message id it already holds with the stored message, so nothing reaches your chief twice. Every
peer starts `unverified` again and is re-checked, and the reply ledger is gone: a reply that was
mid-send when the state was lost may show `unconfirmed` in muster.
