# Accounts, sign-in and tokens

## Two modes

`MUSTER_AUTH_MODE` selects one. Any other value refuses to boot.

### `password` (the default)

- One shared operator password, `MUSTER_UI_PASSWORD`, acting as **user 1**, the
  admin.
- If it is unset or shorter than 12 characters, every browser page answers 503
  and `/login` says which variable is missing. It fails closed.
- `POST /login` compares in constant time. Wrong passwords are counted per
  client address (10 a minute) and for the whole server (100 a minute). The
  step-up confirm shares the counts.
- **Known devices.** A browser that has signed in correctly gets a
  long-lived **device cookie** (`muster_device`: HttpOnly, `SameSite=Lax`,
  `Secure` as the session cookie is, 400 days). It is signed with a key
  derived from the operator password, so changing the password invalidates
  every device cookie.
  - At **sign-in**, a browser that is still signed in (a valid session
    cookie) counts as a known device too. At the **step-up confirm** it does
    not: step-up exists for the case where the session itself is in the wrong
    hands, so there only the device cookie counts, and a session without one
    is refused past the counts like any unknown device.
  - Once its address's count, or the whole server's, is past its limit, a
    request from an **unknown** device is refused with 429 before the password
    is compared, correct or not. So a guesser is held to 10 guesses a minute
    from one address, and 100 across all of them. Each refusal is logged (at
    most one line a minute) so a flood is visible.
  - A request from a **known** device is never refused for the counts when it
    carries the correct password. So a flood of junk sign-ins cannot lock the
    operator out of a browser they have used before.
  - A wrong password from a known device still counts.
  - The device cookie is not a credential: it admits nothing on its own.
  - A browser that has never signed in (or whose cookies were cleared, or that
    signed in before the password was changed) waits for the window to pass
    while a flood from its own address is running.
  - ⚠ Behind a reverse proxy that is not declared trusted
    (`MUSTER_TRUSTED_PROXIES`), every client has the proxy's address, so one
    address's flood refuses every unknown device. Declare the proxy to keep
    the counts per client.
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
- **The client topology must be declared.** muster refuses to boot in this
  mode unless `MUSTER_TRUSTED_PROXIES` (with `MUSTER_CLIENT_IP_HEADER`) names
  the proxy in front of it, or `MUSTER_DIRECT_CLIENTS=1` says there is none.
  GoTrue keys its own per-address limits on the client address muster sends it
  (`X-Muster-Client-IP`), and behind an undeclared proxy every user would share
  one ([configuration.md](configuration.md#client-addresses-behind-a-proxy)).
- **Throttling** is per account: 3 free failures, then a backoff that doubles up
  to 5 minutes. Failures are also counted per client address (10 in 10
  minutes). That count never refuses a correct password or passkey: past it,
  a wrong attempt is answered 429, and an attempt at an email that is not a
  muster account is refused without asking GoTrue. The admin claim's failures
  count the same way (429 past it). Passkey sign-in challenges are never
  refused per address (the sign-in page fetches one on every load, and a
  shared address must not lose passkey sign-in). GoTrue's own per-address
  limit bounds how many one client can mint. muster remembers the challenges
  it issued in one table of at most 8192, oldest dropped, and a passkey
  verify naming a challenge it does not hold is refused without asking
  GoTrue.

### What happens when you sign in (both modes)

- **The sign-in page shows progress.** After you tap Sign in, the button is
  disabled and shows a spinner and "Signing in…", and a screen reader hears
  "Signing in…". It stays that way until the app's page replaces it. A wrong
  password draws the page again, with the form enabled. The passkey buttons
  say where a passkey sign-in has got to: "Waiting for your passkey…" while
  the browser's passkey sheet is open, "Checking your passkey…" while muster
  checks it, and "Signed in. Opening muster…" once it has. Without JavaScript
  the form still works, without the progress.
- **The browser's caches are cleared on the sign-in page, not on the sign-in.**
  Every sign-in page (sign-in, the admin claim, an invite, a password reset)
  asks muster for `Clear-Site-Data: "cache"` in the background
  (`POST /auth/clear-caches`, same-origin only) while you type. That drops
  any page the previous session left in this browser's caches, so Back cannot
  show it to the next person. The case it covers is a session that ended
  without a sign-out: it expired, or was ended on another device. The sign-in
  itself sends no `Clear-Site-Data`. A browser does not follow a redirect
  that carries one until the clear has finished, and Chromium's clear reads
  through the browser's whole disk cache (every site's), so a sign-in that
  carried it took longer the bigger your cache was. Measured in Chromium:
  350–500 ms with 20,000 cached entries, and 10.7 s for the first clear after
  the browser started.
- **Signing out still sends `Clear-Site-Data: "cache"` on its own response**,
  so the sign-in page only appears once the caches are gone.
- ⚠ Browsers honour `Clear-Site-Data` only on a secure origin (HTTPS, or
  localhost). On plain HTTP, a page restored with Back checks with the server
  and reloads if a different person is now signed in
  ([sending-and-step-up.md](sending-and-step-up.md)).

### Cookie names

muster's browser cookies are `muster_session` (password mode), `muster_at` and
`muster_rt` (gotrue mode), `muster_device` (the known-device cookie),
`muster_push_device` (which Web Push subscription is this browser's) and
`muster_signin_app` (a [sign-in with the muster app](#sign-in-with-the-muster-app)
in progress). Every
one is `HttpOnly`, `Path=/` and has no `Domain`, so it goes back only to the
host that set it.

- **On an https deployment they carry the `__Host-` prefix**
  (`__Host-muster_session`, and so on). The rule is per deployment, never per
  request: prefixed when `MUSTER_PUBLIC_URL` is `https` **and** the cookies
  are `Secure`. In gotrue mode they always are, so every https gotrue
  deployment is prefixed, `MUSTER_SECURE_COOKIES=0` included. In password mode
  that takes the resolved secure-cookie setting (see `MUSTER_SECURE_COOKIES` in
  [configuration.md](configuration.md)). Everything else is unprefixed: the
  Compose stack on `http://localhost`, any plain-http install, and a password
  install with `MUSTER_SECURE_COOKIES=0`. The boot banner's `cookie names:`
  line says which.
- **What the prefix buys.** A browser keeps a `__Host-` cookie only if it is
  `Secure`, has `Path=/` and has no `Domain`, so a page on another host, a
  sibling subdomain included, can never set one that muster reads. A prefixed
  deployment's cookies are therefore always `Secure`.
- **Upgrading to a prefixed deployment signs everyone out once.** The old
  names are not read any more, and muster expires each old-named cookie on the
  first response that carries it. The known-device cookie is reset too: until
  a browser signs in once more, it is an unknown device, so while the
  wrong-password limit is tripped it is refused like any other unknown device.
  Paired phones are unaffected (their credential is not a cookie); a browser
  that had Web Push on is asked to turn it on again.
- **A prefixed deployment opened over `http://localhost`.** WebKit drops a
  `Secure` cookie there, so sign-in would work in one engine and not another.
  Instead a sign-in or step-up from a plain-http loopback page is refused with
  a page saying "This muster is set up for `<MUSTER_PUBLIC_URL>`. Sign in
  there.", with a link to that address's sign-in page, and no cookie is set.
  This is what the Compose stack does once it is given an https address (the
  Tailscale step in [quickstart-local.md](quickstart-local.md#add-your-phone)).
- **An unprefixed deployment beside a same-site plugin refuses to boot.** With
  unprefixed names, nothing stops a page on the same registrable domain
  (`meet.example.com` beside `app.example.com`, as well as any subdomain of
  muster's host) setting cookies muster would read. So muster refuses to start
  when any `MUSTER_PLUGIN_ORIGINS` entry shares `MUSTER_PUBLIC_URL`'s
  registrable domain (by the public suffix list) and the names are unprefixed.
  Give muster its `https` public URL, or put the plugin on another
  registrable domain.

### Setting up the first admin

With the GoTrue variables set, `muster-server invite --claim-admin` prints a
single-use link, valid for 24 hours. Opening it binds a new GoTrue login to
user 1.

The GoTrue pair also works in password mode, where it only makes that claim link
usable. That lets you bind the operator's login before you switch modes.

The link is written on `MUSTER_PUBLIC_URL`. In gotrue mode, when that is this
machine's own `http://localhost` address on the main port (a hub without
Tailscale), the command writes the link on the browser listener's port instead
and says to set `MUSTER_PUBLIC_URL` there: the main port refuses a browser.
It assumes each port is published on the same number on the host as in the
container (the Compose stack's mapping, and the override below); with another
mapping, set `MUSTER_PUBLIC_URL` to the address the browser opens.

### A GoTrue server beside the Compose stack

The Compose stack ships no GoTrue. This `docker-compose.override.yml`, beside
`~/muster/docker-compose.yml`, adds one with its own Postgres and publishes the
browser listener on this machine. It runs the GoTrue image the end-to-end
tests run (`e2e/gotrue/compose.yml`), and `tests/compose-check.sh` checks it
parses with the release's compose file and issues the tokens muster accepts.

```yaml
services:
  muster:
    # The browser listener (MUSTER_BROWSER_PORT), on this machine only.
    ports:
      - "127.0.0.1:8107:8107"
    depends_on:
      gotrue:
        condition: service_healthy
  # GoTrue's own Postgres. GoTrue keeps its tables in the `auth` schema,
  # which must exist before its first start.
  gotrue-db:
    image: postgres:16-alpine
    restart: unless-stopped
    entrypoint: ["sh", "-c", "echo 'CREATE SCHEMA IF NOT EXISTS auth;' > /docker-entrypoint-initdb.d/auth.sql && exec docker-entrypoint.sh postgres"]
    environment:
      POSTGRES_PASSWORD: ${GOTRUE_DB_PASSWORD:?set GOTRUE_DB_PASSWORD in .env}
    volumes:
      - gotrue-pgdata:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -h 127.0.0.1 -U postgres"]
      interval: 5s
      timeout: 5s
      retries: 20
  gotrue:
    image: supabase/gotrue:v2.197.0
    restart: unless-stopped
    depends_on:
      gotrue-db:
        condition: service_healthy
    environment:
      GOTRUE_API_HOST: 0.0.0.0
      GOTRUE_API_PORT: "9999"
      API_EXTERNAL_URL: ${MUSTER_PUBLIC_URL:?set MUSTER_PUBLIC_URL in .env}
      GOTRUE_SITE_URL: ${MUSTER_PUBLIC_URL}
      GOTRUE_URI_ALLOW_LIST: ${MUSTER_PUBLIC_URL}/**
      GOTRUE_DB_DRIVER: postgres
      GOTRUE_DB_DATABASE_URL: postgres://postgres:${GOTRUE_DB_PASSWORD}@gotrue-db:5432/postgres?sslmode=disable&search_path=auth
      GOTRUE_DB_NAMESPACE: auth
      # muster checks these three on every token.
      GOTRUE_JWT_SECRET: ${MUSTER_GOTRUE_JWT_SECRET:?set MUSTER_GOTRUE_JWT_SECRET in .env}
      GOTRUE_JWT_ISSUER: muster-auth
      GOTRUE_JWT_AUD: authenticated
      GOTRUE_JWT_DEFAULT_GROUP_NAME: authenticated
      GOTRUE_JWT_ADMIN_ROLES: service_role
      GOTRUE_JWT_EXP: "900"
      # Logins are made by muster (the admin claim, invites), never by
      # sign-up; no mail server, so addresses are confirmed as created.
      GOTRUE_DISABLE_SIGNUP: "true"
      GOTRUE_EXTERNAL_EMAIL_ENABLED: "true"
      GOTRUE_MAILER_AUTOCONFIRM: "true"
      GOTRUE_EXTERNAL_PHONE_ENABLED: "false"
      GOTRUE_EXTERNAL_ANONYMOUS_USERS_ENABLED: "false"
      GOTRUE_PASSWORD_MIN_LENGTH: "12"
      GOTRUE_SECURITY_REFRESH_TOKEN_ROTATION_ENABLED: "true"
      GOTRUE_RATE_LIMIT_HEADER: X-Muster-Client-IP
      # Passkeys, for the address the browser opens (its host is the RP id).
      GOTRUE_PASSKEY_ENABLED: "true"
      GOTRUE_WEBAUTHN_RP_ID: ${GOTRUE_WEBAUTHN_RP_ID:-localhost}
      GOTRUE_WEBAUTHN_RP_DISPLAY_NAME: muster
      GOTRUE_WEBAUTHN_RP_ORIGINS: ${MUSTER_PUBLIC_URL}
    healthcheck:
      test: ["CMD-SHELL", "wget -qO- http://127.0.0.1:9999/health >/dev/null"]
      interval: 5s
      timeout: 5s
      retries: 30
volumes:
  gotrue-pgdata:
```

And in `~/muster/.env` (with INSTALL.md's helpers: `env_set`, and
`env_secret` for the two secrets):

```sh
MUSTER_AUTH_MODE=gotrue
MUSTER_GOTRUE_URL=http://gotrue:9999
MUSTER_GOTRUE_JWT_SECRET=<48 random hex characters>   # env_secret MUSTER_GOTRUE_JWT_SECRET 24
GOTRUE_DB_PASSWORD=<random>                           # env_secret GOTRUE_DB_PASSWORD 24
MUSTER_BROWSER_PORT=8107
MUSTER_PUBLIC_URL=http://localhost:8107
MUSTER_DIRECT_CLIENTS=1   # browsers reach 8107 directly; see below for a proxy
```

The last line declares that nothing sits between the browsers and muster. This
mode refuses to start without a declaration. If you put `tailscale serve` or
another reverse proxy in front instead, remove it and set
`MUSTER_TRUSTED_PROXIES` to the address muster sees the proxy connect from,
and `MUSTER_CLIENT_IP_HEADER=X-Forwarded-For`
([Client addresses behind a proxy](configuration.md#client-addresses-behind-a-proxy)).

`MUSTER_PUBLIC_URL` is the address a browser opens: the browser listener. With
Tailscale, point `tailscale serve` at 8107, set `MUSTER_PUBLIC_URL` to the
`https://…ts.net` address and `GOTRUE_WEBAUTHN_RP_ID` to its host name. Then
`mi_dc up -d --wait --quiet-pull` (which reads the override; a bare
`docker compose -f docker-compose.yml` would not), and
`mi_dc exec -T muster /muster-server invite --claim-admin`.

**Turn it off:** delete `docker-compose.override.yml` **and** the `.env` lines
above (`MUSTER_AUTH_MODE`, `MUSTER_GOTRUE_*`, `GOTRUE_*`,
`MUSTER_BROWSER_PORT`, `MUSTER_DIRECT_CLIENTS`), set `MUSTER_PUBLIC_URL` back to the main port's
address (`http://localhost:8105`, or the `https://` one with `tailscale serve`
pointed at 8105 again), then `mi_dc up -d --wait --remove-orphans` (which
stops the GoTrue containers). Both halves: an override left behind without its
`.env` lines does not parse, and `install.sh` refuses to update until it is
gone.

## Users, roles and invites

- **Roles:** there are two, `admin` and `member`.
- **Invites:** an invite always creates a member. Links last 24, 72 (the
  default) or 168 hours, and carry their token in the URL fragment.
  Invites are managed on `/admin/users`.
- **Display names:** the name a person gives when they redeem an invite, and
  a team's name, follow the rule the instance name uses
  ([branding.md](branding.md)). A name is refused if it holds a control
  character (C0, DEL, C1) or a text-direction or invisible formatting
  character (U+061C, U+200B, U+200E, U+200F, U+202A–U+202E, U+2066–U+2069,
  U+FEFF), or bytes that are not UTF-8, and the form names the character it
  refused. The zero-width
  joiners (U+200C, U+200D) are allowed, because emoji sequences and Persian
  and Indic spellings use them. Names saved before this rule are not changed.
- **Admin pages:** promote a member, disable or enable a user, send a reset
  link, delete a user's passkeys. The last admin cannot be demoted.
- **From the command line:** `muster-server user disable|enable <id>` does the
  same as the admin page. A running server notices within 10 seconds for open
  streams and within 30 seconds for new requests. `muster-server app
  install|rotate|uninstall|list` is the counterpart of `/admin/apps` for the
  server-installed apps ([plugins.md](plugins.md#server-apps)), and `app install
  --link-to` and `app unlink` [link two of them](plugins.md#linked-server-apps),
  which only the command line can do.
- **Data is per user.** Every user-data row has an `owner_id`, enforced by
  Postgres row-level security (see [schema.md](schema.md)). Each user has their
  own tasks, agents and tokens, and on Kubernetes their own chief (the Compose
  stack runs only the admin's chief: [features.md](features.md#chief)). An admin's ordinary pages show only the
  admin's own rows; the cross-user views are on `/admin/*`.
- **Credentials:** members bring their own Claude and OpenRouter credentials
  (`/account/credentials`, which needs the connector key). The pool configured in
  the server's environment belongs to the admin.

## Teams

A team is a group of people on one muster server who share sessions,
officers, tasks and a chief ([design/team-sharing.md](design/team-sharing.md)).
**A team has its own tasks (filed in it or moved into it), a team chief that a
manager provisions and its operators chat with in one shared conversation,
the agents that chief starts, its officers, and its hosts' sessions, which its
members read and whose questions reach their Attention.** A team's agents run
on the server's Claude pool only when an admin lists the team in
`MUSTER_TEAMS_ON_SERVER_POOL`, and work in repositories only when it is listed
in `MUSTER_TEAMS_ON_GITHUB_APP` ([chief.md](chief.md)). A team's operators
and managers type into its hosts' sessions, each with their own fresh sign-in
(team sharing 2b.4).

- **What a team is.** A `users` row of kind `team`: something that can own
  things. **No one signs in as a team.** It has no login, no email and no
  password, and it cannot be an admin, be invited, hold a passkey, mint a token
  or pair a phone. A team-owned agent's own token (the agent, connector and
  approval-relay doors) is admitted as the **team acting principal**:
  `muster.uid` is the team, with no team memberships and never every user's
  rows. The stopped-agent reaper, the attention queue, route timeouts, the
  host-dispatch reconciler and the job queue act as the team the same way
  when they meet a team-owned row, and so does a **team officer**'s runtime
  (the officer door, `authz.ViaOfficer`): an officer whose `owner` in
  `MUSTER_OFFICERS` is a team runs its instances, its store, its asks and its
  outbound drafts as the team. A **team host**'s mirror token (below) is the
  other credential admitted as the team. Every other door refuses a
  credential that resolves to a team ("a team cannot sign in; sign in as a
  person"); any other code that builds a team principal at a door outside
  that set fails. A plugin pairing
  request that names a team as its owner is answered exactly as one that
  names no account.
- **Members and roles.** Each member has one role: `viewer`, `operator` or
  `manager`. A team is never a member of a team. Each request reads the
  person's memberships together with their role, so a change applies from the
  next request.
- **What a role may do with a team's rows.** Postgres decides it, table by
  table ([schema.md](schema.md#team-access)):
  - A viewer reads.
  - An operator writes the shared tables (tasks, asks, queued messages,
    focus, officer items).
  - A manager writes the managed ones (host tokens, officer types and
    charters, plugins and their drafts).
  - Credentials, connectors, devices and push are never shared. A team's
    agents are (0131), without their secrets, which live in `agent_secrets`.
  - A team's [pages](pages.md#a-teams-pages) (0165): every member reads them,
    answers them and shares them; only the person who started the agent that
    made one deletes it or changes its version (a manager when that person
    has left the team), and a manager can always revoke its links.
  - A Claude account's rate-limit and auth-failure marks are the ACCOUNT's:
    the admin and every team on the server pool see the same marks on the
    pool's accounts. A member's own accounts, and their marks, are theirs.
  - A person reaches a team's rows only from the browser or a paired phone.
    A personal API token, their agents (their own chief included), their
    mirror hosts and every background job act on their personal rows only. A
    team-scoped token may come later.
  - A person moves one of their own tasks into a team they operate in, with
    its children; nothing moves out of a team.
- **Who manages teams.** An admin, on **Admin → Teams** (`/admin/teams`),
  creates a team, naming its first manager, and adds, re-roles and removes
  members. A team's **manager** does the same for their own team on
  `/teams/<id>`: add a person by the email they sign in with, change an
  existing member's role, remove a member, and read the team's recent
  activity. Adding by email:
  - The form's response does not reveal whether the email is registered, and
    a disabled account is answered like an unregistered one.
  - A successful add does show the person in the member list on the next
    page view.
  - The person is added **without their consent**. Consent-based team
    invites are an open follow-up. A team always keeps
  at least one manager there. Anyone else gets the same "no such team" a team
  that does not exist gets. Being an admin gives no
  membership: an admin who is not a member sees nothing of a team either. Every
  change writes an `audit_log` row naming the acting person and the team
  (`team.create`, `team.member.set`, `team.member.remove`), in the same
  transaction as the change. The rows are listed under **Recent account
  activity** on `/admin/users`, and a team's under its managers' page.
- **Not a user.** A team is not listed on `/admin/users`, and every user action
  there answers 404 for a team's id.
- **Team hosts** (team sharing 2b.2). A team host is a `muster-mirror`
  connected with a **team host token**: a mirror token owned by the team, on a
  machine (or a Unix user on one) the team treats as shared.
  - **Minting.** A team's **manager** mints, revokes and confirms its host
    tokens on `/teams/<id>/hosts`, signed in within the step-up window; each
    is audited (`team.host.token.create`, `team.host.token.revoke`,
    `team.host.replace`, and `team.host.replace.refused` for a wrong code) as
    the manager. The page shows each token's last
    use, whether its host is connected, and its rotate mark. `muster-server
    mirror token create <host> --team <id>` mints the same token from the
    server's shell (audited `via = cli`), for a headless bootstrap.
  - **What the token does.** `/mirror/connect` registers the host under the
    team, and `/mirror/ask` opens its sessions' questions as the team's. Its
    `POST /mirror/mcp` acts as the **team acting principal**, and Postgres
    then shows it every team row, so the door serves it a **reduced tool
    set** (`teamHostTools`): the session's own asks (`attention_ask`,
    `attention_wait`, `attention_cancel`), the tasks that session created, its
    own host dispatch's `agent_report_done`, and a team host officer
    instance's own tools. Everything else is left off its tool list, which is
    the one guard: muster runs a listed tool only, and answers any other name
    with a refusal that names it. That covers:
    every owner-authority tool (`attention_answer`, the decide tools,
    `agent_destroy`, `chief_restart`, `dispatch_approve`, `route_reply`,
    `plugin_message_route`, and the rest), every read of the team's whole
    estate (`agents_list`, `plugin_drafts_list`, `plugin_outbound_batch_get`,
    …), `dispatch_request`, `chief_ask`, the page tools and a host
    instance's connector tools. So a team host never answers or decides
    anything — no team ask, no dispatch card — and files no step-up card. A
    team token's **remote** call (`X-Muster-Client: remote`) is refused 401:
    it would be served unbound to any listed session. Its waiting and done
    windows are the team's episodes (2b.3): pushed to its operators and
    managers, with no stall classifier.
  - **Its asks.** A team host session's `attention_ask` is the team's, and
    shows on every member's Attention (viewer and up). An operator or a
    manager answers it there; a viewer's copy is drawn with no answer.
  - **Who watches it (2b.3).** The team host's sessions are listed to the
    team's members (viewer and up), on a browser session or a paired phone
    only — never through a personal API token, an agent or a background
    loop — and a member reads them read-only. One table decides what each
    mirror route does with a team session (`mirrorTeamRoutes`): watch it,
    refuse it (the chief panel, proposals, succession and suggested replies:
    403 at every role), or let an operator type into it (below). Anyone not
    a member is answered 404, as for a host that does not exist. A member's
    open views and event stream end within 10 seconds of leaving the team.
  - **Who types into it (2b.4).** An operator or a manager: the composer, the
    terminal's live keys, the send queue and an answer typed into the pane.
    Each needs **that person's own** fresh sign-in, in the browser they type
    from (a viewer is answered 403). Their live terminal is registered at
    operator, so a removal or a drop to viewer ends it within 10 seconds. A
    queued send is the team's (`owner_id`) and the queuer's (`queued_by`):
    at delivery muster re-reads **the queuer** — enabled, still an operator
    or a manager of the team, no sign-out since — and a row that fails is not
    sent. Removing a member or dropping them to viewer cancels what they
    queued into the team's windows, and a disable or a sign-out everywhere
    cancels it on every team. Only the queuer edits, retries or sends now
    their row; a manager may also cancel or delete anyone's. A paired
    phone's answer to a team's question needs an operator or a manager, on a
    fresh read; a viewer's is refused by name (`not_operator`). A team
    chief's card to END one of the team's sessions is ended by an operator
    or a manager from a browser with a fresh sign-in, never from a phone, and
    the session ended is the team's.
  - **The rotate mark.** When an operator or a manager is removed, drops to
    viewer or is **disabled**, the team's unrevoked host tokens are marked
    rotate (`mirror_tokens.rotate_at`), in the same transaction. Nothing is
    revoked (a revoke would cut every team session at once): the page says
    "rotate: ben left on <date>", read from the team's audit row, until a
    manager revokes the token and mints a new one. 🔴 **A token someone kept
    after leaving connects from any machine.** That is why the mark matters.
  - **A second connection waits for a manager.** While a team host is
    connected, a second connection presenting its token is **held**: refused
    as pending a manager's confirmation, sent nothing and shown to no one,
    while the connected host keeps serving. The manager sees "Replace the
    connected host?" on `/teams/<id>/hosts`, listing each held machine
    (first and last seen, the address it came from).
  - **The manager confirms by the machine's code.** Each `muster-mirror`
    process draws a random **attempt code** when it starts (60 bits, shown as
    `XXXX-XXXX-XXXX`) and sends it on every connect. A held one prints
    "give this code to a team manager: XXXX-XXXX-XXXX" in its log, and
    `muster doctor` on that machine shows it. The manager types the code of
    the machine they moved on the hosts page; there is no per-row button
    for such a machine. The confirm needs the step-up sign-in and is audited
    (`team.host.replace`, naming the code's fingerprint). It admits ONLY that
    code's next attempt, within 3 minutes (muster-mirror retries at most a
    minute apart), and is spent once used. Every other connection stays held
    **whatever its address**, so this holds behind a proxy or a Kubernetes
    NodePort, where every machine shares one address. A kept copy of the
    token, on another machine, draws its own code, which the manager never
    types. The server keeps only a hash of the code; its logs and audit rows
    carry a 16-hex fingerprint, an HMAC under a per-process key, so they
    cannot be tested against guesses. A wrong code arms nothing and is
    audited as `team.host.replace.refused`. 🔴 **The moved machine's own
    muster-mirror log shows the code**, once per process: anyone who can
    read that log (its user's journal, an admin of that machine, wherever
    the log is shipped) can learn it, and a kept copy of the token
    presenting it inside the confirm window would be taken.
  - **A `muster-mirror` that sends no code** (older than the code) cannot be
    confirmed at all: an address tells no machines apart behind a proxy or a
    Kubernetes NodePort (measured on a NodePort deployment: every host showed
    as the same pod-network address), so there is nothing to name it by. It
    is held with "upgrade muster-mirror on this machine, or stop the live
    host", and the card says the same. Stopping the live host lets the next
    connection in at once, code or no code.
  - `muster doctor` on a held machine says it is held and shows its code.
    Where machines are held and doctor cannot read this machine's code, or
    the code is not the live connection's, it FAILs ("this may be a held
    machine") rather than report it connected.
  - 🔴 **The hold guards the tunnel only.** A held machine, or anyone keeping
    a copy of the token, can still call `/mirror/mcp` and `/mirror/ask` as
    any session the connected host lists (those doors believe the session a
    call names when the connected host lists it), within the reduced tool
    set. The rotate mark is the answer: revoke the token and mint a new
    one. A connection arriving while the host is
    offline is taken at once, as for a personal host, so a restart of the
    real host is unaffected. This does not close the offline window: only
    rotation does.
  - A viewer reads everything on a team host's screens and transcripts; an
    operator who types runs commands as the host's Unix user. Team members
    have to trust each other.
- **Team officers.**
  - `MUSTER_OFFICERS` accepts a team's id as an officer's `owner`. Boot
    refuses an owner that names nobody.
  - **Team host officers (2b.5).** A team may own a `"runtime": "host"`
    officer. It runs on the team's own host of that name (a host connected
    with a team host token, above), never on a member's personal host of the
    same name. Its spawn rows are the team's, and every member reads them on
    `/officers/spawns`, grouped under the team. Nobody lends or changes them.
    A team host officer that declares `connectors` is refused, at boot and
    when the database roster is loaded, because nobody could decide a team's
    connector request (G3). See
    [officers-builtin.md](officers-builtin.md#a-teams-host-officer-team-hosts).
  - Every member reads the team's officer types and charters on
    `/teams/<id>/officers`. Only a **manager** changes them, there, signed
    in. Postgres refuses an operator's write too.
  - The JSON admin API (`/api/officers/*`, `muster officer`) takes a
    personal API token or the hook token, so it acts on the caller's own
    officers only and never names a team.
  - A team officer's question to its owner, and an escalation to a team, is
    **one** ask the team owns. Every member sees it, an operator answers it,
    and the answer records who did.
  - Its asks, escalations and outbound drafts push to the team's current
    operators and managers (not viewers), as every team-owned notification
    does: see [push.md](push.md#team-notifications) for who, the
    per-member decisions, the withdrawals and each member's switch on
    **Account → Teams**.
  - Sharing a spawn, taking it back, and editing a team officer's type or
    definition each write an audit row naming the person and the team, which
    the team's managers read on `/teams/<id>`.
  - A team is named as an escalation target only on that officers page, and
    only the page's own team or a team the manager operates. The JSON admin
    API's `--escalate-to` still refuses a team's id.
- **Who approves a team's draft.** A manager, on the Asks page, with a fresh
  sign-in. The team itself never decides its own draft: a team officer's
  instances and runtime are refused, and so is Postgres's write. A manager
  never approves a draft they caused. A personal API token, an agent and a
  mirror host carry no team, so none of them decides a team's draft.
- **Lending a host spawn.** A host officer's spawned session row
  (`officer_host_spawns`) is personal. Its owner may share it with one team
  they are an operator or manager of, and unshare it, on
  **/officers/spawns**, or through their chief's `officer_spawn_share` tool.
  The team's members then see it there, read-only.
  - A personal API token never shares one directly.
  - **Operator-approved exception** to "a personal API token carries no team
    access": a token that messages the owner's chief can have it share or
    unshare the owner's own spawns with a team the owner operates. Nothing
    else: team drafts and charters stay session-only. A person's shares to a team end when they leave it or drop to
  viewer.

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
  one needs a sign-in from the last five minutes, and more: see
  [Adding a passkey](#adding-a-passkey).
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
    nothing. Autofill and the buttons share the page's one prefetched
    challenge: a challenge is spent only by a verify, and only one request
    ever verifies it. A new one is fetched only after a verify has spent it,
    or when it is four minutes old. A closed passkey sheet keeps it, so a
    sign-in page view makes one options request, however many sheets are
    closed. This matters at the public edge, which rate-limits `/login` and
    the passkey routes together with the password sign-in.
  - **Use a passkey from my phone.** The same request with the WebAuthn
    Level 3 hint `hybrid` (Chrome 128 and later), which goes straight to the
    phone's QR code. A password-manager extension that intercepts the request
    may ignore the hint and show its own window. The page then tells the
    person to pick that window's option for the browser, a phone or a
    security key. Password managers label that option differently, so the
    page names no label.
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

⚠ **An account with two or more passkeys added before muster recorded ids
never gets the passkey-list signals.** A sign-in cannot tell which of two
id-less passkeys was used, so it records nothing. Such an account then gets
neither `signalAllAcceptedCredentials` nor `signalUnknownCredential`;
`signalCurrentUserDetails` is still sent. To fix it, the person opens
**Account → Passkeys**, adds a new passkey on each device (a new one records
its id), then removes the old ones, leaving at most one without an id. An
admin can also remove all of a user's passkeys (**Admin → Users → Remove
passkeys**) and have the person add them again.

### Adding a passkey

A passkey can approve a [step-up card](mirroring.md#confirming-a-card-in-the-browser-with-a-passkey)
in the browser, which otherwise only the paired phone can do. So adding one,
pairing a phone that confirms requests, and removing either need more than
the password: 🔴 **once an account has a passkey or a paired phone with a
step-up key, a password alone never reaches card approval.**

The one place the password is enough is an account's **first factor**, on an
account that has neither:

- its first passkey is added with the password, and it **cannot** approve a
  card (below);
- its first phone with a step-up key is paired with the password, and it
  **can** answer cards: a phone's step-up key is the hardware-bound proof
  cards were built on, and without it the account could answer no card at
  all.

From then on, every change below takes a proof.

- **The account already has a passkey, or a paired phone with a step-up
  key:** adding another needs, besides a sign-in from the last five minutes,
  one of these proofs for that browser session:
  - **a passkey you already have.** The passkeys page asks for it
    (`POST /account/passkeys/proof/options`, then `…/proof/verify`), with user
    verification, as the step-up confirm does;
  - **your paired phone.** The page files a step-up card, *Add a passkey to
    your account?* (`POST /account/passkeys/proof/phone`). Only the phone's
    Confirm answers it; a passkey in the browser cannot. The page waits for
    the Confirm, then you tap **Add a passkey on this device** again.

  The proof allows one passkey, added from that browser session, within ten
  minutes. Without it, both the options and the add answer
  `403 proof_required`, naming the proofs the account can give.
- **The account has neither:** there is nothing to prove with, so the password
  adds the first passkey, as before. That passkey signs in and confirms
  "Confirm it's you" like any other, but **it cannot approve a step-up card**.
  A card shows the phone line and says why.
- **Which passkeys may approve a card.** Every passkey added records it, with
  the credential id read from the authenticator's **signed** data:
  - one added with the phone's Confirm may;
  - one added with the password alone may not;
  - one added with a passkey proof takes the standing of the passkey that
    proved it.

  An add whose claimed id (`rawId`) differs from the signed one is refused
  before GoTrue registers anything. 🔴 A passkey **with no record** may
  approve a card only when GoTrue created it before muster started recording
  (`passkey_rules.vouching_since`, set once by migration 0187). A passkey whose
  creation time GoTrue does not give is not trusted. That keeps
  older passkeys working, card approval included. A newer passkey with no
  record (one added through GoTrue's own endpoints, or one whose record was
  lost) approves no card and proves no enrolment. muster writes the record
  before it answers the add. If the write fails it removes the passkey; if the
  removal fails too, that passkey stays and approves nothing. The record
  writes are serialised per account, so two at once lose neither.
- **Pairing a phone that confirms requests** (one with a step-up key) takes
  the same proof once the account has a passkey or such a phone. Confirm on
  Account → Passkeys first; the pairing code shown afterwards may pair a
  step-up key, and the proof is spent. A code minted without the proof says so
  (`stepup_allowed: false`). A phone redeeming that code with a step-up key is
  refused (`403 stepup_proof_required`) and the code is not used up. A phone
  without a key still pairs read-only. Every pairing pushes every device
  already registered "A phone was paired to your account", whatever your
  notification settings say.
- **Removing a passkey** takes the same proof once the account has a passkey
  or a step-up phone. Proving with the passkey you are removing is enough.
  The removal pushes "A passkey was removed from your account".
- **The admin pages do not reset or strip the admin's own account.** An admin
  cannot mint a password-reset link for their own account, or remove their
  own passkeys from Admin → Users. Those are done on Account, with its proof.
- **A proof allows one change, used once.** The ticket is taken when it is
  checked, so two adds racing on one ticket add one passkey.
- **Removing a phone or a notification target** (on Account) takes the same
  proof on an account that has a passkey or a paired phone with a step-up
  key. Without the proof the removal is refused and nothing is removed:
  confirm on Account → Passkeys (`?confirm=1` opens the proofs), then remove
  within ten minutes. So a person holding only the password cannot first
  remove your devices to silence the notice below. On an account with neither,
  the removal goes through as before, after every device registered for push
  is told ("A phone was removed from your account"). A removal with the proof
  is announced the same way. Removing a phone or target that does not exist
  answers 404, spends no proof and announces nothing. These stay open:
  - a phone removing itself from the app;
  - a browser turning off its own notifications;
  - a password reset, which revokes every paired phone. That is the recovery
    for a lost phone on an account without a passkey. With a passkey, prove
    with it and remove the lost phone.
- **Every passkey added pushes you "A passkey was added to your account"** on
  every device registered for push, whatever your notification settings say:
  a person holding only your password could turn them off first. The
  `user.passkey_added` audit row records the proof (`passkey`, `phone` or
  `password`) and whether the passkey may approve cards.

### Passkeys in the Android app

The [Android app](design/android.md)'s WebView runs WebAuthn as the app
(`WEB_AUTHENTICATION_SUPPORT_FOR_APP`). Two things differ from a browser:

- Android runs the ceremony only if the site lists the app.
  `MUSTER_ANDROID_PACKAGE` and `MUSTER_ANDROID_CERT_SHA256` make muster serve
  `/.well-known/assetlinks.json` with the `get_login_creds` relation only
  ([configuration.md](configuration.md)): not `handle_all_urls`, since the app
  verifies no links. The one link it takes is the QR sign-in's hand-off, an
  `intent:` link addressed to its package, which needs no verification
  ([design/qr-sign-in.md](design/qr-sign-in.md#the-android-app)). It is
  open, `application/json`, and your edge must serve it with no redirect.
- The assertion's `clientDataJSON.origin` is
  `android:apk-key-hash:<the signing certificate's SHA-256, unpadded base64url>`,
  not `https://…`. muster does not read that origin; GoTrue checks it against
  `GOTRUE_WEBAUTHN_RP_ORIGINS`. In GoTrue v2.197.0 (go-webauthn v0.16.5), an
  origin that does not start with `http://` or `https://` is compared as an
  exact string, so add that value to the list. The RP id stays the site's host.

Passkey autofill (conditional mediation) is not available in a WebView; the
buttons are.

## Sign in with the muster app

The sign-in page, in both modes, has a third way in: **Sign in with the muster
app**. It shows a QR code. Scan it with a phone that is already signed in to
muster (the installed app, or a browser), check what is asking, tap
**Approve**, and the browser shows whose account approved and signs in as it
when you tap **Continue as …**. The research and the threat
model are in [design/qr-sign-in.md](design/qr-sign-in.md).

- **It needs the database.** With no store the option is not shown, and its
  routes answer 404.
- **The code works once, for 2 minutes.** The page polls every 2 seconds and
  says where things are: waiting for the scan, scanned (approve on your phone),
  then approved, naming the account that approved ("Approved on a phone signed
  in as …") with **Continue as …** and **Cancel**. Nothing is signed in until
  you tap Continue: someone else who saw the code (on a screen share, over a
  shoulder) could approve it, and the browser would be signed in as them. It
  also says when the code is denied, cancelled or expired, with **Get a new
  code**. A link under the code opens the same page on a phone
  with no camera.
- **Only the browser that asked can use the approval.** Starting a sign-in sets
  `muster_signin_app` (HttpOnly, `SameSite=Strict`, 2 minutes) on that
  browser. The approval is spent only with the code **and** that cookie, so a
  code seen on a screen, in a photo or in a log signs nothing in. It is spent
  once, and the cookie is cleared.
- **The phone must be signed in, on a known device.** The QR opens
  `<public>/auth/qr#<code>`. The code is in the URL fragment, which a browser
  never puts in a request line, and the page sends it to muster in a POST
  body. So it is in no path or query string, no access or proxy log, and no
  history entry.
  - A phone that is not signed in is asked to sign in first and comes back to
    the request. The code crosses that sign-in in the tab's `sessionStorage`,
    not in the sign-in link.
  - **Approving needs a recent sign-in on the phone** (the step-up window,
    `MUSTER_STEPUP_WINDOW`). A stale phone is asked to confirm it's you and
    approves after that; denying needs nothing. Without it, an old or copied
    session could approve a new browser and so turn itself into a session
    that is fresh for the whole window.
  - **The browser that started a request cannot approve it.**
  - A signed-in browser that is not a known device is refused: one that never
    signed in with a credential there, such as a copied session or a browser
    this flow signed in. Sign out on it and sign in with your password or
    passkey once.
  - In password mode the known device is the `muster_device` cookie
    ([above](#password-the-default)). gotrue mode now sets `muster_device` too,
    on every sign-in with a password, passkey, claim, invite or reset link. It
    is signed with a key derived from `MUSTER_GOTRUE_JWT_SECRET`, your user id,
    your session cutoff and your latest Sign out everywhere, so it vouches only
    for you and stops vouching after a Sign out everywhere, a password reset or
    a disable. Signing out on that browser clears it. A phone that signed in
    before this release has none until it signs in again.
- **What the phone shows.** The requesting browser ("Chrome on Windows"), its
  address, the phone's own address, and how long the code has left. Approve only
  a sign-in you started yourself, on a screen in front of you. If someone sent
  you the code or asked you to scan it, tap **Deny**: that is how a QR sign-in
  is phished. muster has no geolocation, so it shows no place.
- **What you are signed in as.** In password mode, the operator. In gotrue mode,
  the person who approved. muster asks GoTrue for a single-use sign-in token
  for that person through its admin API and exchanges it at once, server-side.
  It checks that the session GoTrue returns is that person's (amr `otp`).
  ⚠ GoTrue keeps one such token per user, shared with the password-reset link,
  so signing in this way **ends a reset link** an admin handed you and you have
  not used yet. Ask for a new one.
- **An approval dies with its session** (gotrue mode). If the account is
  disabled, reset, or signed out everywhere after the approval, the redeem is
  refused, and that is checked again after GoTrue issues the session: one
  issued while such an event landed is ended at once. Password mode has no
  such revocation for its one stateless session, so none applies to an
  approval either. A session
  issued this way is an ordinary session: Sign out and Sign out everywhere end
  it.
- **Logged.** Each approval, denial and sign-in is in the audit log
  (`signin.app.approve`, `signin.app.deny`, `signin.app.redeem`, as the
  approver, with the requester's address and browser) and in the server log.
- **Throttles:**
  - 10 new codes a minute per client address, and 300 a minute across the
    server;
  - 10 failed look-ups or redeems a minute per client address (a live code is
    never refused for its address);
  - 90 status polls a minute per request (its cookie, so desktops behind one
    address do not slow each other), after which the page is told to slow
    down.
- **Refused from another origin.** Starting, approving and redeeming are POSTs
  checked like every sign-in. In gotrue mode they are served on the browser
  listener only.
- **On Android the phone's browser hands off to the muster app**, when the
  server names the app: `MUSTER_ANDROID_PACKAGE` (with
  `MUSTER_ANDROID_CERT_SHA256`, [configuration.md](configuration.md)). A
  phone camera opens the QR's link in the browser. On Android, and not
  already inside the app, the page shows **Open in the muster app** first,
  with **Continue in this browser** (the approval described above) beneath
  it. Nothing is opened on the server until you choose. Inside the app or
  another app's built-in browser, on any other phone or browser, or with no app
  configured, the page opens the request at once, as before.
  - **Open in the muster app** opens the app, which approves in its own
    screen. The code is still in no URL muster receives.
  - **Without the app installed**, the browser comes back to the page, with no
    code in its address, and the approval goes on in the browser.
  - Which button comes first is a guess from the browser, not a security
    check: a wrong guess changes only the order.
  The contract with the app (the link, its extra, the fallback, how the page
  tells the app from a browser) is in
  [design/qr-sign-in.md](design/qr-sign-in.md#the-android-app).

| route | what it does |
|---|---|
| `POST /login/app` | Starts a sign-in: sets the browser's binding cookie, answers `{code, url, svg, expires_in, interval}`. |
| `GET /login/app/status` | The request's state by the cookie: `pending`, `scanned`, `approved` (with `approver`: `{name, email}`), `denied`, `redeemed` or `expired`, with `interval`. Too fast: 429 `slow_down`. |
| `POST /login/app/redeem` | `code` and `next`, with the cookie: sets the session and answers `{redirect}`. |
| `GET /auth/qr` | The phone's page: moves `#<code>` into a form and posts it (on Android outside the app, after offering **Open in the muster app**). |
| `POST /auth/qr` | `step=open` (the details), `step=approve` or `step=deny`, with `token`. |

## Cross-origin writes

`SameSite=Lax` does not stop a page on a **sibling host** (any other
subdomain of muster's registrable domain): that page is the same site, so the
browser attaches muster's cookies to a write it makes to muster. So every
request that a **browser session** admits (in both modes) and whose method is
not `GET`, `HEAD` or `OPTIONS` must come from muster's own pages. Before any
handler runs, muster reads:

1. **`Sec-Fetch-Site`, first.** `same-origin` admits. `same-site`,
   `cross-site` and `none` are refused. One exception: with
   `MUSTER_PUBLIC_URL` set, an `Origin` exactly equal to its origin is
   admitted whatever `Sec-Fetch-Site` says. A browser sets `Origin` itself,
   so only a page on that origin can send it.
2. **Only if it is absent, `Origin`.** muster's own origin admits: its host
   equal to the request's `Host`, or its scheme and host equal to
   `MUSTER_PUBLIC_URL`'s (behind a proxy that rewrites `Host`). `null` and
   any other origin are refused.
3. **Only if both are absent, `Referer`.** A `Referer` on muster's own origin,
   by the same rule as `Origin`, admits.
4. **None of the three:** refused.

A refusal is `403` with the error `cross-origin request refused: this action
must come from muster's own pages`, logged (never the body) and counted in
`muster_csrf_refusals_total{route_class, reason}`.

- **Why `Sec-Fetch-Site` comes first.** Under `Referrer-Policy: no-referrer`
  (which some of muster's pages still serve) a browser's native form `POST`
  carries `Origin: null` while still carrying `Sec-Fetch-Site: same-origin`.
  An `Origin`-first rule would refuse muster's own no-JavaScript forms.
- **muster's default `Referrer-Policy` is `same-origin`.** It sends no
  `Referer` to any other origin, so a token in a URL stays out of other
  sites' logs, and a native form `POST` carries muster's real `Origin`. The
  share links (`/s/…`), the notification redirect and page content documents
  serve `no-referrer` themselves.
- **Bearer credentials are not checked.** Personal tokens, plugin, runner,
  mirror, device and agent tokens, and the hook and service secrets are
  headers a cross-site page cannot make a browser send. A request carrying a
  cookie and a bearer is judged by the credential its route's door read. The
  muster CLI and `muster-mirror` use bearer tokens.
- **The unauthenticated writes** (sign-in, sign-out, invite, reset and claim
  links, passkey sign-in, plugin and device pairing) do not pass through the
  session door. Sign-out reads the session, so it applies the same rule
  itself (GoTrue mode: `isSameOrigin`). ⚠ The password-mode sign-in reads
  the session only to exempt a signed-in browser from the sign-in throttle,
  and checks no origin. Every function that reads a browser session is listed
  in a test with its reason, and a new one fails it.
- **The routes that also carry `requireSameOrigin`** keep it. It reads
  `Origin`, then `Referer`, and refuses `Origin: null`, so a native form on
  one of those routes still needs its page served `Referrer-Policy:
  same-origin` ([sending-and-step-up.md](sending-and-step-up.md)).
- The route ledger, `internal/api/testdata/csrf_ledger.golden`, lists every
  write route as `cookie`, `bearer-only` or `unauthenticated`, and a test
  fails when a route is added, removed or moves class.

**What browsers send.** Current Chromium, Firefox and Safari send
`Sec-Fetch-Site` on every request to a **trustworthy** origin (`https`, or a
loopback address), and `Origin` on `fetch` and htmx writes.

- **A plain-`http` deployment on a non-loopback address gets no
  `Sec-Fetch-Site`** (measured in Chromium), so the check reads `Origin`.
  htmx and `fetch` writes carry the real `Origin`, and so does a native form
  `POST` from a page served `same-origin`, muster's default. ⚠ A form on a
  page served `no-referrer` carries `Origin: null` there and is refused.
- ⚠ **Not yet measured on a device:** the Android app's WebView (Chromium, so
  by documentation it sends `Sec-Fetch-Site`), the installed PWA (the browser
  itself) and Safari with its strict privacy settings. Each must show
  `Sec-Fetch-Site: same-origin`, or else muster's own `Origin`, or else a
  same-origin `Referer`, on a `fetch`, an htmx action and a form `POST`. Until
  that is captured, treat those clients as unverified.

## Machine credentials

| credential | acts as | notes |
|---|---|---|
| Personal API token (`mst_…`) | you: your user id and role | Minted only in the browser at `/account/tokens`, and shown once. Only its sha256 is stored. A token cannot mint another token. Revoking one refuses it on the next request. Disabling a user revokes all of their tokens (their mirror and flow runner tokens too, in the same transaction), and re-enabling them brings none back. |
| `MUSTER_HOOK_TOKEN` | user 1, admin | A shared secret, required: muster refuses to boot without it, in both modes. |
| `MUSTER_SERVICE_TOKEN` | user 1 | It also requires an `X-Muster-Actor` header. It fails closed (503 while unset) and must be at least 32 characters. |
| Mirror token (`mmt_…`) | the host it was minted for | A **team host token** (owned by a team) is served a reduced tool set and never remotely; see [Teams](#teams) ("Team hosts"). A disabled owner's token is refused, and the owner's open host connections end at the next token recheck (30s). Disabling a user revokes all of their mirror tokens, and re-enabling them brings none back. Opens `GET /mirror/connect`, the `AskUserQuestion` hook's `/mirror/ask` routes, `POST /mirror/mcp`, `POST /mirror/ask-attachments` (a file for one of that host's listed sessions' asks, stored as an upload of its owner: [attention.md](attention.md#attachments-and-context)) and `POST /mirror/pages/{id}/assets` (an image or video for a page made from that host: [pages.md](pages.md#images-and-video)). Through `/mirror/mcp` a leaked one reaches, beyond the host's tunnel and its questions, the attention asks of that host's own Claude Code sessions (file, wait, cancel), the tasks those sessions created, and — through `attention_answer` — an answer as the operator to any of its owner's open asks not raised by a mirrored session on that host (recorded as via MCP from that host; accepted like any of your answers, **including as `agent_dispatch` instructions** — the accepted risk in [attention.md](attention.md#mirrored-claude-code-sessions)) — and, through `connector_request_decide`, an approval as the operator of any of its owner's agents' pending connector requests (recorded and shown as via MCP from that host; the accepted risk in [connectors.md](connectors.md#deciding-a-request-from-your-own-session)), and — through `plugin_drafts_list` — a READ of every one of its owner's pending plugin reply drafts (the inbound message's sender, subject and full text, and the reply), and through `plugin_draft_decide` an approval as the operator of any of them, which sends that text to an outside party with no fresh sign-in (recorded on the audit row as via MCP from that host; any of the owner's mirrored sessions may approve, the one a message was routed to included; the accepted risk in [plugins.md](plugins.md#deciding-a-draft-from-your-own-session)), and — through `plugin_outbound_batch_get` / `plugin_outbound_batch_decide` — a READ of its owner's officers' outreach batches (the people, their engagement excerpts, the officers' reasons) and an approval as the operator of any follows and likes in them (recorded as via MCP from that host; the same accepted risk), and — through `agent_destroy` — the **teardown of any of its owner's agents but the chief** (the tasks are kept; **no audit row**: the host is named only in muster's server log; also an accepted risk, by operator decision — [attention.md](attention.md#mirrored-claude-code-sessions)) — and, through `agents_list`, a READ of its owner's agents (name, kind, worker pool, Claude account name but never a token, status, task) — and, through `chief_restart`, a **restart (Stop then Start) of its owner's own chief**, which with `force` cuts off a turn in flight (the same as the agent page's Stop/Start, which need no fresh sign-in either; **no audit row**: the host is named in muster's server log — [chief.md](chief.md#restarting-the-chief)) — and, with the chief's dispatch on, through `dispatch_request` and `dispatch_approve`, the **start of a new agent on its owner's behalf** with a prompt of the caller's choosing, or a **machine-job worker in any pool it names**, on one of its owner's Claude accounts — **including a pool that carries deployment privileges** (`MUSTER_AGENT_CC_BYPASS_POOLS`: the pool's job turns run without permission prompts; `MUSTER_AGENT_CC_POOL_DEPLOY_KEYS`: the worker's pod holds a mirror of that pool's private repository, and must work on that one repository) — but **a pool worker takes no prompt** on this door (refused at the request, and again at the approval), so the token cannot give that worker instructions: its only turns are the jobs submitted to its pool. A worker it adds holds one of the owner's live slots and takes a share of the pool's jobs. All within the chief's caps (each request and approval is an audit row naming the host and session; each start notifies the owner — [chief.md](chief.md#starting-an-agent-from-your-own-session)) — never another task (but for the comment `route_reply` adds to a task the chief [routed](plugins.md#routing-a-task-to-a-session) to one of that host's sessions), another host's session, another owner's ask or request, a secret, or a chief tool. 🔴 **With a token whose host has started agents for the chief** (the token is marked the first time its host advertises `-agent-spawn` or has a host dispatch, and stays marked until it is rotated), or from a host running a host officer, every one of those decisions — `attention_answer`, `connector_request_decide`, `plugin_draft_decide`, `plugin_request_decide`, `plugin_message_route`, `plugin_outbound_batch_decide`, `route_reply`, `dispatch_approve`, `agent_destroy` — is instead a confirmation card that runs only when the owner confirms it on their paired phone ([mirroring.md](mirroring.md#a-machine-that-starts-agents-is-step-up-gated)), so such a token alone decides none of them (but for a `route_reply` from the session the route was sent to). It **can still restart its owner's chief**: `chief_restart` is not gated on any machine, so such a token, or any process on that host, can restart the chief and with `force` cut off a turn in flight, by operator decision ([mirroring.md](mirroring.md#chief_restart-is-not-gated)). Rotate a host's token when you turn its `-agent-spawn` off, once the agents it started have ended. See [mirroring.md](mirroring.md) and [attention.md](attention.md#mirrored-claude-code-sessions). |
| Flow runner token (`mfr_…`) | its owner, confined to one host | A disabled owner's token is refused. Disabling a user revokes all of their runner tokens, and re-enabling them brings none back. See [browser-flows.md](browser-flows.md). |
| Plugin token (`mpl_…`) | its owner, on `/plugin/v1/*` only | Minted at `/plugins` and shown once. Every other route refuses it. See [plugins.md](plugins.md). |
| Server app token (`mpl_…`, a plugin row with `scope = 'server'`) | **nobody**; on the act-for routes, the one person whose live grant the `Muster-Act-For` subject names | Installed by an admin for the whole server and shown once. The same shape as a plugin token, so every other route refuses it. See [Server apps](#server-apps-and-the-people-they-act-for). |
| Device credential (`mdv_…`) | its owner, on `/api/device/*` only | A phone paired with the [Android app](design/android.md). See [Paired devices](#paired-devices). Every other route refuses it. |

The two shared secrets may not start with `mst_`, `mpl_` or `mdv_`. The server
refuses to boot if one does.

### Server apps and the people they act for

A server app ([plugins.md](plugins.md#server-apps)) is a plugin row with no
owner, so row-level security shows it to no principal: every read of one is
an explicit `authz.System` read keyed on the app's own id, in the System
ledger. Its token carries **no request principal** on `GET /plugin/v1/self`,
the hand-off redeem and `POST /plugin/v1/owners/check`: it is never given a
fake one, and it never borrows the installing admin (`installed_by` is
bookkeeping only). On the act-for routes (the knock, its withdrawal, and
`POST /plugin/v1/tasks`) the door reads the grant row for the
`Muster-Act-For` subject and admits the request as THAT person, through
`authz.ViaServerPlugin`, with the person's own role and row-level security.
The principal comes from the row, never from the header. `ViaServerPlugin`
is not a team door, and every rule a plugin is held to (no other route, no
routing-rule writes) holds for it too (`authz.IsPluginVia`).

- **A grant exists only after a redeem** of a code that person's own browser
  minted, signed in, on muster's page. The code records the minting
  session's **sign-in time**; a session with none recorded is asked to sign in
  again rather than mint a code nothing could revoke.
- **The grant is cut by the same cutoffs as a paired phone**: it is live only
  while that sign-in time is strictly after the person's Sign out everywhere
  and session cutoff, in whole seconds (at-or-before refused), and the person
  is enabled and has not hidden the app. The comparison uses the PERSISTED
  sign-in time of the minting session, never the redeem's time, because in
  GoTrue mode a sign-out is applied from each process's memory: a stolen
  session from before a Sign out everywhere could still mint on a restarted
  pod, and its code is refused at the redeem and on every later call whatever
  any process remembers. A sign-out in one browser does not end a grant.
- **It lasts 8 hours** from the latest redeem; Hide, a disable, Sign out
  everywhere, the session cutoff, an uninstall or a retired origin end it
  sooner.
- **A stolen server token** can knock for and file tasks for each person who
  opened the tab in the last 8 hours and has not hidden it, and only if it
  also holds their subjects. It reads nothing of anyone's. Uninstall or
  rotate ends it at once.
- **A linked pair** ([plugins.md](plugins.md#linked-server-apps)) shares one
  subject per person between two server apps, so the two can join their own
  records. Only an admin can make one, by installing an app with `muster-server
  app install --link-to` (there is no route for it, so no token or person can
  make or undo one), and only for an app that knows nobody yet, linked to an
  app whose subjects no other live server app holds (after an unlink, the
  former pair's two apps can never be linked to a third), and each
  person it affects is told so on **Account → Apps**. It widens nothing a
  token can do: the grant, the act-for door, `owners/check`, the knock caps
  and the task ledger stay keyed on the token's own app, so a token acting
  with the shared subject of a person who never opened its own app is the
  same `404` as any other subject.

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
says can write one. They are read back with `GET /api/pr-sessions` on the same
fail-closed door, and only the caller's own: a row names a session id that
`claude --resume` accepts and the host it ran on, so an unarmed server does not
serve it. 🔴 **Accepted risk: whoever holds that credential can
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

**Throttles.** Failed pairing attempts are limited to 10 a minute per client
address. The code is checked first, so a valid code is redeemed whatever the
address's count.
A device route request with a valid credential is charged only to its phone:
60 a minute. A refused request (no, a malformed, or an unknown or revoked
credential) is charged to its address: 120 a minute, then 429. So junk sent
from an address a phone shares cannot lock that phone out. Registering push,
which makes muster send a test push, is limited to 5 a minute per phone. The
address is the TCP peer, or the client a trusted proxy names
(`MUSTER_TRUSTED_PROXIES` with `MUSTER_CLIENT_IP_HEADER`; see
[Client addresses behind a proxy](configuration.md#client-addresses-behind-a-proxy)).

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
- **Start agents only** (`agent-create`): [`POST /api/agents`](provisioner.md#creating-an-agent-without-the-chief)
  and **nothing else** (every other route answers **403**). It is also the
  only token that reaches that route: a **full-access token is refused there**,
  so the CLI's ordinary token cannot create agents. `muster agent create`
  reads it from `MUSTER_AGENT_CREATE_TOKEN`.
- **Read Claude usage only** (`account-usage`): [`GET /api/cc-accounts/usage`](jobs.md#get-apicc-accountsusage),
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

### Mirror tokens on another muster instance

A host's mirror can call **another** muster instance's admin tools
([mirroring.md](mirroring.md#several-muster-instances)) with a mirror token
minted **on that instance** for that host (`muster-server mirror token create
<host>` there). The host never connects there, so the instance can bind no call
to a window. A call marked `X-Muster-Client: remote` is served unbound,
restricted to the mirror door's admin tools (`remoteMirrorTools` in
`internal/api/mirror_mcp.go`), and refused when that instance's hub knows
that host at all, online or offline. The hub's memory keeps an offline host
for 24 hours and loses it on a restart, so past that a host that mirrors
there is served unbound too. Every owner-authority tool such a call
names (the step-up ledger, `hostStepUpTools`) files a step-up card and runs
only once the owner confirms it from their paired phone. That instance
cannot tell whether the calling machine runs a host officer, so it always
asks.

What such a token reaches is therefore **a subset** of what the same token
reaches on an instance the host mirrors to. The difference is that no session
binding is involved: the session a call names (`X-Muster-Remote-Session`) is
recorded on the rows it writes but is not checked against a listing. That is
no wider in practice, because a mirror token can always open `/mirror/connect`
and list any session it likes. Revoke it on that instance like any other
mirror token.

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
