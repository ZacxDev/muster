# Accounts, sign-in and tokens

## Two modes

`MUSTER_AUTH_MODE` selects one. Any other value refuses to boot.

### `password` (the default)

- One shared operator password, `MUSTER_UI_PASSWORD`, acting as **user 1**, the
  admin.
- If it is unset or shorter than 12 characters, every browser page answers 503
  and `/login` says which variable is missing. It fails closed.
- `POST /login` compares in constant time and is limited to 10 attempts a
  minute.
- The session cookie lasts 30 days. Set `MUSTER_SECURE_COOKIES=1` when muster
  is served over TLS.

### `gotrue`

Per-user accounts through [GoTrue](https://github.com/supabase/auth), the
Supabase auth server.

- **Required:** `MUSTER_GOTRUE_URL`, `MUSTER_GOTRUE_JWT_SECRET` (HS256, at least
  32 characters), `MUSTER_HOOK_TOKEN` and `MUSTER_BROWSER_PORT`.
- **Tokens GoTrue must issue:** issuer `muster-auth`, audience `authenticated`.
- **The browser never talks to GoTrue.** muster calls it server-side and keeps
  the session in `HttpOnly`, `Secure` cookies, which it refreshes itself.
- **Two listeners.** `MUSTER_BROWSER_PORT` is the only listener that serves
  browser sessions. The main port then serves machine tokens only. On the browser
  listener, the shared hook and service secrets are refused with 403.
- **Throttling** is per account: 3 free failures, then a backoff that doubles up
  to 5 minutes. There is also a limit per client address.

### Setting up the first admin

With the GoTrue variables set, `muster-server invite --claim-admin` prints a
single-use link, valid for 24 hours. Opening it binds a new GoTrue login to
user 1.

The GoTrue pair also works in password mode, where it only makes that claim link
usable. That lets you bind the operator's login before you switch modes.

## Users, roles and invites

- **Roles:** there are two, `admin` and `member`.
- **Invites:** an invite always creates a member. Links last 24, 72 (the
  default) or 168 hours, and carry their token in the URL fragment.
  Invites are managed on `/admin/users`.
- **Admin pages:** promote a member, disable or enable a user, send a reset
  link, delete a user's passkeys. The last admin cannot be demoted.
- **From the command line:** `muster-server user disable|enable <id>` does the
  same as the admin page. A running server notices within 10 seconds for open
  streams and within 30 seconds for new requests.
- **Data is per user.** Every user-data row has an `owner_id`, enforced by
  Postgres row-level security (see [schema.md](schema.md)). Each user has their
  own tasks, agents, tokens and chief. An admin's ordinary pages show only the
  admin's own rows; the cross-user views are on `/admin/*`.
- **Credentials:** members bring their own Claude and OpenRouter credentials
  (`/account/credentials`, which needs the connector key). The pool configured in
  the server's environment belongs to the admin.

## Passkeys

Passkeys are GoTrue's own WebAuthn passkeys, proxied through muster. They are
offered only in gotrue mode, and only when GoTrue reports `passkeys_enabled`.

- muster refuses any assertion without user verification.
- A passkey is an **additional** way in. The password stays mandatory.
- A password reset **keeps** the user's passkeys by default. The reset page has
  an unticked box, **Also remove all my passkeys (a device was lost or
  stolen)**. Tick it and the reset deletes every passkey before it sets the
  password.
- Ticked or not, the reset first ends every session and revokes the user's
  API tokens and paired phones. It then sets the password and writes the
  `user.password_reset` audit row. The row's detail records `remove_passkeys`,
  and when the box was ticked, `passkeys_removed` (how many).
- If the box was ticked and a passkey cannot be deleted, the reset stops
  there. The sessions, tokens and phones are already gone, but the password is
  NOT changed and NO audit row is written. Any passkeys not yet deleted remain,
  and the person needs a new reset link.
- The account page the reset lands on says how many passkeys were removed, or,
  with the box unticked, that they were kept. It says "kept" only when the
  person has at least one: after the password is set, the reset counts them
  through GoTrue's admin API (a read; it deletes nothing). If that count fails,
  the page says "Any passkeys you have were kept", and the reset is not held up.
- The box is on every reset page. It does not depend on GoTrue reporting
  passkeys as enabled, because passkeys made while they were on can outlive
  that setting.
- You manage your passkeys at **Account → Passkeys** (`/account/passkeys`).
  The link is on the Account page of every gotrue-mode server, whether or not
  GoTrue reports passkeys as enabled; with them off the page says so. Adding
  one needs a sign-in from the last five minutes.
- Signing in with a passkey needs no username. The sign-in page offers three
  ways:
  - **Autofill.** The email field is `autocomplete="username webauthn"`, and
    where the browser supports conditional mediation
    (`getClientCapabilities().conditionalGet` or
    `isConditionalMediationAvailable()`) the page starts a conditional
    request on load. The browser then lists the account's passkeys in the
    field's suggestions, and picking one signs in. Submitting the password
    form aborts that request before the form posts.
  - **Sign in with a passkey.** The button first aborts the pending autofill
    request (a page can run one WebAuthn request at a time). That abort shows
    nothing. Each request uses a challenge of its own, and a new one is
    fetched when a request ends.
  - **Use a passkey from my phone.** The same request with the WebAuthn
    Level 3 hint `hybrid` (Chrome 128 and later), which goes straight to the
    phone's QR code. A password-manager extension that intercepts the request
    may ignore the hint and show its own window. That window's "use another
    device" option hands over to the browser's.
- When no passkey was used (the sheet was closed, the device has none for
  this site, or it timed out) the page says "No passkey was used" and lists
  the other ways in. It does not say "cancelled", because the browser does
  not tell the page which of those happened. A passkey that was checked and
  refused shows the server's reason.
- Passkeys can also confirm a step-up; see
  [sending-and-step-up.md](sending-and-step-up.md).

### Keeping password managers in step (the Signal API)

A removed passkey stays in the person's password manager or phone, which keeps
offering it. Where the browser has the WebAuthn Signal API, muster tells it.
Every call is feature-detected, and a browser without the API is left as it
was.

- `/account/passkeys` calls `signalAllAcceptedCredentials` with every passkey
  the account has, and `signalCurrentUserDetails` with its email. A removal
  redirects there, so the removed passkey is signalled away on the next page.
- The page a reset that removed passkeys lands on signals the passkeys GoTrue
  still lists (none, straight after the reset). The set comes from GoTrue, not
  from the page's query string.
- A passkey sign-in that GoTrue refuses calls `signalUnknownCredential`, but
  only when muster can show the passkey is not the account's. GoTrue answers
  every failed passkey sign-in the same way, so muster checks the user the
  passkey names: no such user, a user with no passkeys, or a user whose
  passkeys all have known ids and none is this one. The page then says "That
  passkey is no longer registered here."

The calls need the user handle and the credential ids. GoTrue v2.197.0
registers the user handle as the GoTrue user id's text, so muster derives it.
GoTrue's passkey list does not return credential ids, so muster records each
one in the user's GoTrue `app_metadata` (`muster_passkeys`, beside the backup
flags) when the passkey is added. A passkey added before muster recorded ids
gets its id at its next sign-in, when it is the account's only passkey without
one. 🔴 Until every passkey of an account has a known id,
`signalAllAcceptedCredentials` is not sent for that account: a partial list
would tell the password manager to drop a passkey that still works. The RP ID
is read from the options GoTrue hands out.

