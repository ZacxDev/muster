# Design: contacts — the people you meet, their meetings, and how to reach them

**Status: proposed. Nothing here is built.** The operator's decisions are
recorded under [decisions](#decisions) and are binding. Everything else is the
recommended design; what is still open is under
[open questions for the operator](#open-questions-for-the-operator). Where
this page says how muster or the meetings plugin works today, it cites
`file:line` at muster commit `a9ec003df` and muster-meetings commit
`5e4655b`, and the code is the authority ([decisions.md](../decisions.md)).
A claim nothing confirmed is marked **UNVERIFIED** and has a
[phase 0 spike](#phase-0-spikes).

This page proposes **muster-contacts**: a plugin that keeps the people its
owner has met, the meetings they met in, and (later) the channels used to
reach them. It is built beside [meetings](meetings.md) and talks to it over a
small token-gated API. muster changes in exactly one place: an admin can mark
two server apps a **linked pair**, which then know each person by the same
subject ([the core change](#the-core-change-linked-server-apps)).

## Summary

- **The whole feature, from the owner's side:** a **Contacts** tab in
  muster's sidebar. It lists the people you have met, each with the meetings
  you met in. People arrive on their own: a guest who did not know you fills
  in a short card after the meeting, and (phase 2) everyone you invite to a
  scheduled meeting is added when the invite goes out. You can add, edit and
  delete anyone.
- **From a guest's side:** when the meeting ends (or they leave), a card asks
  for their name (already filled in from the lobby) and email, says plainly
  that the details go to the host, and has **Skip**.
- **Shape: a second server app, a direct API, and one core change**
  (decision 2). The plugin is a separate repository,
  `ZacxDev/muster-contacts`, built from the
  [plugin template](https://github.com/ZacxDev/muster-plugin-template), and
  installed by an admin as a [server app](../plugins.md#server-apps) exactly
  as meetings is. Meetings gains an **optional** contacts URL and token; with
  neither set it behaves exactly as today.
- 🔴 **Today the two plugins cannot share a person id, by design.** muster
  gives each person a different random subject under each server app
  (`internal/plugins/server.go:76-87`, [plugins.md](../plugins.md#server-apps)).
  The one core change (decision 7) lets an **admin** install a server app
  linked to one already installed (decision 8), after which both receive the
  **same** subject for each person. The app
  already in use keeps its subjects, so meetings' existing owners and
  meetings are untouched. Owners do nothing: every owner works the moment
  both apps are installed and linked. Every unlinked app keeps pairwise
  subjects exactly as today.
- **Phases** (decision 1): **P1** the contacts store, meeting history per
  contact, and the post-meeting card. **P2** scheduled meetings with emailed
  invites and an `.ics` file, sent through one SMTP relay per install with
  Reply-To the owner; invitees become contacts. **P3** channels: each
  contact's WhatsApp, Discord, Signal and email handles, then messages through
  muster's draft → approve → claim → result flow, behind an adapter seam.
- **Per owner, first deployment only** (decision 3): nobody sees anyone
  else's contacts. It ships to the operator's own deployment first; a
  deployment whose guests may be a client's people waits for an explicit
  operator decision ([before a second deployment](#before-a-second-deployment)).
- **A delete really deletes.** Deleting a contact removes its rows and every
  link to a meeting in the contacts store. The meetings plugin's own history
  (who knocked, the transcript) is unchanged by it.

## Goals

1. The owner can answer "who have I met, and when?" from one tab.
2. A guest who wants to stay in touch can leave their details in one step,
   and one who does not can skip it in one tap.
3. Nothing about a guest reaches the contacts store without that guest's
   action (the card) or the owner's (adding, inviting).
4. Meetings works exactly as today with contacts absent, unconfigured or
   down.
5. The data model and the API carry phases 2 and 3 without a migration that
   rewrites phase 1's rows.

## Non-goals

- **A shared or team address book.** Contacts are per owner (decision 3).
  Sharing one with a team is later work.
- **Importing or syncing** an external address book (a phone's, a mail
  account's). The owner types, invites or receives a card.
- **Recognising a guest by name.** Two people can type the same name; a
  name is never a match key ([merge rules](#merge-rules)).
- **Verifying a self-submitted email in phase 1.** There is no mail sender
  until phase 2 ([open question 4](#open-questions-for-the-operator)).
- **Enrichment.** No lookups of a person on the web, no avatars fetched
  from anywhere, no inferred company or role.
- **The chief or an officer reading contacts** in phases 1 and 2. Phase 3
  introduces the only agent-facing path, the draft flow, and every message
  through it is approved by the owner.
- **Calendar sync** in phase 2: an invite carries an `.ics` file; nothing
  reads anyone's calendar.

## What exists today

| piece | where | what it gives this design |
|---|---|---|
| Server apps | [plugins.md](../plugins.md#server-apps); `internal/plugins/server.go`; migration `internal/db/migrations/0172_plugin_server_scope.sql` | an admin installs an app once; every person gets the tab; the token acts as nobody and learns a person only from a hand-off their own browser minted |
| The pairwise subject | `internal/plugins/server.go:76-87` (`newSubject`: 128 random bits), `0172_plugin_server_scope.sql:151-152` (`UNIQUE (plugin_id, owner_id)`, `UNIQUE (plugin_id, subject)`), [plugins.md](../plugins.md#server-apps) ("It is **pairwise**") | the only person id a plugin receives: 22 base64url characters, different under every app, never derived from the user id. With it, the display name, and never a user id or an email |
| The `ref` hint | [plugins.md](../plugins.md#app-tab) ("Opened as `/apps/{id}?ref=<ref>`"); muster-meetings `internal/muster/client.go:215-219` | a 1–64 character `[A-Za-z0-9_-]` hint muster passes from its page's address to the app's redeem. A contact's meeting line uses it to open that meeting in the Meetings tab |
| The App tab's frame | [plugins.md](../plugins.md#app-tab) | `sandbox="allow-scripts allow-same-origin allow-forms allow-popups allow-downloads allow-modals"`: popups yes, top navigation no |
| Tab icons | `internal/plugins/app.go:35-52` | a fixed list of 16 emoji. No person; `book` (📖) is the address book |
| UI kit | [ui-kit.md](../ui-kit.md) | `mu-card`, `mu-input`, `mu-sheet`, `mu-icon-btn`, `mu-chip`, `mu-avatar`, `mu-toast`. Its icons have `search`, `plus`, `send`, `link`, `copy`, `close`, `check`, `people`, `more`, but **no** person, pencil, bin, envelope, calendar or download |
| Meetings: owners | muster-meetings `internal/store/store.go:99-103` | `owners(subject, display_name)`: keyed on the subject; nothing holds a user id or an email |
| Meetings: participants | `internal/store/store.go:139-151` | one row per knock: `id`, `meeting_id`, `kind` (`owner` / `guest`), `display_name` (what the guest typed, checked by `lobby.CleanName`), a random LiveKit `identity`, a hashed `ticket`, `state` (`waiting`, `admitted`, `denied`, `removed`, `left`, `ended`), times |
| Meetings: the guest flow | `internal/server/guest.go:96-157` (knock), `:216-266` (wait), `:268-337` (events), `:340-362` (Leave); `internal/server/calls.go:183-208` (End) | the guest's ticket lives in the page's memory and is sent in a body, never a URL (`noQuery`, `guest.go:82`). After End the link answers `410` (`ticketOf`, `guest.go:175-202`), so **a ticket cannot reach anything after End** |
| Meetings: revocation | muster-meetings README ("How it fits muster", item 5) | `/self` and `owners/check` every 60 s; `expired` keeps a person's meetings, `revoked` ends them |
| The draft flow | [plugins.md](../plugins.md#replying-drafts-you-approve), [claim](../plugins.md#post-pluginv1draftsclaim-collect-an-approved-reply), [result](../plugins.md#post-pluginv1draftsidresult-report-what-happened) | inbound message → chief's `reply_draft` → owner approves on the Asks page → plugin claims, sends, reports. It needs the `messages` capability, which **a server app may never hold** ([plugins.md](../plugins.md#server-apps): "Never `messages`") |

### Finding: the two plugins get different subjects for the same person

The brief assumed both plugins receive the same subject for one muster user.
They do not. Each grant row is `(plugin_id, owner_id)` with its own random
subject (`0172_plugin_server_scope.sql:140-152`, `server.go:76-87`), and
muster says so as a feature: "two apps cannot join their records on it".
Meetings would know the owner by one subject and Contacts by another, and
nothing either is told relates the two.

The operator chose the fix (decision 7): **a small core change**. An admin
installs Contacts linked to Meetings (decision 8), and muster then gives both apps
the same subject for each person
([the core change](#the-core-change-linked-server-apps)). Two other ways
were weighed and dropped: a connect step each owner does once through both
tabs (no core change, but every owner must do it and it can be done wrong),
and copying a person id between the plugins (impossible: neither is ever
told one).

## User flow

### The owner

1. Opens **Contacts** in the sidebar. Nothing to set up: once an admin has
   installed and linked the two apps, the people met in Meetings arrive here.
2. Holds meetings as today. Nothing changes in the call.
3. After a meeting, opens Contacts: anyone who filled in the card is at the
   top, marked **Added by them** with the meeting's title.
4. Taps a person: a sheet with their name, emails, the meetings you met in
   (newest first, each opening that meeting in the Meetings tab), and where
   the contact came from. **Edit** and **Delete** are icon buttons.
5. **Add** (icon `plus`) for someone met elsewhere.
6. (P2) In Meetings, **Schedule**: a time, a title, invitees picked from
   contacts or typed as emails. Each invitee gets an email with the link and
   an `.ics` file, and is a contact from then on.

### A guest

1. Joins by link exactly as today: types a name, knocks, is admitted.
2. When the owner presses **End**, or the guest presses **Leave**, the
   ended screen shows the card (unless they were invited, or their browser
   holds a receipt from an earlier card for this owner: [when the card shows](#when-the-card-shows)):

   > **Stay in touch with *Ada*?**
   > Your name and email go to Ada, who will see that you were in this
   > meeting. Nothing else is shared.
   > Name `[Grace           ]`  Email `[                ]`
   > **Send** · Skip

3. **Send**: "Sent to Ada." **Skip**: the card goes. Either way it does not
   come back for this meeting.

## Architecture

```
 owner's browser (muster origin)                                guest's browser
 ┌─────────────────────────────────────────────┐               ┌──────────────────────┐
 │ /apps/<contacts id>   /apps/<meetings id>   │               │ meetings guest page  │
 │  frame: contacts       frame: meetings      │               │  card after End/Leave│
 └───────┬────────────────────────┬────────────┘               └──────────┬───────────┘
         │ owner API (bearer)     │ owner API (bearer)                    │ POST /card
         ▼                        ▼                                       ▼
 ┌───────────────────┐  internal  ┌──────────────────────────────────────────────┐
 │ muster-contacts   │◄───────────│ muster-meetings                              │
 │  owner app, API   │  bearer    │  unchanged with no CONTACTS URL; with one:   │
 │  SQLite           │  token     │  card, outbox                                │
 └───────┬───────────┘            └──────────────┬───────────────────────────────┘
         │ redeem, /self, owners/check           │ redeem, /self, owners/check, notify, tasks
         ▼                                       ▼
 ┌──────────────────────────────────────────────────────────────────────────────┐
 │ muster-server: two server apps, each its own token; one linked pair, one     │
 │ subject per person across the two                                            │
 └──────────────────────────────────────────────────────────────────────────────┘
```

- **muster never calls either plugin**; each calls muster as server apps do.
- **Meetings calls Contacts; Contacts never calls Meetings.** One direction,
  so meetings' behaviour can only change when it chooses to call.
- **Contacts' API is on an internal listener**, like meetings' webhook
  listener (`:8081`), reachable only from meetings by the network policy.
  The owner app is on the public listener, under its own origin in
  `MUSTER_PLUGIN_ORIGINS`.
- **Two tokens, never one.** Each plugin keeps its own `mpl_` server token
  for muster. The contacts API token is a separate shared secret, in both
  plugins' Secrets, that muster never sees.

### Why these boundaries

- **A separate plugin, not a meetings feature,** because contacts outlive
  meetings: P2 invites, P3 channels, and an owner who never holds a meeting
  but keeps contacts.
- **Meetings pushes, contacts does not pull,** so a contacts outage, bug or
  compromise cannot slow a call, and a meetings install with no URL makes no
  call at all.
- **Meetings keeps no copy of the contact list.** It asks contacts only
  narrow questions about its own guest ([the API](#the-contacts-api)).

## The core change: linked server apps

One small muster change, its own PR before any plugin work
([phase 0](#phased-plan)): an admin can install a server app **linked** to
one already installed, and a linked pair knows each person by **one**
subject. Everything else about server apps stays as it is.

### What the admin does

The smallest surface is the command line muster already has for server
apps (`cmd/muster-server/app_cmd.go`), plus a read-only line on Account →
Apps (decision 8):

```
muster-server app install … --link-to <id>   # the new app is linked to app <id>
muster-server app unlink <id>
muster-server app list                        # gains a "linked with <id>" column
```

- `--link-to` names the app whose subjects are kept (meetings); the new app
  (contacts) takes them. The new app and its pair are made in **one
  transaction**, so the new app knows nobody when it is linked and nobody
  can open it unlinked. Linking two apps that are both already installed is
  not offered.
- **No page form and no bearer API**: linking changes what two apps may
  join, and the CLI already needs the shell and database access only an
  operator has. **Account → Apps** adds one line to each linked app's
  description: "*Meetings* and *Contacts* know you as the same person." The
  operator reads the pair in `app list`.
- The install's audit row `plugin.server.installed` records `linked_to`;
  `plugin.server.unlinked` records the partner (both `via: cli`).

### The rules

- **Server apps only, live ones only, two at a time.** `--link-to` must
  name a `scope = 'server'` app that is not revoked and is in no pair. An
  app is in at most one pair. Personal plugins are never linked. A refused
  link installs nothing.
- 🔴 **A source whose subjects are already shared is refused** (round 1's
  Y1). After an unlink, the former pair still share the subjects of the
  people both knew; a new app linked to either would share them with the
  other too, an app it was never linked to and that Account → Apps would
  not name. So `--link-to` is refused while any of the source's subjects is
  held by another live server app: "install the new app without
  `--link-to` instead". Uninstalling the former partner clears it (its rows
  go with it). A person's subject is shared only between the two apps of
  one link, which keeps "pairwise unless linked" literally true.
- **The source keeps every subject it has.** The link writes no
  `plugin_server_users` row and changes none. Meetings' owners, sessions and
  meetings, keyed on its current subjects (`internal/store/store.go:99-125`
  in muster-meetings), are untouched.
- **A new grant takes the partner's subject.** When either app of a pair
  needs a subject for a person (the redeem, or Hide), muster uses the
  subject that person already has under the partner, and only when there is
  none makes a new random one. So a person who has opened only Meetings is
  known to Contacts by Meetings' subject the first time they open Contacts,
  and one who opens Contacts first gives Meetings the subject Contacts drew.
- **Everything else stays per app.** The grant (its 8 hours, its
  `signin_at`), Hide, `owners/check`, `Muster-Act-For`, the knock caps and
  the task ledger are each keyed `(plugin_id, …)` as today. A shared subject
  lets the two apps join their **own** records; it does not let one app's
  token act for a person through the other app's grant (the act-for door
  reads `plugin_id` from the token, never from the subject).
- **Unlink** deletes the pair and nothing else. People already known to
  both keep the same subject under both (what the two apps have learned
  cannot be unlearned, so muster does not pretend to: re-keying would only
  orphan their data). People who first open either app after the unlink get
  independent subjects again. The command says so when it runs.
- **Uninstall of either app ends the pair** in the uninstall's own
  transaction, and its audit row names the partner (`unlinked_from`). A
  reinstalled app is a new id, linked only if installed `--link-to` again.

### Schema: migration `0178`

One row per app, two per pair, so "each app in at most one pair" is
constraints alone (round 0's simplification; it replaced a `low_id`/`high_id`
row with two unique indexes and a trigger):

```sql
CREATE TABLE IF NOT EXISTS public.plugin_server_pairs (
    plugin_id   BIGINT      NOT NULL PRIMARY KEY REFERENCES public.plugins (id) ON DELETE CASCADE,
    partner_id  BIGINT      NOT NULL REFERENCES public.plugins (id) ON DELETE CASCADE,
    linked_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT plugin_server_pairs_not_self CHECK (plugin_id <> partner_id),
    CONSTRAINT plugin_server_pairs_both UNIQUE (plugin_id, partner_id),
    -- every row's mirror exists; deleting either deletes the other
    CONSTRAINT plugin_server_pairs_mutual FOREIGN KEY (partner_id, plugin_id)
        REFERENCES public.plugin_server_pairs (plugin_id, partner_id) ON DELETE CASCADE
);
```

- The primary key gives an app at most one row; the mutual key makes a
  second partner for B (C → B) need B → C, a second row for B, which the key
  refuses. Both rows are written by one `INSERT`.
- **No owner column**: forced row-level security with a `scope_all` policy
  only, like `signin_requests`; every statement is an explicit
  `authz.System` one. `GRANT SELECT, INSERT, DELETE … TO muster_app`.
  Rollback: `DROP TABLE`, which leaves every subject as it is.
- `plugin_server_users` is **not changed**. Its `UNIQUE (plugin_id, subject)`
  (`0172_plugin_server_scope.sql:152`) still holds: a shared subject appears
  once under each of the two apps.

### The code seams

- 🔴 **One function chooses a subject.** The two writers that called
  `newSubject()` (`internal/plugins/server.go`), the redeem (the grant
  upsert) and Hide (a row for a person who hides an app they never opened),
  both call `subjectFor(tx, appID, ownerID)`: the partner's subject for that
  owner if the app is paired and the partner has a row, else `newSubject()`.
  A Go-AST ledger test pins `newSubject` to exactly one caller, `subjectFor`,
  so a third writer cannot skip the pair.
- **The race.** Two first redeems for one person, one per app of a pair, at
  the same moment, would each find no partner row and draw two subjects.
  `subjectFor` takes `pg_advisory_xact_lock` keyed on the **person**, on
  every draw, before it reads, so the second waits and finds the first's
  row. (Keyed on the person rather than the pair, so "is the app paired, and
  does the partner know them" is one statement under the lock, with no
  separate path for an unlinked app; one person's simultaneous first opens
  of two apps wait a few milliseconds for each other.) No other race needs a
  lock: the new app is invisible to every other transaction until its
  install commits, by which time its pair exists.
- **Link at install**: `InstallServerApp` with `LinkTo` runs `linkAtInstall`
  in its own transaction, holding the source row `FOR SHARE` (an uninstall
  racing it waits). **Unlink**: `UnlinkServerApp(ctx, id, by)`, one
  `DELETE` (the mutual key removes the mirror). `UninstallServerApp` gains
  `DELETE FROM plugin_server_pairs WHERE plugin_id = $1`.
- **Display:** `ServerApps` (for `app list`) and `AccountServerApps` read the
  partner's label.
- **Docs:** [plugins.md](../plugins.md#server-apps)'s "It is **pairwise**"
  gains the exception for a linked pair; [auth.md](../auth.md#server-apps-and-the-people-they-act-for)
  and [cli.md](../cli.md) gain the command and the Account → Apps line.

### The security argument

- **Pairwise subjects protect people from apps colluding without the
  admin.** A link is that comparison, made **explicit, by an admin**, for
  two apps the same admin installed, whose origins and tokens that admin
  already controls, and is shown to every person it affects (Account →
  Apps).
- **Only an admin can link.** There is no `/plugin/v1` route for it, so no
  server token, personal token or person can make or undo one; the CLI
  needs the server's database credentials.
- **It widens nothing a token can do.** Acting, notifying, filing tasks and
  `owners/check` stay keyed on the token's own app.
- **It never rewrites a subject,** so a link cannot move one person's data
  under another person's id: the new app knows nobody when it is linked,
  and the absence of any `UPDATE … SET subject` is tested.

### Tests (in muster)

- **Invariant: unlinked stays pairwise.** Two unlinked apps, three people:
  different subjects (an invariant guard).
- **Linked: one subject, either order**; three people with pairwise-distinct
  subjects.
- **The source's subjects are kept.** People known to meetings before
  contacts is installed `--link-to` it: the new app has no rows right after
  the install, answers meetings' existing subject, and every row of the
  source is byte-for-byte what it was.
- **Refusals, each installing nothing:** an unknown id; a personal plugin;
  an uninstalled app; an app already paired (either side of a pair); an
  app whose subjects are shared from an earlier pair (unlink, then
  `--link-to` either former side: refused; once the former partner is
  uninstalled: links). Each
  with its own error text, and no app row left by the refused install's
  name (**install `--link-to` is atomic**).
- **Hide on an adopter before first open** makes a row with the partner's
  subject.
- **The race:** two concurrent first redeems on the pair for one person end
  with one subject (50 runs, a row-lock barrier).
- **Cross-app acting is still refused,** both directions.
- **Unlink:** both rows go; existing people keep one subject; a new person
  gets two.
- **Uninstall** of either deletes the pair.
- **The ledgers:** `newSubject` has one caller; `linkAtInstall` one caller
  (the install); `ServerAppIn.LinkTo` is set and `UnlinkServerApp` called
  only by the CLI; the pairs table is spelled only in System functions; no
  SQL sets `plugin_server_users.subject`.
- **CLI:** `install --link-to`, `unlink`, `list` output and exit codes;
  audit rows. The migration's constraints, each by name.

### Mutation rows (core)

| guard | mutation | must turn red |
|---|---|---|
| pair lookup | `subjectFor` always calls `newSubject` | "linked: one subject" |
| pair scoping | look up a partner row without the pair (any app's row for the owner) | "invariant: unlinked stays pairwise" |
| one writer | Hide calls `newSubject` directly | "hide on an adopter" and the `newSubject` ledger |
| no rewrite | at install, copy the source's subjects onto the new app, or re-key the source | "the source's subjects are kept" (and the no-`UPDATE` ledger) |
| server only | allow a personal plugin as `--link-to` | "refusals: personal plugin" |
| one pair | drop the primary key on `plugin_id`; drop the mutual key | "refusals: already paired"; "unlink: both rows go" and the migration test |
| race lock | drop the advisory lock | "the race" |
| act-for per app | resolve the act-for grant by subject alone | "cross-app acting is still refused" |
| uninstall | leave the pair on uninstall | "uninstall deletes the pair" |
| atomic install | a refused link does not fail the install | "refusals: install `--link-to` is atomic" |
| no third app | drop the shared-subjects check | "a link to an app whose subjects are shared is refused" |
| CLI only | set `LinkTo` from the `/admin/apps` handler | the CLI-only ledger |

## Data model (the plugin's SQLite)

One database, `contacts.db`, `STRICT` tables, `PRAGMA foreign_keys = ON`,
`PRAGMA secure_delete = ON`, directory `0700`, file `0600`, as meetings
does (`store.go:211-216`). Every row that holds a person's data carries
`owner` (the subject the linked pair shares for that person) and every
query filters on it.

```sql
CREATE TABLE owners (
  subject      TEXT PRIMARY KEY CHECK (length(subject) = 22),  -- the pair's shared subject; never a user id
  display_name TEXT NOT NULL DEFAULT '',                       -- from the redeem; never an email
  redeemed     INTEGER NOT NULL CHECK (redeemed IN (0, 1)),    -- 1 once this person opened Contacts
  collect      INTEGER NOT NULL DEFAULT 1 CHECK (collect IN (0, 1)),  -- Settings: "Add people from Meetings"
  updated_at   INTEGER NOT NULL
) STRICT;

CREATE TABLE owner_sessions (                                  -- as meetings: bearer in page memory
  token_sha256 TEXT PRIMARY KEY,
  subject      TEXT NOT NULL REFERENCES owners (subject),
  created_at   INTEGER NOT NULL,
  expires_at   INTEGER NOT NULL
) STRICT;

CREATE TABLE contacts (
  id          TEXT PRIMARY KEY CHECK (length(id) = 22),        -- random; the contact_ref meetings holds
  owner       TEXT NOT NULL REFERENCES owners (subject) ON DELETE CASCADE,
  name        TEXT NOT NULL DEFAULT '',
  name_source TEXT NOT NULL CHECK (name_source IN ('owner', 'self', 'invite')),
  note        TEXT NOT NULL DEFAULT '',                        -- the owner's own words, at most 2,000
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL
) STRICT;
CREATE INDEX contacts_owner_name ON contacts (owner, name COLLATE NOCASE);

CREATE TABLE contact_emails (
  contact_id  TEXT NOT NULL REFERENCES contacts (id) ON DELETE CASCADE,
  owner       TEXT NOT NULL,
  address     TEXT NOT NULL,                                   -- as given, for display and sending
  address_key TEXT NOT NULL,                                   -- the merge key (below)
  verified_at INTEGER,                                         -- P2: a confirmation link was followed
  source      TEXT NOT NULL CHECK (source IN ('owner', 'self', 'invite')),
  created_at  INTEGER NOT NULL,
  PRIMARY KEY (owner, address_key)                             -- 🔴 one contact per email per owner
) STRICT;

CREATE TABLE contact_handles (                                 -- P3; created empty in P1
  contact_id TEXT NOT NULL REFERENCES contacts (id) ON DELETE CASCADE,
  owner      TEXT NOT NULL,
  kind       TEXT NOT NULL CHECK (kind IN ('whatsapp', 'signal', 'discord', 'email')),
  value      TEXT NOT NULL,
  value_key  TEXT NOT NULL,                                    -- E.164, a Discord user id, an email key
  source     TEXT NOT NULL CHECK (source IN ('owner', 'self', 'inbound')),
  created_at INTEGER NOT NULL,
  PRIMARY KEY (owner, kind, value_key)
) STRICT;

CREATE TABLE provenance (                                      -- how a contact came to be, and each merge
  id         INTEGER PRIMARY KEY,
  contact_id TEXT NOT NULL REFERENCES contacts (id) ON DELETE CASCADE,
  owner      TEXT NOT NULL,
  kind       TEXT NOT NULL CHECK (kind IN ('owner_entered', 'self_submitted', 'invited', 'merged')),
  meeting_id TEXT,                                             -- self_submitted / invited: which meeting
  said_name  TEXT NOT NULL DEFAULT '',                         -- the name a card sent, when it differs
  at         INTEGER NOT NULL
) STRICT;

CREATE TABLE meetings (                                        -- only meetings some contact is linked to
  owner      TEXT NOT NULL,
  meeting_id TEXT NOT NULL,                                    -- meetings' own id
  title      TEXT NOT NULL DEFAULT '',
  started_at INTEGER,
  ended_at   INTEGER,
  PRIMARY KEY (owner, meeting_id)
) STRICT;

CREATE TABLE meeting_links (                                   -- "met at meeting X"
  owner          TEXT NOT NULL,
  contact_id     TEXT NOT NULL REFERENCES contacts (id) ON DELETE CASCADE,
  meeting_id     TEXT NOT NULL,
  participant_id TEXT NOT NULL,                                -- meetings' participants.id
  identity       TEXT NOT NULL,                                -- meetings' LiveKit identity, for the transcript's lines
  role           TEXT NOT NULL CHECK (role IN ('guest', 'invitee')),
  typed_name     TEXT NOT NULL,                                -- what they typed at the knock
  via            TEXT NOT NULL CHECK (via IN ('card', 'invite', 'receipt', 'owner')),
  joined_at      INTEGER,
  left_at        INTEGER,
  PRIMARY KEY (owner, meeting_id, participant_id),
  FOREIGN KEY (owner, meeting_id) REFERENCES meetings (owner, meeting_id)
) STRICT;

CREATE TABLE receipts (                                        -- a guest browser's proof of an earlier card
  receipt_sha256 TEXT PRIMARY KEY,
  owner          TEXT NOT NULL,
  contact_id     TEXT NOT NULL REFERENCES contacts (id) ON DELETE CASCADE,
  created_at     INTEGER NOT NULL
) STRICT;

CREATE TABLE idempotency (
  owner          TEXT NOT NULL,
  key            TEXT NOT NULL,                                -- 'card:<id>', 'attend:<meeting>:<participant>', …
  request_sha256 TEXT NOT NULL,
  status         INTEGER NOT NULL,                             -- the answer's status; never its body
  contact_id     TEXT REFERENCES contacts (id) ON DELETE CASCADE,
  at             INTEGER NOT NULL,
  PRIMARY KEY (owner, key)
) STRICT;

CREATE TABLE audit (
  at     INTEGER NOT NULL,
  owner  TEXT,
  event  TEXT NOT NULL,                                        -- 'contact.created', 'contact.deleted', 'link.made', …
  detail TEXT NOT NULL DEFAULT ''                              -- 🔴 never a name, email, handle, title or note
) STRICT;
```

**What is deliberately absent.** No muster user id or email of the owner
(the plugin is never told either). No IP address or user agent of a guest
(the card's rate limit is in memory). No copy of a transcript. No meeting
row without a contact linked to it.

### Provenance

Each contact's origin is the first `provenance` row; later rows record
merges. The detail sheet draws them as one line each, icon-led:

| kind | shown as |
|---|---|
| `owner_entered` | "Added by you" |
| `self_submitted` | "Added by them after *meeting title*" (decision 5), and "email not confirmed" until P2 verification |
| `invited` | "Invited to *meeting title*" |
| `merged` | "Also sent their details after *meeting title*" (with "they said *said_name*" when the card's name differed) |

### Merge rules

The **email key** is the only match key. `address_key` is the address
trimmed, NFC-normalised, with the domain converted to lower-case ASCII
(IDNA) and the local part lower-cased. Plus-addressing and dots are
**not** folded: `ada+work@` and `ada@` are two addresses, which the owner
may merge by hand.

1. **A card or an invite naming a known email key** links the meeting to
   that contact and adds a `merged` (card) or `invited` row. It never
   creates a second contact.
2. **A name is never overwritten automatically.** A contact the owner named
   keeps the owner's name; a card's different name goes in `said_name`.
   A contact whose name came from a card or an invite takes a newer card's
   name only while the owner has never edited it.
3. **An unknown email key** creates a contact (`name_source` `self` or
   `invite`) with that one address.
4. **The owner adding an email that another contact holds** is refused
   with "*Grace* already has this email" and **Merge** offered: merging
   moves every email, handle, meeting link, receipt and provenance row to
   the contact kept, adds a `merged` row, and deletes the other. One
   transaction.
5. 🔴 **The guest's answer never says which happened.** A card's submit
   answers the same body, status and timing class whether it created a
   contact, merged into one, or was dropped (not accepting):
   a guest cannot learn whether an email is in the owner's contacts.
6. **Impostors.** Anyone admitted to a meeting can type anyone's email.
   Until P2 verification exists, a card that merges into an existing
   contact is drawn with "email not confirmed" on that meeting line, so the
   owner never reads an unverified claim as fact.

### Deletion

- **Delete a contact** (owner, confirmed in a sheet): one transaction
  deletes the `contacts` row, and by cascade its emails, handles,
  provenance, meeting links, receipts and idempotency rows; then every
  `meetings` row left with no link. Then a `wal_checkpoint(TRUNCATE)`, so
  `secure_delete`'s zeroed pages are what is on disk. The audit row says
  `contact.deleted` and nothing about whom.
- **What does not change:** the meetings plugin's own records (the
  participant row with the typed name, the transcript lines under it) are
  untouched; they are meetings' and follow meetings' retention. The Delete
  sheet says so: "Meetings keeps its own record of the calls."
- **A receipt dies with its contact,** so that guest's browser sees the card
  again next time: a deleted contact is not silently re-created.
- **Delete everything** (Settings, danger): every row for this owner,
  including the link.
- **Backups.** If the deployment backs up the volume, a deleted contact
  survives in backups until they expire. The README states the backup
  retention as the outer bound of "deleted".
- **Uninstall** of the app in muster makes the data unreachable (new
  subjects on any reinstall) but does not delete the file. The README says
  to delete the volume.

## The contacts API

The **internal** listener (`CONTACTS_INTERNAL_LISTEN`, default `:8081`)
serves meetings, and only meetings:

- **Auth:** `Authorization: Bearer <CONTACTS_API_TOKEN>`, 32+ random
  bytes, compared in constant time, the same value in both plugins'
  Secrets. Missing or wrong: `401`, one body. Never logged.
- **Per-owner scoping:** every route names `owner`, the subject meetings
  holds for the person, which is contacts' subject for them too because the
  two apps are a [linked pair](#the-core-change-linked-server-apps). The
  door validates its shape and every query below runs with it. A
  `contact_ref` of another owner is the same `404` as an unknown one.
- **The owner's standing:** contacts polls muster's `owners/check` for the
  subjects it has redeemed, as meetings does. For those, `revoked` (hidden,
  disabled, signed out everywhere) refuses every write with
  `404 {"error": "not accepting"}`, and so does the owner's own **Add people
  from Meetings** switch turned off. 🔴 **`expired` does not refuse**: it
  only means the owner has not opened the Contacts tab in 8 hours, which is
  the normal state.
- **A person who has never opened Contacts** is accepted (an `owners` row
  with `redeemed = 0` is made), so every owner works the moment both apps
  are installed and linked. `owners/check` cannot help here: it answers
  `revoked` for a subject the app never redeemed, the same as for one that
  was hidden ([plugins.md](../plugins.md#server-apps)), so contacts cannot
  tell "never opened" from "hid it before ever opening it"
  ([open question 1](#open-questions-for-the-operator)).
- **Bodies** are JSON, at most 4 KiB (16 KiB for P2's invitee list), unknown
  fields refused (`400`).
- **Idempotency:** every write carries `Idempotency-Key`. The same key with
  the same body answers the stored status and changes nothing; the same key
  with a different body is `409`. Keys are kept 7 days.
- **Limits:** 200 card submissions per owner per day, 2,000 per install;
  past either `429` with `Retry-After`. Meetings treats a `429` as "retry
  later" for its outbox.

### Routes

| route | phase | body → answer | purpose |
|---|---|---|---|
| `GET /v1/owners/{owner}` | 1 | → `200 {"accepting": true}` or `404 {"error": "not accepting"}` | may meetings offer the card for this owner? Cached by meetings 60 s |
| `POST /v1/submissions` | 1 | `{owner, card_id, meeting: {id, title, started_at, ended_at}, participant: {id, identity, typed_name, joined_at, left_at}, name, email}` → `202 {"receipt": "<43 chars>"}` | a guest's card. Key `card:<card_id>` |
| `POST /v1/receipts/check` | 1 | `{owner, receipt}` → `200 {"known": true, "contact_ref": "…"}` or `200 {"known": false}` | is this guest's own receipt still a contact of this owner? |
| `POST /v1/attendance` | 1 | `{owner, contact_ref, via, meeting: {…}, participant: {…}}` → `204` | "met at meeting X" for a guest already known (receipt, P2 invite). Key `attend:<meeting>:<participant>` |
| `POST /v1/invitees` | 2 | `{owner, meeting: {…}, invitees: [{email, name} or {contact_ref}]}` → `200 {"invitees": [{contact_ref, name, email}]}` | invitees become contacts (decision 1), merged by email |
| `GET /v1/search?owner=&q=` | 2 | → `200 {"contacts": [{contact_ref, name, email}]}`, at most 10 | the Schedule form's invitee picker, proxied for the owner's own meetings session |

`receipt` is 32 random bytes, stored as SHA-256. The submission answers a
receipt **whatever happened** (merge rule 5): a dropped submission's receipt
is simply never stored, so a later check answers `known: false`.

`title` is the meeting's title as the owner set it (or the default
date-and-time label), at most 80 characters, and is the only meeting field
that is free text.

### What meetings sends, and when

| moment | call | only when |
|---|---|---|
| A meeting starts, and at most every 60 s while open | `GET /v1/owners/{owner}` | the URL is set |
| A guest's knock carries a receipt | `POST /v1/receipts/check` | accepting |
| A guest submits the card | `POST /v1/submissions` (through the outbox) | accepting, the card was offered |
| **End** | `POST /v1/attendance` for each admitted participant bound to a contact (a checked receipt, a P2 invite) | accepting |

At End meetings sends **nothing about a guest who skipped the card or never
saw it**: no name, no identity, no count. A guest is in contacts only by
their own card, the owner's invite, or the owner's hand.

### Failure behaviour

- 🔴 **Contacts down ⇒ meetings unaffected.** Every call to contacts runs
  off the request path of anything a call needs (knock, admit, token, End),
  with a 3 s timeout. A failed `owners` check uses the last answer for up to
  24 hours, and with none, offers no card. A failed `receipts/check` treats
  the guest as unknown (they see the card; their submission merges).
- **The outbox.** A submission or an attendance row is written first to
  meetings' own `contacts_outbox` table, in the same transaction that spends
  the card, and the guest is answered from that. A worker delivers it with
  backoff (5 s doubling to 10 minutes), keyed by its idempotency key, so a
  retry after a lost answer is harmless. `2xx` or `404` (not accepting:
  hidden, revoked or collection off) deletes the row; `400`/`409` deletes it and logs the event name;
  `401`, `429`, `5xx` and timeouts retry. A row older than **72 hours** is
  deleted undelivered and counted
  (`meetings_contacts_outbox_dropped_total`). The outbox holds a guest's
  name and email only until then.
- **The guarantee: no URL, no change.** `MEETINGS_CONTACTS_URL` and
  `MEETINGS_CONTACTS_TOKEN` are both set or both unset (one alone refuses to
  start). With both unset: no card route is mounted (`404`, as any unknown
  path), no card token is minted, the guest's `ended` event and the Leave
  answer are **byte for byte** today's, the outbox worker does not start, and on boot any rows
  left in `contacts_outbox` are deleted (the URL was removed; their PII has
  nowhere to go). A golden test pins it ([test plan](#test-plan)).

## The guest card

### When the card shows

An admitted guest's page shows the card, once, when the meeting ends for
them, if **all** of these hold:

1. Meetings has a contacts URL and contacts is accepting for the owner
   (`owners` check).
2. The participant was **admitted** at some point. A guest who only waited,
   was denied, or was **removed** by the owner sees no card.
3. The meeting ended (**End**, or the room emptied out), or the guest
   pressed **Leave**. A dropped connection is not leaving (`guest.go:340-343`);
   if their page is still open at End, they get it then.
4. The participant is **not bound to a contact**: not a P2 invitee who came
   by their own invite link, and not a guest whose receipt checked `known`.

**How "already a contact" is known without leaking the owner's list.** A
link guest is anonymous: meetings knows only a typed name, and a name is not
an identity. Asking for an email and then answering "you are already a
contact" would tell anyone who types an address whether it is in the
owner's contacts. So the only signals used are ones **the guest already
holds**:

- **An invite token** (P2): their own invite link says they are the
  invitee.
- **A receipt** (P1): after a card is sent, the guest page stores the
  receipt in `localStorage` on the meetings origin under a key derived from
  the meeting's owner (`HMAC(MEETINGS_LINK_KEY, owner subject)`, first 16 bytes,
  base64url: the page never learns the subject). The next knock to that owner
  sends it in the knock body; meetings checks it with contacts. A `known`
  answer binds the participant to the contact; the guest sees, instead of
  the card, one quiet line: "*Ada* has your details." with **Forget this
  browser** (clears the receipt).

What the receipt reveals: only to the browser that holds it, and only
whether the owner still has that contact. That is information about the
guest themself, not about anyone else ([open question 3](#open-questions-for-the-operator)).
Every other guest sees the card; a repeat costs one form, and the merge
rules make it a merge, not a duplicate.

### How meetings delivers it

- At End (or Leave) meetings mints a **card id** per eligible participant:
  32 random bytes, stored as SHA-256 on the participant (`card_sha256`,
  `card_expires_at` = 30 minutes on, `card_state` in `offered`, `sent`,
  `skipped`). It rides in the `ended` event the guest's open stream already
  receives (`guest.go:242`, `:302`) as `{"message", "card": {"id",
  "name", "owner"}}`, and in the Leave answer (today a `204`; with a card, a
  `200` with the same object).
- The page holds the id in memory, as it holds the ticket. It is never in a
  URL (the [ticket rule](meetings.md), `noQuery`).
- **`POST /card {id, name, email}`** and **`POST /card/skip {id}`**, on the
  guest listener, outside `/j/{code}` (the link is `410` after End). Each
  spends the id: a second use, an unknown id or an expired one is `404`
  "This form has expired." Both are `noQuery`.

### Fields and validation

| field | rule |
|---|---|
| Name | prefilled with what they typed at the knock; the same check, `lobby.CleanName` (1–40 characters, as at the knock). Required |
| Email | required; trimmed; at most 254 characters; exactly one `@`; a local part of 1–64; a domain of DNS labels with at least one dot, converted by IDNA; no spaces, no angle brackets, no display-name form, no comma or semicolon (one address only) |

Meetings validates before it stores; contacts validates again with the same
rules, from one shared test table ([contract tests](#test-plan)). An invalid
field is `400` with the field named, and the card keeps what was typed.

### Consent wording

Exactly, with the owner's display name (or "the host" when it is empty):

> **Stay in touch with *Ada*?**
> Your name and email go to Ada, who will see that you were in this meeting.
> Nothing else is shared.

- **Skip** is a visible text button beside **Send**, not a close glyph:
  declining is as easy as agreeing.
- **Nothing is pre-ticked, nothing is sent on close.** Closing the tab is a
  skip.
- No marketing sentence, no "recommended". It names the person who gets the
  data, what they get, and what they learn (that you were in this meeting).

### Abuse and rate limits

- **Only people the owner admitted** can receive a card, at most one each,
  so a stranger with the link cannot reach the form at all, and the
  number of cards a meeting can mint is the number of guests the owner
  admitted (at most 8 in the room at once).
- **Single use, 30 minutes.** A card id spent by Send or Skip, or past its
  time, is dead.
- **Per source address:** 10 card posts per 10 minutes (meetings' existing
  limiter, `internal/lobby/limiter.go`, its own bucket), and contacts'
  per-owner daily cap.
- **Bodies** at most 2 KiB.
- **The owner's safeguard** is that every self-submitted contact is marked
  as such and is one tap from deletion.

### What the owner sees

- The new contact at the top of the list with an **Added by them** chip
  (icon `send`) and the meeting's title.
- A merge shows on the existing contact's history as "Also sent their
  details after *meeting title*", with "email not confirmed".
- No push. The card is not urgent, and the knock budget
  ([plugins.md](../plugins.md#post-pluginv1notify-a-knock-notification)) is
  for knocks.

## The owner UI: the Contacts tab

The tab is the [App tab](../plugins.md#app-tab) of a server app, label
**Contacts**, icon `book` (📖, the nearest of the 16: `internal/plugins/app.go:45`).
Entry, sessions and revocation are meetings' exactly: `/enter` refuses
unless `Sec-Fetch-Dest: iframe`, redeems the code, keeps an `mco_` bearer in
page memory, no cookie, `frame-ancestors <muster origin>`, the 30 s alive
poll, `/self` and `owners/check` every 60 s.

The page links the [UI kit](../ui-kit.md) live and follows its theme. The
operator's preferences from the meetings UI pass apply: **simple, icon
buttons with a tooltip, subtle transitions on the kit's duration tokens,
little copy.**

| view | what is drawn |
|---|---|
| **List** | a `mu-input` with the `search` icon (filters by name and email as you type, on the server, 200 ms debounce); an icon button `plus` (Add); `more` (Settings). Rows: `mu-avatar--sm` initials, the name (or the email when there is none), the first email in `muted`, and a chip only when it says something (**Added by them**, **Invited**). Newest activity first. An empty list is one line: "People you meet in Meetings appear here." |
| **Detail** | a `mu-sheet` (bottom sheet on a phone, dialog above 640 px): name, emails (each with `copy`), the note, **Met** (each meeting: title, date, role; tap opens `/apps/<meetings id>?ref=<meeting id>` in the top window through a link the owner taps), and the provenance lines. Header icon buttons: edit, download, delete |
| **Edit** | the same sheet in place: name, emails (add, remove), note. **Save**, Escape cancels |
| **Delete** | a confirm sheet: "Delete *Grace*? Removes their details and meeting history here. Meetings keeps its own record of the calls." **Delete** (`mu-btn--danger`) |
| **Settings** | **Add people from Meetings** (a `mu-switch`, on); **Download all** (JSON); **Delete everything** (danger, typed confirmation) |

- **Icons the kit lacks** (person, pencil, bin, download, envelope,
  calendar) are drawn from a small sprite in the plugin, on the kit's
  24-unit grid in `currentColor`, until the kit has them
  ([open question 6](#open-questions-for-the-operator)).
- **Motion:** sheets and toasts use the kit's own (`--mu-dur-slow`,
  `--mu-dur-base`); a new row fades in with `--mu-dur-base`; reduced motion
  makes all of it instant, as the kit does.
- **Copy:** every visible sentence is in the table above or the card. No
  explanations on the list; the ⓘ is the tooltip.
- **Phones:** one column, the sheet full height, the search field sticky.

The owner API (`/api/*`, bearer `mco_…`, owner-scoped by the session's
subject): `GET /api/contacts?q=&cursor=`, `GET /api/contacts/{id}`,
`POST /api/contacts`, `PATCH /api/contacts/{id}`,
`DELETE /api/contacts/{id}`, `POST /api/contacts/{id}/merge {from}`,
`GET /api/contacts/{id}/export`, `GET /api/export`, `DELETE /api/all`,
`POST /api/link/start`, `POST /api/link/confirm {code}`, `DELETE /api/link`.

## Phase 2: scheduled meetings and emailed invites

### What changes

- **Meetings gains a `scheduled` state** before `open`: a time, a duration,
  a title and invitees. The link exists from scheduling (derived from
  `MEETINGS_LINK_KEY` as today). Opening it before the owner starts shows
  "This meeting starts at *time*" and the knock opens when the owner
  starts the meeting.
- **Schedule** (meetings tab, icon `plus` beside New meeting): when, how
  long, title, invitees (the picker searches contacts through
  `GET /v1/search`, or takes a typed email). At most **7** invitees (the
  8-participant cap, less the owner).
- **Each invitee gets their own link**: the meeting link with an invite
  token in the **fragment**, `…/j/<code>#i=<token>` (a fragment never
  reaches a server or a log). The guest page sends it in the knock body.
  Meetings stores the token as SHA-256 with the `contact_ref` it was sent
  to, on an `invitees` table. A knock with a valid token binds the
  participant to that contact (no card, `role = invitee`), and the lobby row
  says "*Grace* (invited)". It does not skip the lobby
  ([open question 7](#open-questions-for-the-operator)).
- **Invitees become contacts** (decision 1): scheduling calls
  `POST /v1/invitees` first; each email is merged or created
  (`invited` provenance), and meetings keeps only the `contact_ref`. If
  contacts is not accepting or is down, scheduling still works and the
  invites still go; the invitee rows wait in the outbox.

### The sender: one per install, Reply-To the owner (decision 4)

- **Meetings sends**, because the invite carries the join link, which only
  meetings can make; contacts never sees a join link. Configuration:
  `MEETINGS_SMTP_URL` (`smtps://` or `smtp://` with STARTTLS required),
  `MEETINGS_SMTP_USER`, `MEETINGS_SMTP_PASSWORD`, `MEETINGS_MAIL_FROM`
  (one address for the install). Unset: **Schedule** is drawn without the
  invite field and nothing is mailed.
- **From** is the install's address with the owner's display name as its
  phrase ("Ada via Meetings"); **Reply-To** is the owner's address.
- 🔴 **muster never tells a plugin the owner's email,** so Reply-To needs
  one the owner gives: Meetings → Settings → **Reply-To**, confirmed by a
  link mailed to it (so nobody can make invites reply to someone else's
  address). Until confirmed, invites carry no Reply-To and say "Reply to
  *Ada* directly" in the body. ([open question 2](#open-questions-for-the-operator).)
- **The message**: a plain-text and an HTML part; the title, the time in
  UTC and in the invitee's local time as a calendar sees it (from the
  `.ics`), the owner's display name, the link, and one line: "You were
  invited by *Ada*. Reply to this email to reach them." No owner-written free
  text beyond the title.
- **The `.ics`**: `METHOD:REQUEST`, one `VEVENT` with `UID:<meeting id>@<meetings host>`,
  `DTSTAMP`, `DTSTART`/`DTEND` in UTC, `SUMMARY` (the title), `URL` and
  `DESCRIPTION` (the invitee's own link), `ORGANIZER;CN=<owner name>:mailto:<MEETINGS_MAIL_FROM>`,
  one `ATTENDEE;RSVP=FALSE` (that invitee only: invitees do not see each
  other's addresses), `SEQUENCE`. A change re-sends with `SEQUENCE + 1`; a
  cancel sends `METHOD:CANCEL`. No RSVP is collected
  ([open question 8](#open-questions-for-the-operator)).
- **Limits:** 50 invite emails per owner per day, 500 per install; one
  re-send per invitee per hour. Audited as counts, never addresses.
- **Outbox:** each send is an outbox row (address, token, meeting) deleted
  once the relay accepts it; a relay refusal is shown on the invitee's row
  ("not delivered") and retried by the owner's tap. The address is kept by
  meetings only while it is in the outbox.

### Email verification for self-submitted contacts

With a sender in place, a card submission can optionally send "Confirm you
gave *Ada* your email" with a link; following it sets `verified_at` and
drops "email not confirmed" ([open question 4](#open-questions-for-the-operator)).

## Phase 3: channels, then messages

### P3a: channels as handles

- A contact's **handles** (`contact_handles`): WhatsApp and Signal as an
  E.164 number, Discord as a user id (the stable number, never the
  changeable username; the username is shown when an adapter can resolve
  it), email as an email key. Entered by the owner, or (P3b) recorded from
  an inbound message.
- The detail sheet lists them under the emails, each with its channel's
  glyph and `copy`. Matching is per `(owner, kind, value_key)`, the same
  one-person-per-key rule as emails.

### P3b: messages through muster's draft flow

The operator already runs self-hosted services for WhatsApp, Discord,
Signal and mail. Contacts does not replace them; it talks to them through
one seam.

**The adapter seam.**

```go
// One per channel kind; configured by URL and token, never built in.
type Adapter interface {
    Kind() string                                   // "whatsapp", "signal", "discord", "email"
    Normalize(raw string) (key string, err error)   // the value_key rule for this kind
    // Send delivers body to the handle once. nothingSent is true only when the
    // adapter KNOWS nothing reached the service (muster's nothing_sent).
    Send(ctx context.Context, handle, body, idempotencyKey string) (externalRef string, nothingSent bool, err error)
    // Inbound streams messages the service received, oldest first, from a cursor.
    Inbound(ctx context.Context, cursor string) (msgs []Inbound, next string, err error)
}
```

Each adapter is an HTTP client to one existing service, configured per
install (`CONTACTS_ADAPTER_<KIND>_URL`, `CONTACTS_ADAPTER_<KIND>_TOKEN`),
with the same plain-http rule as meetings' `MUSTER_URL`. A conformance
suite (a fake service per kind, the same cases for every adapter) is the
bar for adding one.

**The flow** is muster's, unchanged:

1. An adapter's inbound message from a known handle is posted to muster as
   a plugin message (`POST /plugin/v1/messages`), its sender shown as the
   contact's name and **UNVERIFIED** (the services, not muster, vouch for
   it). Unknown senders are not posted unless the owner turns that on per
   channel.
2. The chief drafts a reply (`reply_draft`); the owner approves it on the
   Asks page, on its exact text.
3. Contacts claims it (`POST /plugin/v1/drafts/claim`), sends it through the
   adapter for that message's channel and handle, and reports
   (`POST /plugin/v1/drafts/{id}/result`, with `nothing_sent` only when the
   adapter knows).
4. With `draft_checks`, contacts refuses a draft whose contact was deleted
   or whose handle no longer resolves, before it can be approved.

🔴 **What this needs that phase 1 does not have:** the `messages`
capability, which **a server app may never hold**
([plugins.md](../plugins.md#server-apps)). So P3b needs either a personal
plugin token per person (minted on `/plugins`, with `messages` and
`draft_checks`) beside the server app, and a way to bind that token's owner
to the contacts subject, or a core seam. An owner-initiated first message
(not a reply) has no draft path today at all. Both are
[open question 5](#open-questions-for-the-operator); P3b is not designed
past this until it is answered.

## Security and privacy

- **This is PII in a public-code plugin.** The code is public; the data
  never is. Nothing in the repository, its fixtures, its goldens or its CI
  logs is a real person: fixtures use `example.test` addresses and invented
  names, and the leak gate's pattern for an email address runs on the
  plugin's repository as on muster's.
- **Data minimisation.** Per contact: a name, emails, a note, handles (P3),
  and links to meetings by id with the typed name. No IP, no user agent, no
  transcript, no meeting without a linked contact. Meetings keeps a guest's
  email only in its outbox, for at most 72 hours.
- 🔴 **No PII in logs.** Log lines carry event names, ids that are random
  (contact id, meeting id), counts and statuses; never a name, email,
  handle, title, note, receipt, card id, code or token. The logger has one
  path for request lines that drops bodies and the `/card` path's fields,
  and a test logs a request whose body holds a fixture name and email and
  asserts neither appears (with a positive control that a deliberately
  logged fixture does appear, so the grep can see one).
- **Owner scoping** is one function: the door that resolves a session or a
  `owner` to an owner, and a store whose every method takes that
  owner as its first argument. A ledger test lists every SQL statement that
  touches a person table and fails when one lacks `owner = ?`.
- **Export on request:** **Download** on a contact gives every row about
  them (JSON, the same shape as the API), so the owner can answer a
  person's request for their data; **Delete** answers a request to be
  forgotten ([deletion](#deletion)). A guest who wants either asks the
  owner: the owner is the one holding the data, and the card says so.
- **Tokens and codes** are random 32 bytes, stored as SHA-256 where stored
  at all; compared in constant time; never logged.
- **One subject per person across the pair** is muster's doing, made by an
  admin, never by either plugin or a person
  ([the security argument](#the-security-argument)). Contacts trusts the
  `owner` meetings names because only meetings holds the API token, and
  meetings only ever names a subject muster gave it.
- **Network:** the internal API is reachable only from meetings (network
  policy), and contacts refuses to start when `CONTACTS_API_TOKEN` is
  shorter than 32 bytes or equals either plugin's muster token.

### Before a second deployment

Decision 3 defers any deployment beyond the operator's own until the
operator decides, because a second deployment's guests could be a client's
customers. What would need deciding first:

1. **Who controls the data:** the deployment's owner, each muster user, or
   the client whose call it was; and therefore whose words the card uses.
2. **Whether self-submission is allowed at all** on that deployment, or
   only owner-entered and invited contacts.
3. **Retention:** a default and a maximum for contacts and for the meetings
   outbox, and whether a contact with no meeting in N months expires.
4. **Where the data may live** (region, volume, backups) and the backup
   retention that bounds "deleted".
5. **Who may see a contact:** still strictly per owner, or shared within a
   team (the non-goal above).
6. **How export and delete requests are answered,** by whom, and how fast.
7. **The mail sender's identity** (P2) on that deployment's own domain.

## Failure modes

| failure | what happens |
|---|---|
| Contacts is down | Meetings works as today; cards are still shown while the cached `owners` answer says accepting, submissions wait in the outbox for up to 72 h; the Contacts tab shows muster's frame error until it is back |
| Meetings is down | Contacts works: list, edit, delete, add. Nothing new arrives |
| The API token is wrong on one side | Every call `401`; meetings logs `contacts: unauthorised` once per minute, offers no card after its cache runs out, and drops nothing from the outbox |
| The owner turns **Add people from Meetings** off, or hides Contacts | `404 not accepting` on the next call: the outbox rows for that owner are dropped, no card from then on |
| The owner is revoked in muster | Contacts refuses writes (`404`); meetings has already ended their meetings |
| A reinstall of either app | The pair ends with the uninstall. Install the app again `--link-to` the **surviving** app: the new install takes the very subjects the old install shared, so its old database lines up again |
| The SMTP relay refuses (P2) | The invitee row says "not delivered"; the meeting is scheduled all the same |
| An adapter's service is down (P3) | `nothing_sent` with `transient`, so muster retries; inbound pauses at its cursor |

## Phased plan

| phase | delivers | repositories | gate to the next |
|---|---|---|---|
| **0 (core PR)** | [linked server apps](#the-core-change-linked-server-apps): the migration, `subjectFor`, `app install --link-to` and `app unlink`, the Account → Apps line, the plugins.md / auth.md / cli.md exception | muster | its tests and mutation rows green; released and deployed on the first deployment before contacts is installed |
| **0 (spike)** | spike S1 | none | answered in this page |
| **1a** | contacts plugin: store, owner app, the Contacts tab, add/edit/delete/merge/export | muster-contacts (new, from the template) | unit, contract and e2e green; mutation sweep; installed **and linked** on the first deployment |
| **1b** | meetings: `MEETINGS_CONTACTS_URL`/`_TOKEN`, the card, the outbox, attendance at End | muster-meetings | the no-URL golden; contract tests against contacts' real server; the card e2e |
| **1c** | receipts: the guest browser's marker and the quiet line | both | after 1b has run for a while ([open question 3](#open-questions-for-the-operator)) |
| **2** | scheduled meetings, invite emails with `.ics`, Reply-To confirmation, invitees as contacts, optional email verification | both | an invite received and opened in two calendar clients; the `.ics` validated by a parser in CI |
| **3a** | handles on a contact | contacts | — |
| **3b** | messages through the draft flow, behind adapters | contacts (+ whatever open question 5 decides) | the adapter conformance suite; one channel end to end with a real approval |

muster's own repository changes by this page, the phase-0 core PR, and,
optionally, the kit icons of open question 6.

### Phase 0 spikes

(The earlier popup spike for an owner-side connect step is gone with that
step. That two unlinked apps answer different subjects, and a linked pair
the same one, is measured by the core PR's own tests, not a spike.)

- **S1:** the guest's `ended` event reaches a guest page that has been in
  the background for 10 minutes on a phone (the card's delivery path); if
  not, the page asks for the card on its next foreground through
  `POST /card/pending {ticket}`, which is the one place a ticket is accepted
  after End, for 30 minutes.

## Test plan

### Phase 1: contacts unit tests

- **Merge rules:** a table test, one row per rule above, including the
  IDNA domain, the not-folded plus address, a card whose name differs (kept
  in `said_name`), a contact named by the owner (never overwritten), and a
  manual merge moving every child table (asserted by counting each table
  before and after).
- **Deletion:** after a delete, a query of **every** table for the contact's
  id and for its email key returns nothing (the table list is read from
  `sqlite_master`, so a new table is covered without editing the test, and
  the test fails if it finds a table it does not know how to query); the
  orphaned `meetings` row is gone; a raw read of the database file does not
  contain the fixture email (the `secure_delete` claim, with a positive
  control that the file does contain it before the delete).
- **Owner scoping:** two owners with pairwise-distinct fixtures (names,
  emails, ids, meeting ids all different, and none equal to a constant the
  assertion names); every API and owner route called as owner A with owner
  B's ids answers the same `404` as an unknown id, byte for byte.
- **The SQL ledger:** every statement on a person table has `owner = ?`.
- **Accepting:** a subject contacts never redeemed is accepted; a redeemed
  one answered `revoked` by muster is refused, `expired` accepted; the
  owner's **Add people from Meetings** off is refused.
- **Validation:** the shared field table (below).
- **Logs:** the no-PII test with its positive control.

### Phase 1: the API contract, shared by both plugins

One table of cases in a small Go package, `contactsapi`, in the contacts
repository and imported by meetings' tests at a pinned version: each case a
request and the exact expected status and body. Contacts runs it against
its real server; meetings runs it against the same real server binary in
its integration job, so a change on either side that breaks the other is
red in both repositories. It covers every route, `401`, `404 not accepting`,
idempotency (same key same body, same key different body `409`), the
`429`, `expired` accepted and `revoked` refused, and the field table:
valid and invalid names and emails, each with its literal expected answer.

### Phase 1: meetings tests

- 🔴 **No URL, no change:** with both variables unset, a golden of the
  guest's `ended` event, the Leave answer, the route table, and the boot
  log, compared with the same goldens taken at the pre-change commit
  (red-at-base is not possible for an equality to the base; the mutation
  row below supplies the failing case). One variable alone refuses to
  start.
- **Card eligibility:** one case per condition of
  [when the card shows](#when-the-card-shows), each alone false, and all
  true.
- **Card spending:** Send, Skip, a second use, an expired id, an id from
  another meeting.
- **Outbox:** contacts answering each class (`2xx`, `404`, `400`, `409`,
  `401`, `429`, `5xx`, timeout) leads to the expected keep or delete; the
  72-hour drop; a retry after a lost answer creates exactly one contact
  (against the real contacts server).
- **Contacts down:** a meeting's knock, admit, token and End complete within
  their usual time with contacts black-holed (a listener that never
  answers), measured against a run with contacts up.

### Phase 1: end to end (Playwright)

- **The card:** owner and guest in a call (the fake LiveKit fixture
  meetings already has); owner ends; the guest's card shows with the knock's
  name; Send; the Contacts tab shows the contact with **Added by them** and
  the meeting. Then: Skip (no contact); Leave (card on Leave); a removed
  guest (no card); a second meeting with the same email (one contact, two
  meetings); contacts stopped during the submit (the guest still sees
  "Sent", the contact appears after contacts returns).
- **The tab:** list, search, open, edit, delete (gone after reload; the
  meetings transcript still shows the guest's lines), export (the file
  holds the fixture), mobile and desktop goldens of list and sheet.
- **Linked pair, end to end:** against a test muster carrying the core PR,
  with both apps installed and linked: a person who has never opened
  Contacts holds a meeting, a guest sends the card, and the contact is
  there the first time the person opens Contacts. A second person's
  contacts stay empty (owner scoping across the real subjects).
- **Theme:** the page under two kit themes (meetings' fixture kits).

### Phase 2 tests

- `.ics` parsed by a library in CI (UID, SEQUENCE, CANCEL, one ATTENDEE);
  the email's MIME structure; the Reply-To appears only once confirmed; the
  invite token only in the fragment (a test asserts the server never sees
  `#` content: no route accepts it and no log line holds it); invitees
  merged by email; an invitee joining with their link gets no card; limits.

### Phase 3 tests

- The adapter conformance suite per kind; the draft path against a test
  muster: an inbound message, an approved reply, the claim, the result with
  and without `nothing_sent`; a deleted contact's draft refused by the
  check.

### Mutation rows for every guard

Run as a scripted sweep in each plugin's CI, one mutant at a time, applied,
tested, reverted, with `GOFLAGS=-count=1` and one mutant known to be caught
in each batch as the positive control. A row names the test that must fail
**with that guard's own error**, not merely some test.

| guard | mutation | must turn red |
|---|---|---|
| owner scoping (door) | resolve the owner from the request body instead of the link row | "owner scoping" cross-owner `404` |
| owner scoping (store) | drop `owner = ?` from the contact read | "owner scoping", "SQL ledger" |
| contact_ref scoping | look up `contact_ref` without the owner | "owner scoping" attendance case |
| merge key | match on the raw address instead of `address_key` | merge rule "case differs" |
| merge: no duplicate | drop the `PRIMARY KEY (owner, address_key)` | merge rule "second card, same email" |
| merge: name kept | let a card overwrite an owner-named contact | merge rule "owner's name kept" |
| merge: no oracle | answer `200` for a merge and `202` for a new contact | contract "submission answers alike" |
| deletion | drop `ON DELETE CASCADE` from `meeting_links` | "deletion: every table" |
| deletion | skip removing the orphaned meeting row | "deletion: orphaned meeting" |
| accepting: revoked | accept writes for a redeemed subject muster answers `revoked` | "accepting" revoked case |
| accepting: switch | ignore **Add people from Meetings** off | "accepting" switch case |
| card: admitted only | offer the card to a `denied` or `removed` participant | "card eligibility" (one each) |
| card: not accepting | offer the card with `owners` answering `404` | "card eligibility: not accepting" |
| card: invitee | offer the card to a participant bound by invite | "card eligibility: invitee" |
| card: receipt | offer the card after a `known` receipt | "card eligibility: receipt" |
| card: single use | do not spend the id on Send; on Skip | "card spending" (one each) |
| consent: skip sends nothing | let Skip enqueue a submission | "card spending: skip" and e2e Skip (no contact) |
| consent: wording | change the consent sentence | the card golden (the whole normalised string) |
| no URL | mint card ids with the URL unset; mount `/card` unset | "no URL, no change" (one each) |
| no URL: outbox | keep outbox rows on a boot with no URL | "no URL, no change: outbox cleared" |
| outbox: 404 drops | retry a `404` forever | "outbox" `404` case |
| contacts down | call contacts on the knock path synchronously | "contacts down" timing case |
| `expired` accepted | refuse writes for `expired` | contract "expired accepted" |
| `revoked` refused | accept writes for `revoked` | contract "revoked refused" |
| API auth | accept any bearer; compare with `==` on a prefix | contract `401` cases |
| logs | log the request body on `/card` | "no PII in logs" |
| validation | accept two addresses separated by a comma | the field table |
| P2 invite token | read the token from the query | "invite token in the fragment" |
| P2 Reply-To | set Reply-To before confirmation | "Reply-To once confirmed" |

## Rollout

**The first deployment only** (decision 3).

0. **The core PR first:** released and deployed on the first deployment.
   Check `muster-server app list` shows the new "linked with" column.
1. **Contacts next.** Deploy the contacts plugin in the muster tenant
   beside meetings, on its own one-label subdomain of muster's host
   (`contacts-<x>.<domain>`), added to `MUSTER_PLUGIN_ORIGINS`. Install it on
   the command line, **linked to meetings**: `muster-server app install
   --name contacts --label Contacts --icon book --origin … --entry …
   --link-to <meetings id>` (no `--tasks`); check there is room (at most 5
   server apps). The pair exists the moment contacts does, so nobody can
   open contacts unlinked; meetings keeps its subjects.
2. Open the tab; add, edit and delete a contact by hand.
3. **Meetings next.** Set `MEETINGS_CONTACTS_URL` (the internal listener)
   and `MEETINGS_CONTACTS_TOKEN`; redeploy meetings. Nobody connects
   anything.
4. **Verify on the click path:** a meeting with a second browser as the
   guest; End; the card; Send; the contact in the tab with the meeting. Then
   Skip in a second meeting and confirm nothing arrived.
5. **Phase 2 and 3** follow the same order: contacts first, then meetings.

**What changes in the meetings plugin** (phase 1): two optional variables;
one migration adding `card_sha256`, `card_expires_at`, `card_state` to
`participants` and a `contacts_outbox` table; the card in `guest.js` and
the two card routes; the outbox worker; the `owners` cache;
attendance at End. Nothing on the call path waits for contacts.

**Rollback.**

- **Meetings:** unset the two variables and redeploy. It behaves as today,
  and deletes its outbox on boot. The added columns stay, unused.
- **Contacts:** Hide or uninstall the app in muster; the data stays on its
  volume until the operator deletes it. Meetings, still configured, finds
  contacts `404` or unreachable and offers no card once its cache runs out;
  unset its variables too.
- **The link:** `muster-server app unlink <id>` stops new people from
  sharing a subject; it does not re-key anyone (see
  [the rules](#the-rules)). Rolling back the core PR itself is its
  migration's `DROP TABLE`; subjects already shared stay shared.
- Either order works, because meetings never depends on contacts to run.

## Open questions for the operator

1. **Collect for an owner who has never opened Contacts?** With the pair,
   contacts can accept a card for a person from their very first meeting,
   which is what "every owner works the moment both apps are installed"
   asks. But `owners/check` answers `revoked` both for a subject an app
   never redeemed and for one the person hid, so contacts cannot tell
   "never opened Contacts" from "hid Contacts before ever opening it", and
   would collect for the second. (The core question this replaces, a
   connect step or a core change, was answered: decision 7.)
   **Recommendation: collect, as designed,** and make the Hide case
   visible instead: Account → Apps's line for a linked pair already says
   the two apps know you as one person, and showing the app again shows
   what was collected, with **Delete everything** one tap away. The strict
   alternative (collect only after the first open) is a one-line change if
   preferred.
2. **The owner's Reply-To address (P2).** muster never tells a plugin an
   email, so Reply-To needs the owner to type theirs once.
   **Recommendation: typed once in Meetings → Settings and confirmed by a
   mailed link before it is used;** invites before that say "reply to *Ada*
   directly" without a Reply-To.
3. **Receipts: recognise a returning guest in the browser?** It spares a
   repeat card and records attendance automatically, and reveals to that
   browser only whether the owner still has its contact.
   **Recommendation: yes, as phase 1c,** after the card has run alone for a
   while; until then every non-invited guest sees the card and repeats
   merge.
4. **Verify self-submitted emails?** Without it, anyone admitted can claim
   someone else's address (marked "email not confirmed").
   **Recommendation: yes, in phase 2** once the sender exists, as an
   optional confirmation the guest may ignore; the owner sees which are
   confirmed.
5. **Messaging needs `messages`, which a server app cannot hold (P3b).**
   Options: a personal plugin token per person beside the server app (a
   second thing for each person to set up, whose owner contacts would know
   by `owner_id`, never by the pair's subject), or a second core seam
   letting a server app file drafts for a person it holds a live grant for,
   plus a draft that starts a conversation rather than answering one.
   **Recommendation: decide at P3b, leaning to the core seam,** for the
   same reason decision 7 chose a core change over per-owner setup.
6. **Kit icons.** The kit lacks person, pencil, bin, download, envelope and
   calendar. **Recommendation: the plugin draws its own in phase 1, and a
   small additive kit PR (allowed in `v1`) follows,** after which the plugin
   switches to the kit's.
7. **Do invitees skip the lobby (P2)?** Their own link identifies them.
   **Recommendation: no, not in P2:** an invite link can be forwarded, and
   admitting is one tap; revisit with real use.
8. **RSVPs (P2).** Calendar replies go to the `ORGANIZER`, which is the
   install's sender address. **Recommendation: `RSVP=FALSE` and no RSVP
   handling;** replies by email reach the owner through Reply-To.

## Decisions

From the operator's brief and answers. All are binding on this design.
Decision 7 answers this page's first open question and revises decision 2.

1. **Phasing: contacts → invites → channels.** P1 a muster-contacts
   plugin (contacts, meeting history per contact, the post-meeting "add
   your details" card). P2 scheduled meetings with emailed invites and a
   calendar (`.ics`) file; invitees become contacts. P3 channels (WhatsApp,
   Discord, Signal, email) as each contact's linked handles, then messaging
   through muster's draft and approve flow.
2. **Integration: a direct, token-gated API between the plugins, plus one
   core change.** muster-contacts exposes a small API; the meetings plugin
   gets an optional contacts URL and token and records "met at meeting X"
   and the card's submissions. With no URL set, meetings behaves exactly as
   today. The one muster change is decision 7's linked pair; there is no
   other. (Revised by the operator's answer to the first open question:
   originally "no muster core change".)
3. **Ownership: per owner, the first deployment only.** Each muster user
   has their own contacts; nobody sees anyone else's. It ships to the
   operator's own deployment; any other waits for an explicit operator
   decision, since its guests' names and emails could be a client's data.
4. **Invite sender (P2): one plugin sender, Reply-To the owner.** The
   plugin sends through an SMTP relay configured per install; Reply-To is
   the owner.
5. **Guest card: added at once, marked self-submitted.** The guest becomes
   a contact at once, tagged "added by them after *meeting*"; the owner can
   edit or delete it. The card is optional, says plainly that the details
   go to the host, and can be skipped. A known email merges into the
   existing contact instead of creating a duplicate.
6. **Start: design doc, then build.** This page first, for the operator to
   read; then a new public repository, `ZacxDev/muster-contacts`, from
   muster-plugin-template, and the build with its tests.
7. **Linked server apps: a small muster core change.** An admin links two
   server apps (made at install, decision 8), and muster then gives both the same
   per-person subject. One small server change and migration, with tests,
   designed [here](#the-core-change-linked-server-apps). No connect step for
   owners, nothing to mis-wire, and every owner works the moment both apps
   are installed. Every unlinked app keeps pairwise subjects, and the app
   already in use keeps its subjects.
8. **Link at install.** `muster-server app install … --link-to <id>`: the
   new app is linked the moment it exists, so no one can open it first.
   This removes the adopter-knows-nobody refusal, the reinstall recovery and
   a locking change. Linking two apps that are already in use is not
   offered. (Operator's answer to round 0 of the core PR's audit, which also
   dropped the pair's `source_id` and the `/admin/apps` line, and kept
   `app unlink`.)
