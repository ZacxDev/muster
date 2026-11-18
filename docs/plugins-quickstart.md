# Build a plugin in 15 minutes

A **plugin** is a small program you run that connects muster to another
system: an inbox, a chat, a forum, a ticket tracker. It brings that system's
messages to your chief, and it sends the replies you approve. muster never
calls the plugin; the plugin calls muster for everything
([why](decisions.md#plugins-pull-muster-never-calls-out)).

This page walks the whole loop by hand with `curl`, then runs the same loop
with the [template plugin](#run-the-template-instead), a small Go program you
copy and turn into your own. Every step links to its section of the full
reference, [plugins.md](plugins.md).

```
your plugin --POST /plugin/v1/messages-->  muster --> your chief (or a session it routes to)
                                           muster <-- a reply draft
you approve the exact text on the Asks page
your plugin --POST /plugin/v1/drafts/claim--> muster   (long-poll)
your plugin: send it upstream
your plugin --POST /plugin/v1/drafts/{id}/result--> muster
```

## Before you start

- muster running on this machine (the one-line install:
  [quickstart-local.md](quickstart-local.md)). This page uses its address,
  `http://localhost:8105`; put yours in `MUSTER` below.
- **A chief**, to read the message, **with a credential**: a Claude
  subscription token (`claude setup-token`) or an OpenRouter key. On the
  Compose install that is the `chief` profile
  ([features.md](features.md#chief)). Without one, messages wait in a queue
  and nothing drafts a reply.
- **An agent runtime to write the reply.** The loop cannot finish without
  one: something has to write the text you approve. On the Compose install
  that is **one mirrored Claude Code session** with muster's tools
  ([attention.md](attention.md#mirrored-claude-code-sessions)): the Compose
  chief cannot write a reply itself, so it hands the message to one of your
  sessions, and the session writes the reply
  ([chief.md](chief.md#on-the-docker-compose-install)). **The one-line install
  does not set that session up**: do
  [The session that writes the reply](#the-session-that-writes-the-reply)
  below first. On Kubernetes the chief writes the reply itself, and you can
  skip this.
- `curl`, and `jq` for the examples that read an answer.

```sh
MUSTER=http://localhost:8105
```

### The session that writes the reply

On the Compose install, on the machine where your Claude Code session runs.
The one-line install starts `muster-mirror` **read-only** (its log says
`read-only`), and a read-only host refuses a routed message: the chief's
`route_to_session` answers `host_not_interactive`, "This host is read-only —
its muster-mirror was not started with -interactive.", and the message never
reaches the session. The session also needs muster's tools, which the install
does not register, to answer with `route_reply`.

1. **Let muster type into this host's windows, and turn on its tools.** Add
   two lines to `~/.config/muster-mirror/env` (the install keeps lines it did
   not write):

   ```sh
   MUSTER_MIRROR_INTERACTIVE=1
   MUSTER_MIRROR_MCP_LISTEN=127.0.0.1:47823
   ```

   ⚠ An interactive host is effectively a shell on this machine for whoever
   signs in to your muster: it can open interactive terminals here, not only
   paste a routed message
   ([mirroring.md](mirroring.md#read-only-interactive-and-sending)).
2. **Restart `muster-mirror`.** On Linux:

   ```sh
   systemctl --user restart muster-mirror
   journalctl --user -u muster-mirror -n 20
   ```

   On macOS the service holds a copy of the settings made when the installer
   last ran, so run the installer again instead (it also updates muster to
   the latest release); it keeps both lines and writes the interactive one
   itself:

   ```sh
   curl -fsSL https://github.com/ZacxDev/muster/releases/latest/download/install.sh | MUSTER_MIRROR_INTERACTIVE=1 sh
   ```

   If this machine joined a hub, add `MUSTER_JOIN_URL=<hub>` to that command
   (before `sh`, beside `MUSTER_MIRROR_INTERACTIVE=1`), as when you installed
   it. Without it, on a Mac with Docker, the installer starts a muster server
   on this Mac; the mirror token it kept from the hub gets a 401 from that new
   server, so it deletes the token file; it mints a token on the new server;
   and it rewrites `MUSTER_MIRROR_URL` in `~/.config/muster-mirror/env` to
   `http://localhost:<port>`, so this Mac's sessions leave the hub. Going back
   takes a new token minted on the hub ([Agent ask tool](features.md#agent-ask-tool)). Without
   Docker it stops before changing anything. On
   Linux without a systemd user session (WSL, a container), stop the
   `muster-mirror` you started by hand and start it again the same way.

   The log (`~/Library/Logs/muster-mirror.log` on macOS) now says
   `INTERACTIVE (-interactive)` and `MCP endpoint for this host's Claude Code
   sessions at http://127.0.0.1:47823/mcp (secret in <path>)`. Copy that
   `<path>`: the next step needs it, and it differs by OS
   ([Where the MCP secret is](features.md#where-the-mcp-secret-is); `muster
   doctor` prints it too).
3. **Register the endpoint with Claude Code**, for this user, by running the
   first line below, and add the second line, with `<path>` the secret's path
   from step 2, to the shell tmux starts (`~/.zshrc` or `~/.bashrc`), which
   hands each session the secret:

   ```sh
   claude mcp add-json --scope user muster '{"type":"http","url":"http://127.0.0.1:47823/mcp","headers":{"Authorization":"Bearer ${MUSTER_MIRROR_MCP_SECRET:-}","X-Muster-Pane":"${TMUX_PANE:-}","X-Muster-Tmux":"${TMUX:-}"}}'
   export MUSTER_MIRROR_MCP_SECRET="$(cat '<path>')"
   ```

   Keep the single quotes: Claude Code expands the `${…}` itself, in each
   session ([features.md](features.md#agent-ask-tool)).
4. **Start Claude Code in a new tmux window**, so it reads both. Its `/mcp`
   lists `muster` with `route_reply` among its tools.

## 1. Turn plugins on

Plugins are off until the server has `MUSTER_PLUGINS=1`. On the Compose
install, add these lines to the `.env` beside `docker-compose.yml` (in
`~/muster` after the one-line install):

```sh
MUSTER_PLUGINS=1
MUSTER_SESSION_ROUTING=1   # the Compose chief hands messages to a session; not needed on Kubernetes
```

Then restart, and check the boot log says so:

```sh
cd ~/muster && docker compose up -d --wait
docker compose logs muster | grep 'plugins: ENABLED'
```

A quick check from outside: with plugins on, a plugin route without a token
answers `401`; with them off, every plugin route answers `404`.

```sh
curl -s -o /dev/null -w '%{http_code}\n' "$MUSTER/plugin/v1/self"   # 401
```

Reference: [Plugins](plugins.md#plugins) (on and off),
[features.md](features.md#plugins).

## 2. Create the plugin and its token

A plugin has a **name** (lower-case letters, digits and hyphens), a **label**
(what your chief's conversation for it is called) and a token that starts
`mpl_`. The token acts as you, and opens `/plugin/v1/*` only.

**On the page.** Open `$MUSTER/plugins` (it is not in the menu), fill in
**Name** `echo-inbox` and **Label** `Echo inbox`, and press **Create plugin
token**. The token is shown **once**. Save it to a file only you can read:

```sh
umask 077 && cat > ~/.echo-inbox-token   # paste the token, then Ctrl-D
TOKEN=$(cat ~/.echo-inbox-token)
```

Reference: [Two ways in](plugins.md#two-ways-in),
[Capabilities](plugins.md#capabilities).

**Or pair it, and never handle the token.** The plugin asks for its own
token, you approve it by typing the code it printed, and the plugin collects
the token straight from muster. `owner` is your login: the email you sign in
with, or, on an install with one shared password, `operator`.

```sh
curl -s -X POST "$MUSTER/plugin/v1/pair" -H 'Content-Type: application/json' \
  -d '{"owner": "operator", "name": "echo-inbox", "label": "Echo inbox", "capabilities": ["messages"]}' \
  | tee /tmp/pair.json
# {"request_id": "…", "code": "ABCD-EFGH", "poll_secret": "…", "expires_at": "…"}
```

Open `$MUSTER/plugins`, type the `code` into the pending request's **Code the
plugin printed** field, and press **Approve**. Approving needs a recent
sign-in, as approving a draft does ([step 5](#5-approve-the-draft)); an
older one asks you to confirm it is you first.

No pending request on `/plugins`? Check the `owner` you sent. An `owner` muster
does not know gets the same answer as a real one (a code and a poll secret,
so the route does not reveal who has an account here), but no request is
made, and its poll says `pending` until it expires.

Then poll. The answer carries the token **once**: the next poll says
`redeemed`. So write it straight to a file only you can read, as it arrives.
It goes to a temporary file first, and replaces `~/.echo-inbox-token` only if
a token arrived, so a poll that answers `redeemed` or fails on the network
never empties a good token file:

```sh
( umask 077
  curl -s "$MUSTER/plugin/v1/pair/$(jq -r .request_id /tmp/pair.json)" \
    -H "Authorization: Bearer $(jq -r .poll_secret /tmp/pair.json)" \
    | jq -r '.token // empty' > ~/.echo-inbox-token.tmp
  if [ -s ~/.echo-inbox-token.tmp ]; then mv ~/.echo-inbox-token.tmp ~/.echo-inbox-token
  else rm -f ~/.echo-inbox-token.tmp; echo 'no token in this answer'; fi )
TOKEN=$(cat ~/.echo-inbox-token)
# the answer was {"status": "approved", "token": "mpl_…", "plugin_id": 1, "plugin_name": "echo-inbox", …}
```

`no token in this answer` means the answer carried none, and the token file
was left as it was. Most often you polled before
approving (`{"status": "pending", …}`): approve, then run it again. After
`redeemed` (the token went to an earlier poll), `denied` or `expired`, revoke
any plugin it minted and pair again. A request expires after 10 minutes.
Reference:
[Pairing a plugin without handling its token](plugins.md#pairing-a-plugin-without-handling-its-token).

**Check the token.** `GET /plugin/v1/self` says who it is:

```sh
curl -s "$MUSTER/plugin/v1/self" -H "Authorization: Bearer $TOKEN"
# {"plugin_id": 1, "plugin_name": "echo-inbox", "owner_id": 1, "owner_login": "operator",
#  "owner_kind": "person", "capabilities": ["messages"], "deliver_to": null}
```

Reference: [`GET /plugin/v1/self`](plugins.md#get-pluginv1self-who-the-token-is).

## 3. Post an inbound message

```sh
curl -s -X POST "$MUSTER/plugin/v1/messages" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"external_id": "echo-0001", "sender": "someone@example.com",
       "subject": "hello", "body": "Is the echo plugin working? A one-line reply is fine."}'
# {"id": 1, "status": "queued"}    (202)
```

- `external_id` is your upstream's id for the message. It is the idempotency
  key: send the same one again and you get `200` with the stored message, and
  nothing is delivered twice. So if you did not see an answer, send it again,
  and move your upstream cursor on only after a `2xx`.
- `body` is required (up to 16 KiB). A message over a limit is refused, never
  cut.
- `429` means 60 new messages in the last hour, or 200 still waiting: wait for
  the `Retry-After` header.

Reference: [`POST /plugin/v1/messages`](plugins.md#post-pluginv1messages-an-inbound-message).

## 4. Watch the chief handle it

muster hands the message to your chief as a turn of its own, fenced and marked
as untrusted data from an outside sender. The sender is marked
`UNVERIFIED`, because the plugin did not say it authenticated them
([`sender_verified`](plugins.md#sender_verified)).

What the chief does next is its own judgement:

- **On the Compose install** it hands the message to the session best placed
  to answer it (`route_to_session`). You see the message queued in that
  session's window, and pasted once Claude Code is idle. The session answers
  with its `route_reply` tool, and that answer becomes a **reply draft**. If
  the chief leaves it, ask it in its chat to route message 1 to your session.
  If it says the route was refused with `host_not_interactive`, that host's
  `muster-mirror` is read-only: see
  [The session that writes the reply](#the-session-that-writes-the-reply).
- **On Kubernetes** the chief can also draft the reply itself (`reply_draft`).

Either way nothing is sent: a draft waits for you. You get a push, "A reply
draft awaits your approval".

Reference: [Delivery to your chief](plugins.md#delivery-to-your-chief),
[Routing a message to a session](plugins.md#routing-a-message-to-a-session),
[Replying: drafts you approve](plugins.md#replying-drafts-you-approve).

## 5. Approve the draft

Open the Asks page's **Drafts** tab, `$MUSTER/attention?tab=drafts`. The card
quotes the message, and shows the reply **exactly as it will be sent**, every
character that is not plain ASCII spelled out. Press **Approve and send**. It
needs a recent sign-in: from the last 12 hours by default, a window the
server's `MUSTER_STEPUP_WINDOW` can change
([configuration.md](configuration.md#muster-server-sign-in-and-machine-credentials)). An
older one asks you to confirm it is you first. **Reject** needs none.

From one of your own mirrored Claude Code sessions you can decide it instead,
with `plugin_drafts_list` and `plugin_draft_decide`.

Approving queues the reply; your plugin sends it.

Reference: [The card](plugins.md#the-card),
[Deciding a draft from your own session](plugins.md#deciding-a-draft-from-your-own-session).

## 6. Claim it

The plugin long-polls for approved replies. `wait_seconds` is 0 to 25; give
your HTTP client a timeout at least 10 seconds longer.

```sh
curl -s -X POST "$MUSTER/plugin/v1/drafts/claim" --max-time 35 \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"wait_seconds": 25}' | tee /tmp/claim.json
# {"draft_id": 1, "kind": "reply", "message_external_id": "echo-0001",
#  "body": "Yes, it is working.", "approved_at": "…"}
```

- `200` hands you **one** approved reply, leased to you for 2 minutes. It is
  never offered again, even if you never report on it.
- `204` (no body) means nothing was approved within the wait: call again.
- Each claim also retries delivering your queued messages to the chief, so a
  plugin that keeps long-polling keeps delivery moving.

`message_external_id` is the message the reply answers. Send `body` upstream,
as it is. Record the `draft_id` somewhere durable **before** you send, and
never send the same `draft_id` twice.

Reference: [`POST /plugin/v1/drafts/claim`](plugins.md#post-pluginv1draftsclaim-collect-an-approved-reply),
[At most once](plugins.md#at-most-once).

## 7. Report the result

```sh
curl -s -X POST "$MUSTER/plugin/v1/drafts/$(jq -r .draft_id /tmp/claim.json)/result" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"ok": true, "external_ref": "upstream-id-123"}'
# {"draft_id": 1, "status": "sent"}
```

`ok: false` with a `detail` says it was not sent, and the reply goes back to
whoever wrote it to try again. The same result twice is safe, so post it again
if you did not see an answer. No result within the 2-minute lease leaves the
reply **unconfirmed**: muster cannot know whether it went, so it never offers
it again. A send that takes longer renews the lease
([renew](plugins.md#post-pluginv1draftsidrenew-keep-a-long-sends-claim)).

The card on the Drafts tab now shows it as sent.

Reference: [`POST /plugin/v1/drafts/{id}/result`](plugins.md#post-pluginv1draftsidresult-report-what-happened),
[When the plugin refuses a reply](plugins.md#when-the-plugin-refuses-a-reply).

## Run the template instead

The template does steps 3, 6 and 7 in a loop: a fake upstream produces a
message every so often, the plugin posts it, claims what you approve, "sends"
it by printing it, and reports the result. It is
[muster-plugin-template](https://github.com/ZacxDev/muster-plugin-template), a
single Go program with no dependencies, tests against a fake muster, and a
README on turning it into your own plugin.

```sh
git clone https://github.com/ZacxDev/muster-plugin-template && cd muster-plugin-template
MUSTER_URL=$MUSTER MUSTER_PLUGIN_TOKEN=$TOKEN go run .
```

Configuration is environment variables only; `.env.example` lists them. To
make it yours, replace the fake upstream with your system's reads and the
printed send with your system's writes, and keep the loop: post, claim, send
once, report.

## Next

- A plugin that works for one specialised officer instead of your chief, and
  posts the officer's drafts:
  [officer-plugin-recipe.md](officer-plugin-recipe.md).
- What else you can build on muster, and how to list your plugin:
  [ecosystem.md](ecosystem.md).
- A reply rule your upstream has (a length, no Markdown): let the plugin check
  each draft before you can approve it
  ([`draft_checks`](plugins.md#check-a-reply-before-it-can-be-approved)).
- Senders you can authenticate (DMARC-aligned mail, a signed protocol):
  [`sender_verified`](plugins.md#sender_verified).
- Notices that map to one task each ("ticket 4711 was assigned to you"):
  [`POST /plugin/v1/tasks`](plugins.md#post-pluginv1tasks-one-task-per-upstream-record).