### Passkeys in the Android app

The [Android app](design/android.md)'s WebView runs WebAuthn as the app
(`WEB_AUTHENTICATION_SUPPORT_FOR_APP`). Two things differ from a browser:

- Android runs the ceremony only if the site lists the app.
  `MUSTER_ANDROID_PACKAGE` and `MUSTER_ANDROID_CERT_SHA256` make muster serve
  `/.well-known/assetlinks.json` with the `get_login_creds` relation only
  ([configuration.md](configuration.md)): not `handle_all_urls`, since the app
  handles no links. It is
  open, `application/json`, and your edge must serve it with no redirect.
- The assertion's `clientDataJSON.origin` is
  `android:apk-key-hash:<the signing certificate's SHA-256, unpadded base64url>`,
  not `https://…`. muster does not read that origin; GoTrue checks it against
  `GOTRUE_WEBAUTHN_RP_ORIGINS`. In GoTrue v2.197.0 (go-webauthn v0.16.5), an
  origin that does not start with `http://` or `https://` is compared as an
  exact string, so add that value to the list. The RP id stays the site's host.

Passkey autofill (conditional mediation) is not available in a WebView; the
buttons are.

## Machine credentials

| credential | acts as | notes |
|---|---|---|
| Personal API token (`mst_…`) | you: your user id and role | Minted only in the browser at `/account/tokens`, and shown once. Only its sha256 is stored. A token cannot mint another token. Revoking one refuses it on the next request. Disabling a user revokes all of their tokens, and re-enabling them brings none back. |
| `MUSTER_HOOK_TOKEN` | user 1, admin | A shared secret, enforced only when set; while unset, the hook routes are open. gotrue mode requires it. |
| `MUSTER_SERVICE_TOKEN` | user 1 | It also requires an `X-Muster-Actor` header. It fails closed (503 while unset) and must be at least 32 characters. |
| Mirror token (`mmt_…`) | the host it was minted for | Opens `GET /mirror/connect`, the `AskUserQuestion` hook's `/mirror/ask` routes and `POST /mirror/mcp`. Through `/mirror/mcp` a leaked one reaches, beyond the host's tunnel and its questions, the attention asks of that host's own Claude Code sessions (file, wait, cancel), the tasks those sessions created, and — through `attention_answer` — an answer as the operator to any of its owner's open asks not raised by a mirrored session on that host (recorded as via MCP from that host; accepted like any of your answers, **including as `agent_dispatch` instructions** — the accepted risk in [attention.md](attention.md#mirrored-claude-code-sessions)) — and, through `connector_request_decide`, an approval as the operator of any of its owner's agents' pending connector requests (recorded and shown as via MCP from that host; the accepted risk in [connectors.md](connectors.md#deciding-a-request-from-your-own-session)), and — through `plugin_drafts_list` — a READ of every one of its owner's pending plugin reply drafts (the inbound message's sender, subject and full text, and the reply), and through `plugin_draft_decide` an approval as the operator of any of them, which sends that text to an outside party with no fresh sign-in (recorded on the audit row as via MCP from that host; any of the owner's mirrored sessions may approve, the one a message was routed to included; the accepted risk in [plugins.md](plugins.md#deciding-a-draft-from-your-own-session)), and — through `agent_destroy` — the **teardown of any of its owner's agents but the chief** (the tasks are kept; **no audit row**: the host is named only in muster's server log; also an accepted risk, by operator decision — [attention.md](attention.md#mirrored-claude-code-sessions)) — and, through `chief_restart`, a **restart (Stop then Start) of its owner's own chief**, which with `force` cuts off a turn in flight (the same as the agent page's Stop/Start, which need no fresh sign-in either; **no audit row**: the host is named in muster's server log — [chief.md](chief.md#restarting-the-chief)) — and, with the chief's dispatch on, through `dispatch_request` and `dispatch_approve`, the **start of a new agent on its owner's behalf** with a prompt of the caller's choosing, within the chief's caps (each request and approval is an audit row naming the host and session; each start notifies the owner — [chief.md](chief.md#starting-an-agent-from-your-own-session)) — never another task (but for the comment `route_reply` adds to a task the chief [routed](plugins.md#routing-a-task-to-a-session) to one of that host's sessions), another host's session, another owner's ask or request, a secret, or a chief tool. See [mirroring.md](mirroring.md) and [attention.md](attention.md#mirrored-claude-code-sessions). |
| Flow runner token (`mfr_…`) | its owner, confined to one host | See [browser-flows.md](browser-flows.md). |
| Plugin token (`mpl_…`) | its owner, on `/plugin/v1/*` only | Minted at `/plugins` and shown once. Every other route refuses it. See [plugins.md](plugins.md). |
| Device credential (`mdv_…`) | its owner, on `/api/device/*` only | A phone paired with the [Android app](design/android.md). See [Paired devices](#paired-devices). Every other route refuses it. |

The two shared secrets may not start with `mst_`, `mpl_` or `mdv_`. The server
refuses to boot if one does.

### Mirror tokens and session routing

With [session routing](plugins.md#routing-a-message-from-your-own-session) on,
`POST /mirror/mcp` also offers `plugin_messages_list` and `plugin_message_route`.
Through them a mirror token **reads its owner's 30 most recent routable inbound
plugin messages** (the sender with its `UNVERIFIED` mark, the subject, the first
280 characters of the body, the route state) and **routes any of them into any
of its owner's mirrored Claude Code sessions that can take one**. The text
pasted is always the stored message, fenced as untrusted, never words the
caller writes, and the `session_route.routed` audit row names the host and
session the route came from.

🔴 **Accepted risk: a mirrored session can route a message to itself and
answer it.** Any of the owner's mirrored sessions — or anyone holding a leaked
mirror token — can route any of the owner's recent repliable messages **to
itself** and answer it with `route_reply`. When the sender is verified and on
the owner's allowlist for that plugin (`MUSTER_ROUTE_RELAY_ALLOW`), that answer
is **sent with no human review**. For any other sender it becomes a pending
reply draft for the owner — and **if this muster has the `plugin_draft_decide`
tool**, the same session can approve that draft, its own reply, over the same
door, so that reply too reaches the outside sender with no other review.
Revoke a token you suspect.

### Threads a session started

`POST /api/plugin-threads` ([plugins.md](plugins.md#a-reply-on-a-thread-a-session-started))
takes route reply's credential: a personal API token, or the hook token. 🔴
**Accepted risk: whoever holds it can bind any thread id of its owner — for
agent mail or ClankMail, the two thread plugins — to any of the owner's online
mirrored sessions** — the first report for a
thread wins — and every later reply on that thread is then pasted into that
session **with no chief and no owner in between**, fenced as untrusted like
every routed message, and answered through the ordinary relay (approved
automatically only for a verified, allowlisted sender). The thread id is not
checked against the upstream: muster cannot know who started a thread. A
binding is never replaced; to undo one, delete its `plugin_threads` row.

⚠ **One thread namespace for both thread plugins.** `plugin_threads` is keyed
by owner and thread id, with no plugin column, so a binding routes a reply that
carries that thread id from **either** `agent-mail` or `clankmail`. muster does
not check which plugin a bound thread belongs to: a thread plugin that let a
sender choose its `thread` verbatim could aim a reply at a session bound for
the other plugin's thread. A thread plugin must report only a thread id it
derived itself (for ClankMail, the root found by following verified `re`
links, never the hub's unsigned `thread` field).

### Pull-request review mail

A pull-request review ([plugins.md](plugins.md#a-pull-request-review-mail)) is
pasted into the session that authored the pull request **with no chief and no
owner in between**, fenced as untrusted like every routed message. Two things
decide it: the mail's sender, which must be on `MUSTER_PR_MAIL_SENDERS` **and**
marked `sender_verified` by the agent-mail plugin (a verified agent-mail
sender proves its owner; its handle is that owner's gateway's claim) —
**never** a ClankMail message, verified or not, since a ClankMail handle is
claimed on a hub and proves nothing about who that owner is to this
deployment — and the
`repo`/`pr`/`head_ref` its body names. 🔴 **Accepted risk: a listed sender can
put a message into whichever of your sessions created, or pushed the branch
of, a pull request it names** — never another owner's session, and never one
that only viewed or mentioned it. The authorship rows come only from `POST /api/pr-sessions`, with route
reply's credential (a personal API token or the hook token); nothing a mail
says can write one. 🔴 **Accepted risk: whoever holds that credential can
record any of the owner's Claude session ids as the author of any pull
request or branch**, and the next review of it is then pasted into that
session — the thread bind's risk, for review mail.

### Fleet-report requests

A [fleet-report request](plugins.md#a-fleet-report-request) is answered with
**no chief and no owner in between**: muster files an already-approved reply
holding the names, repositories and focus titles of the owner's sessions that
work under `MUSTER_FLEET_REPORT_PATHS`, over the requested window. One thing
decides it: the mail's sender, which must be on `MUSTER_FLEET_REPORT_SENDERS`
**and** marked `sender_verified` by the agent-mail plugin (never a ClankMail
message, for the reason above). The request's
`member` is not checked: every read runs as the owner the plugin token acts
for, under row-level security, so no request can select another owner's rows.
No host label is sent, and titles are not screened except for secret-shaped
strings. 🔴 **Accepted risk: a listed sender reads the owner's work-session
titles without approval**, for any window of up to four days.

## Paired devices

The native Android app gets its own credential, the **device credential**
(`mdv_…`), one per paired phone. It opens `/api/device/*` and nothing else.

**Pairing.** A signed-in page inside the app asks for a one-time pairing code
(`POST /account/devices/pair`). Any valid signed-in browser session may ask,
however long ago it signed in: there is no recent-sign-in window, unlike adding
a passkey (an operator decision). The session must still be valid, and what
that means depends on the mode. In gotrue mode a session that is expired,
signed out, or from before Sign out everywhere or the session cutoff is
refused, and the session's sign-in time is stored with the phone, so those
revocations still reach it. In password mode the session is a stateless cookie
valid for 30 days: Sign out only clears that browser's cookie and there is no
Sign out everywhere, so a copy of the cookie can still pair a phone until it
expires, and such a phone is cut off only by Remove on Account → Devices. A
machine token, the hook token or a device credential cannot ask for
a code. The code is single use and expires after 60 seconds. The app redeems it at
`POST /api/device/pair` with the phone's name, its app version and its step-up
key: an EC P-256 public key the app generates in the Android Keystore at
pairing, its type, and the Keystore security level the app reports. The
credential is shown to the app once. Only its sha256 is stored.

**The step-up key** ([design](design/android.md#the-step-up-key)):

- A `biometric` key (a fresh strong biometric on every use) is accepted.
- A `biometric_or_credential` key (the device PIN also works) is accepted only
  when `MUSTER_ANDROID_ALLOW_PIN_KEYS` is set. It is off by default, because such
  a key survives a new fingerprint being enrolled.
- A key the app reports as `software` is refused. Until key attestation exists,
  muster trusts the app's report.
- A phone without an acceptable key pairs **read-only**, with no key.

- **A device credential cannot mint another.** Only a browser session can ask
  for a pairing code.
- **It is confined to `/api/device/*`.** Every other route refuses it, and the
  device routes refuse every other credential, a session cookie included. Two
  device routes take no credential at all: `POST /api/device/pair` (the code
  is the authority) and `GET /api/device/web` (configuration only, below).
- **Which listener.** In gotrue mode the phone reaches muster through the public
  edge, so the device routes are served on the browser listener as well. The
  shared hook and service secrets stay refused there.

**Revocation.** These end a phone, and each one **deletes** it: its row, its
push registration (so it gets no more notifications), its place in the 20-phone
limit and its line on Account → Devices.

- you remove it on **Account → Devices**, or the app unpairs itself
  (`DELETE /api/device`);
- the account is disabled;
- a password reset (it moves the account's session cutoff);
- **Sign out everywhere** (gotrue mode only). This is the design's
  recommendation, not an operator requirement: it is what "everywhere" is
  expected to mean for a phone. It is stricter than for personal tokens, which
  Sign out everywhere does not revoke. If muster cannot record the sign-out, or
  cannot tell who you are, the page says it did not finish rather than
  pretending it did.

As a second wall, every request also checks the account from the database,
with nothing cached: a phone whose pairing sign-in is at or before the account's
session cutoff or latest Sign out everywhere, or whose account is disabled, is
refused, and is not sent notifications, even if its row somehow survived.

**Throttles.** Pairing is limited to 10 attempts a minute per client address.
A device route request with a valid credential is charged only to its phone:
60 a minute. A refused request (no, a malformed, or an unknown or revoked
credential) is charged to its address: 120 a minute, then 429. So junk sent
from an address a phone shares cannot lock that phone out. Registering push,
which makes muster send a test push, is limited to 5 a minute per phone. The
address is the one the edge reports: on the browser listener that is
`CF-Connecting-IP` (or the first `X-Forwarded-For` entry), which only an edge in
front of muster can be trusted to set.

**Account → Devices** (on `/account`) lists each phone: its name, app version,
when it was paired, when it was last seen, whether push is registered, and its
step-up key type as the app reported it (biometric, biometric or PIN, or none:
read-only), labelled "(reported)" until key attestation exists. Each
has a **Remove** button. Removing a phone also removes its push registration. A
user may pair at most 20 phones.

The list updates itself while the page is open. A phone registers push a few
seconds after it pairs, so while something is pending the list re-fetches
(`GET /account/devices/live`, the signed-in user's own devices) every 3 seconds
while the tab is visible. Pending means a phone paired in the last 10 minutes
with no push registration, on a server that serves push. (The app reloads the
page as soon as it has paired, which lists the new phone.) Each response re-decides, so the polling
stops once nothing is pending; a page with nothing pending never polls. The list
also re-fetches once whenever the tab becomes visible again. The same response
updates the Notifications section's device list, so **Send a test notification**
appears as soon as the phone is registered.

The device routes:

| route | what it does |
|---|---|
| `GET /api/device/web` | **Open: no credential.** The [sign-in origins](#sign-in-origins-for-a-single-sign-on-proxy), as `{"sso_origins": ["https://…", …]}` (`[]` when none are set), with `Cache-Control: no-store`. |
| `GET /api/device/config` | The server version; push (whether it is served, the VAPID public key the app registers with, and the operator's push hosts, for the app's pairing check); whether PIN keys are allowed; and the feature switches (`push`; `actions`, true where the attention queue is served and the signed ask actions with it; `bubbles` and `state`, false until their phases ship). |
| `POST /api/device/challenge` | A one-time challenge (2 minutes) for one signed action, bound to the action, its target and the sha256 of the exact text it carries. The phone's step-up key signs the returned `message`. Refused to a phone with no step-up key. |
| `POST /api/device/actions/attention/asks/{id}/answer` | Answers an ask with `{nonce, signature, answer}` — the same answer as the Attention page's, recorded only when the signature verifies over the challenge. Audited as `device.action` once it has run, with its outcome (the ask's new status, or the refusal's code). |
| `POST /api/device/actions/attention/asks/{id}/cancel` | Dismisses an ask with `{nonce, signature}`, signed the same way. |
| `PUT /api/device/push` | Registers the phone's UnifiedPush endpoint `{endpoint, p256dh, auth}`. The endpoint must be on an allowed push host ([push.md](push.md#which-push-services)), and muster keeps it only after a test push to it succeeds. A phone holds one registration; a new one replaces it. |
| `DELETE /api/device/push` | Removes the phone's push registration. |
| `DELETE /api/device` | Unpairs the phone. |

The signed actions' wire contract (the message format, the signature, the
refusal codes) is in [design/android.md](design/android.md#the-signed-ask-actions-as-built).

### Sign-in origins for a single sign-on proxy

An option for deployments behind a single sign-on proxy: a gate at the edge
that answers muster's first page with a redirect to an identity provider. The
Android app shows muster in a WebView and opens every other origin in the
browser, so without this the sign-in would run in the browser, its cookies
would land there, and the app would never reach muster.

`MUSTER_ANDROID_SSO_ORIGINS` ([configuration.md](configuration.md)) lists the
origins that sign-in passes through. muster serves them at
`GET /api/device/web`:

```json
{"sso_origins": ["https://sso.example.com", "https://idp.example.com"]}
```

- **No credential.** The app reads the list before the first sign-in, and so
  before pairing. Your proxy must therefore let `/api/device/` through without
  its own sign-in, which the device routes need anyway. Let **only**
  `/api/device/` through: everything else stays behind the proxy's sign-in. (Optionally `/api/instance` and `/brand/` too, so the app
  shows the server's [name and logo](branding.md) before signing in; they hold
  nothing per-user.)
- **Keep `/manifest.webmanifest` behind the sign-in.** The app checks whether
  the proxy's session is still alive by fetching that file without following
  redirects: muster serves it with no session of its own, so only the proxy
  can answer it with a redirect or a 401, and that answer is what sends the
  page back to sign-in. A proxy that lets the file through always answers 200,
  and an expired session then leaves the page stalled with no sign-in.
- **Only the list.** The answer holds nothing else and nothing per-user, and it
  is sent with `Cache-Control: no-store`, so a change reaches the phone the
  next time it opens muster.
- **A page load, nothing more.** The app lets those origins load in this
  server's WebView, in its own profile. Its bridge, document-start scripts,
  microphone and upload routes stay on muster's own origin.
- **List every origin the sign-in visits in the WebView**, in order: the
  proxy's sign-in host if it differs from muster's, any sign-in hub it sends
  to, and the identity provider's own pages. An origin left out opens in the
  browser, and the sign-in does not come back to the app.
- **Listing an identity provider's origin moves every link to it into the
  app's WebView**, not only the sign-in: a link on any muster page to that
  origin (a repository, a profile) then opens inside the app instead of the
  browser. List only the providers your sign-in needs, ideally one.
- **Not every identity provider allows a WebView.** Some refuse sign-in from an
  embedded WebView by policy; pick one that allows it.

The app's side, including what a sign-in origin may not do (no file picker, no
passkeys, no bridge), is in the muster-android README, section
[A server behind a sign-in gate](https://github.com/ZacxDev/muster-android#a-server-behind-a-sign-in-gate).

Each entry is an exact `https` origin: no path, query, fragment, user info or
wildcard, and at most 8. Its host is a DNS name whose last label contains a
letter, a dotted-quad IPv4 address, or a bracketed IPv6 address. A host a
browser would rewrite is refused: an IPv4-mapped IPv6 address
(`[::ffff:1.2.3.4]`) or a name that looks numeric (`127.1`, `0x7f.0.0.1`,
`1.2.3.4.5`, or any label that is all digits or `0x` hex). A malformed value stops the server from starting.
Unset, the route answers `{"sso_origins": []}` and the app behaves as before.

Two endpoints help with personal tokens:

- `GET /api/whoami` reports who a credential acts as.
- `GET /api/tokens` and `DELETE /api/tokens/{id}` manage personal tokens. They
  accept a personal token only.

**Token scopes.** When you mint a token you choose its access:

- **Full access** (the default): every route a personal token reaches, as
  before scopes existed. Every token minted before them is full access.
- **Bar summary only**: [`GET /api/attention/summary`](attention.md#the-summary-api)
  (and `HEAD` on it) and **nothing else**. Every other route a personal token
  reaches answers **403** before it runs: the task, agent and flow APIs, the
  token API, and `GET /api/whoami` too (the summary's own 200, 401 or 403
  says whether the token works). Give a status bar this kind.
- **Create agents** (`agent-create`): [`POST /api/agents`](provisioner.md#creating-an-agent-without-the-chief)
  and **nothing else** (every other route answers **403**). It is also the
  only token that reaches that route: a **full-access token is refused there**,
  so the CLI's ordinary token cannot create agents. `muster agent create`
  reads it from `MUSTER_AGENT_CREATE_TOKEN`.
- **Account usage only** (`account-usage`): [`GET /api/cc-accounts/usage`](jobs.md#get-apicc-accountsusage),
  your Claude accounts' utilization and who used them, and **nothing else**
  (every other route answers **403**). Give it to a service that should read
  that and hold nothing else. A full-access token reaches the route too.

The allowed routes are one list in `internal/api/api_tokens.go`
(`tokenScopeRoutes`), checked where every personal token is admitted, so a
route added later is refused to a narrower token until someone adds it
there; a route listed in `explicitScopeRoutes` is refused to a full token too.
The token page and `muster token ls` (`"scope": "full"`, `"summary"` or
`"agent-create"`, `"account-usage"`) show each token's scope, and `muster whoami` shows the
presented (full) token's.

From the CLI these are `muster whoami`, `muster token ls` and
`muster token revoke <id>` ([cli.md](cli.md)).

### Mirror tokens and the chief

`POST /mirror/mcp` also offers `chief_ask` ([attention.md](attention.md#asking-the-chief)).
Through it a mirror token **puts a question — fenced as the session's words —
into a turn of its owner's chief** and **reads the chief's answer**; with
`route_id` (any of the owner's routes) the chief is also shown that routed
message's sender, subject and body. Every question and answer writes a
`chief_question.*` audit row; the question's row names the host and
session it came from.

🔴 **Accepted risk: a mirror token can start chief turns and read the chief's
answers.** Any of the owner's mirrored sessions may ask, by operator decision —
so may a leaked mirror token, or a session fed outside text. The chief is told
the question is data, not instructions; that is a prompt, not a boundary. The
chief holds the fleet tools (`sessions_overview`, `focus_search`, `fleet_list`), so
a question can ask it to read another session, and the answer goes back to
whoever asked. The chief answers no ask but one put to it. Revoke a token you suspect.
