# Design: team sharing on one muster server

**Status: decided. Phase 1 is in review; phase 2a is next (build order 1 → 2a → 3 & 4 → 2b).** The
operator accepted every recommendation ([operator decisions](#operator-decisions-2026-11-13)). The operator asked for
muster as "multiplayer": a personal instance for your own work, plus shared
team instances where people share sessions and officers. The operator's
decision is a design doc first, with no code.

Where this page says how muster works today, it cites `file:line` at `main`
`8e9d6b1f`, and the code is the authority ([decisions.md](../decisions.md)).
A claim nothing here measured is marked **UNVERIFIED**, and each one is an
acceptance check in a phase below. Every example is synthetic: users `ana`
(id 7), `ben` (id 8) and `cal` (id 9), a team `platform` (id 12), a host
`build-01`.

## Summary

- **Two layers, two designs.** Moving between a personal server and a team
  server is the *client* problem that [multi-tenant.md](multi-tenant.md) already
  solves (CLI and mirror built; Android proposed; the web switcher is being
  built separately). This page is the *server* problem: several people on
  **one** muster server sharing some sessions, officers and tasks, while
  everything else stays private.
- **A team is an owner.** A team is a row in `users` with `kind = 'team'`, which
  can never sign in. A shared thing is a row whose `owner_id` is the team. Every
  owner-keyed mechanism muster has (row-level security, the composite foreign
  keys, the mirror hub's `(owner, host)` key, one chief per owner, an officer's
  `owner`, `escalate_to`) then works for a team without being taught about
  teams.
- **Membership is a table, roles are three.** `team_members(team_id, user_id,
  role)`, role `viewer` < `operator` < `manager`. Viewer watches; operator types,
  answers and queues; manager mints the team's tokens, edits charters, approves
  outbound text and manages members.
- **Private by default, mechanically.** `owner_id` keeps defaulting to the
  acting **person** (`muster.uid`), so an insert that does not name the team
  lands private. Nothing a user already owns ever becomes visible by joining a
  team. Sharing is explicit: you mint a team host token, define a team officer,
  file a team task, or move a task to a team.
- **Row-level security grows one arm, on a ledgered set of tables.** The
  shareable tables swap `owner_isolation` for `owner_or_team`, which also
  admits a team the acting person belongs to, at a role threshold per table
  for reading and for writing. Every other table (credentials, connectors,
  devices, push, pins, preferences) keeps `owner_isolation` byte for byte.
- **No "act as team" mode.** Pages show your own and your teams' items grouped
  by owner; a write acts on the owner of the object it touches. There is no
  context switch to get wrong.
- **Every action names a person.** Audit rows, sends, answers, approvals and
  comments record the acting user beside the team owner (the precedent is
  `chat_messages.typed_by`, 0081).
- **Phases** (in build order, as the operator reordered them after phase 1):
  teams and membership with no shared data (1); the access core, the rule
  every team row meets (2a); team officers (3) and team tasks with a team
  chief (4); team hosts, the shared mirrored sessions, last (2b); and an
  optional read-only share of a single personal session (5). Each ships and
  rolls back alone.

## Goals and non-goals

**Goals**

1. Several people on one server can see, answer and type into the same
   mirrored sessions, according to a role.
2. An officer can belong to a team: members share its charter, its asks and
   its escalations, and outbound drafts get a defined approver.
3. A team can have a task board and, later, its own chief.
4. Everything a user owns today stays private unless they explicitly share it.
5. Every action on a shared thing is attributed to the person who did it.
6. Removing a member, disabling a user or leaving a team cuts access at once,
   including open terminals and queued messages.

**Non-goals**

- **Several servers in one client.** That is [multi-tenant.md](multi-tenant.md).
  A "team instance" in the operator's words can be a separate server (the
  client switches to it) *and* that server runs this design for its members.
- **Federation between servers.** A team on server A cannot see a session on
  server B.
- **Sharing a personal machine's shell.** A member typing into a session runs
  commands as that machine's Unix user (see the [threat model](#security-and-threat-model)).
  This page does not make that safe on a personal workstation; it makes it
  something you opt into with a team host.
- **Password mode.** `password` mode is one operator account
  ([auth.md](../auth.md)); teams need real accounts, so they need `gotrue`
  mode.
- **Fine-grained ACLs** per field, per window or per tool. Three roles, per
  team.
- **An admin "see everything" view.** Admins create teams; they read a team's
  data only as a member, as today an admin's ordinary pages show only the
  admin's own rows ([auth.md:54-57](../auth.md)).

## Part 1: what muster does today

### Accounts and isolation

| fact | where |
|---|---|
| Two roles, `admin` and `member`; invites always create a member | `internal/authz/authz.go:37-42`, [auth.md:45-48](../auth.md) |
| A request's identity is an `authz.Principal{UserID, Role, Via}`; System (`Via = system`) acts for every user | `internal/authz/authz.go:237-252`, `:290` |
| A principal's role is read from `users` per request | `internal/api/principal.go:135-142` |
| Every pool acquire sets `role = muster_app`, `muster.uid`, `muster.scope`; no principal sets nobody and fails closed | `internal/db/scope.go:85-113` |
| Every owned table has one policy, `owner_isolation`: `owner_id = muster.uid OR muster.scope = 'all'`, in USING and WITH CHECK; `owner_id` defaults to `muster.uid` | `internal/db/migrations/0007_row_level_security.sql:160-172` |
| Child rows point at parents by `(parent_id, owner_id)`, so a child can never name another owner's parent | `internal/db/migrations/0006_accounts_schema.sql:31-35` |
| A ledger test pins that the owned tables, the forced-RLS tables and the policy set are exactly the expected sets, and that **every** policy on an owned table is `owner_isolation`, `ALL`, `PERMISSIVE` | `internal/db/migration_0007_rls_test.go:171-245` (the check at `:237`) |
| `users` and `audit_log` are readable only for your own id (`self_read`) | `internal/db/migrations/0009_per_user_scoping.sql:50-55`, `:68-75` |
| `audit_log` records the actor (`user_id`, `via`), not the owner of what was acted on | `internal/db/migrations/0006_accounts_schema.sql:326-334` |
| The isolation walk drives **every** route in `routes.golden` as user B against user A's ids and requires each route to declare a class | `internal/api/isolation_test.go:60-98`, `:100-135` |
| `/admin/*` today is users and invites only; there is no cross-user data view yet | `internal/api/admin.go:66`, `internal/api/testdata/routes.golden:55-56`, `internal/api/isolation_test.go:85-88` |

About 67 tables are owned today (the ledger `ownedTables` + `ownedAfter0006`
in `internal/db/migration_0006_accounts_test.go:147-170`).

### Sessions

- **A mirror token belongs to the user who minted it.** `mirror_tokens.owner_id`
  (`internal/db/migrations/0008_mirror_tokens.sql`, header and table). The host
  connects with it, and the hub keys the host by **(token owner, host)**:
  `hostKey{owner, host}` (`internal/mirror/hub.go:68`), `ServeConn(ctx,
  tok.OwnerID, tok.Host, ws)` (`internal/mirror/service.go:219-259`). The
  window list lives in memory "under the token owner's account"
  ([mirroring.md:186-187](../mirroring.md)).
- **One chokepoint decides whose sessions a request may touch:**
  `owner(ctx)` in `internal/mirror/service.go:347-353` returns the principal's
  user id, and `Hosts`, `AttachWindow`, `WatchWindow`, `PasteWindow`,
  `ScrollWindow` and `CatalogWindow` all pass it to the hub
  (`internal/mirror/service.go:357-440`).
- **Typing needs step-up.** An interactive attach and every mirrored send need
  a fresh sign-in, read from the **browser session**, never the user
  ([sending-and-step-up.md:518-537](../sending-and-step-up.md)); the plugin
  checks it again itself (`internal/mirror/service.go:378-384`). The send queue
  stores the decision as `send_queue.stepup_at`
  (`internal/db/migrations/0019_send_queue.sql:42`, `:76`), and a disable or
  sign-out-everywhere cancels a user's queue (`internal/sendqueue/store.go:652`).
- **A session's question belongs to the token's owner.** `/mirror/ask` takes
  `owner, host` from the token (`internal/api/mirror_ask.go:179`) and opens the
  question under `askq.Key{Owner: owner, …}` (`:197`).
- **The mirror token's MCP door acts for its owner, widely.** Through
  `POST /mirror/mcp` (`internal/api/mirror_mcp.go:134-140`) a mirror token can
  answer any of its owner's open asks, approve connector requests, read and
  approve plugin reply drafts, destroy agents, restart the chief, dispatch
  agents and ask the chief ([auth.md:195](../auth.md), [auth.md:488-504](../auth.md)).
  On a personal host that is the owner's own power. **On a shared host it would
  hand every member's session the team's whole power** — the single biggest
  constraint on this design.

### Notifications and live views

- Push goes to one owner: `push.Service.SendTo(ctx, owner, p)`
  (`internal/push/service.go:390`).
- Live page updates go to one owner's streams: `sse.Event.OwnerID`
  (`internal/sse/broadcaster.go:36-38`, delivery at `:175-182`).

### Officers

- An officer's store rows are its owner's: `officer_types`, `officer_defs`,
  `officer_items`, `officer_schedules`, each under `owner_isolation`
  (`internal/db/migrations/0118_officer_backend.sql:24-27`, `:157-172`). One
  user never sees another's types, officers or items
  ([officers-builtin.md:76-79](../officers-builtin.md)).
- Its `owner` in `MUSTER_OFFICERS` defaults to 1
  ([officers-builtin.md:118-121](../officers-builtin.md)).
- `--escalate-to 2,3` names **other users** an escalation reaches
  ([officers-builtin.md:69](../officers-builtin.md); `officer_defs.escalate_to
  BIGINT[]`, `0118_officer_backend.sql:66`). The admin route validates them
  (`internal/api/officer_admin.go:209-235`).
- **This is muster's one existing cross-user flow, and its shape matters.** The
  escalation does not share the item. A System sweep **copies** a notice into
  each recipient's own account: `noticesFor` lists owner plus `escalate_to`
  (`internal/officers/builtin/asks.go:286-305`), and `raiseOfficerNotice`
  creates an Attention ask **owned by the recipient** under
  `attentionPrincipal(ctx, recipient)` (`internal/api/officer_builtin.go:250-252`,
  `internal/api/attention.go:316`). The first answer re-opens the item and the
  other recipients' asks are withdrawn ([officers-builtin.md:185-193](../officers-builtin.md)).
- Outbound drafts (0121) are `plugin_reply_drafts` rows the **owner** approves
  on the Reply drafts tab or with `plugin_draft_decide`
  ([officers-builtin.md:195-219](../officers-builtin.md)).

### The chief: per user, or one per install?

Both statements are true, at different layers:

- **The data model is per user.** 0009 reserves one chief per owner: `chief`
  for user 1 and `chief-<id>` for anyone else, with a unique index
  (`internal/db/migrations/0009_per_user_scoping.sql:6-10`, `:37-39`;
  [chief.md:232](../chief.md)). On Kubernetes every user can provision theirs
  (`POST /chief/provision`).
- **The Docker Compose runtime runs the admin's only.** The Compose chief
  container serves user 1: `composeChiefOwner(owner) = owner ==
  authz.OperatorUserID` (`internal/api/compose_chief.go:317`), and the Account
  page's switch is withheld from everyone else (`internal/api/chief_toggle.go:167-168`).
  The chief's LLM proxy route is the admin's alone too, because the key is the
  admin's (`internal/api/chief_llm.go:139`).

So [features.md:361](../features.md) ("one chief per install: the admin's") is
correct **for the Compose stack**, which is the section it sits in, and
[auth.md:56](../auth.md) ("each user has their own … chief") is correct **for a
Kubernetes deployment**. auth.md should say so; that one-line docs fix is out of
this page's scope and is listed under [operator decisions](#operator-decisions-2026-11-13).

## Part 2: the design

### 1. The sharing model

#### Options considered

| | per-resource grants | a team that **owns** resources | an instance-wide "shared" flag |
|---|---|---|---|
| shape | `grants(resource, user, role)` for each shared session, officer, task | `owner_id = <team>`; `team_members(team, user, role)` | a boolean on each row; every user of the server sees it |
| RLS change | a grant lookup in ~67 tables' policies, each keyed differently (a session is not a row: it is a hub entry) | one policy arm, the same expression on every shareable table | one arm, but no roles and no subgroups |
| in-memory state (hub, SSE, push) | each needs a grant index | owner-keyed already; a team is just another owner | owner-keyed, plus "everyone" |
| who owns it after the creator leaves | the creator, so it leaves with them | the team | the creator |
| "private by default" | yes | yes (the default owner is the person) | yes |
| verdict | too many seams | **recommended** | too coarse: an admin's personal hosts and a member's are both "the server's" |

#### A team is a row in `users`

`users` gains `kind TEXT NOT NULL DEFAULT 'person' CHECK (kind IN ('person',
'team'))`. A team row:

- has no `gotrue_sub`, no `email`, role `member`, and is never the last admin
  (CHECKs: `kind = 'team'` ⇒ `gotrue_sub IS NULL AND email IS NULL AND role =
  'member'`);
- cannot sign in, receive an invite, hold a passkey, mint a personal token or
  pair a phone (each is keyed by a sign-in that a team row cannot have; the
  token, invite and device routes also refuse `kind = 'team'` by name);
- is excluded from `/admin/users` counts and lists, and from the last-admin
  rule (`internal/users/users.go`);
- can **own** anything a person can: mirror tokens, officers, tasks, agents,
  a chief.

Why a `users` row rather than a new `teams` table: `owner_id` is a foreign key
to `users(id)` on every owned table. A separate `teams` table would need every
one of them widened to "a user or a team", plus every composite foreign key.
A row of a new kind changes none of them.

```sql
CREATE TABLE public.team_members (
    team_id   BIGINT NOT NULL REFERENCES public.users (id),  -- kind = 'team' (trigger)
    user_id   BIGINT NOT NULL REFERENCES public.users (id),  -- kind = 'person' (trigger)
    role      TEXT   NOT NULL CHECK (role IN ('viewer', 'operator', 'manager')),
    notify    BOOLEAN NOT NULL DEFAULT true,   -- this member's push for the team's asks
    added_by  BIGINT NOT NULL REFERENCES public.users (id),
    added_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (team_id, user_id)
);
```

Removing a member deletes the row (an `audit_log` row keeps the history), so
"was a member" never reads as "is a member".

#### Roles

| capability | viewer | operator | manager |
|---|---|---|---|
| see the team's sessions in the sidebar, their state and focus | ✓ | ✓ | ✓ |
| watch a team session's Rendered view (transcript) and a read-only terminal | ✓ | ✓ | ✓ |
| see the team's asks and their answers; see team tasks | ✓ | ✓ | ✓ |
| type into a team session, queue a send, answer a session's question | | ✓ (+ step-up) | ✓ (+ step-up) |
| answer the team's Attention asks (officer asks, escalations) | | ✓ | ✓ |
| file and comment on team tasks; message the team chief | | ✓ | ✓ |
| read inbound plugin messages and reply drafts bound to the team | | ✓ | ✓ |
| approve an outbound draft or a reply draft | | | ✓ (+ step-up) |
| mint and revoke the team's host tokens and plugin tokens | | | ✓ (+ step-up) |
| edit an officer's charter, mode, schedules, `escalate_to` | | | ✓ |
| set the team's model credentials (write-only) | | | ✓ (+ step-up) |
| add, remove and re-role members | | | ✓ |

The **admin** role is not a team role: an admin creates and deletes teams and
names the first manager (`/admin/teams`), and reads a team's data only if a
member. That keeps today's rule that an admin's ordinary pages are scoped to
the admin's own rows (`internal/api/isolation_test.go:85-88`).

#### Who can share what

| thing | how it becomes shared | who |
|---|---|---|
| a host's sessions | the host connects with a **team** mirror token | a manager mints it |
| an officer | defined with `owner` = the team | a manager |
| a task | filed in the team, or **moved** from your own board | an operator files; the task's owner moves their own |
| an agent (incl. the team chief) | dispatched as the team | a manager (phase 4) |
| one personal session, read-only | a per-session view share (phase 5, optional) | its owner |

Nothing moves **out** of a team into a person's account: a member who leaves
leaves the team's things behind.

### 2. Row-level security

#### What the database knows

`internal/db/scope.go` sets one more setting on every acquire, in the same
round trip (`applyScopeSQL`, `:113`):

```text
muster.uid    the acting PERSON (unchanged)
muster.scope  'all' for System (unchanged)
muster.teams  '12:2,15:1'  = team 12 as operator (2), team 15 as viewer (1); '' = none
```

`authz.Principal` gains `Teams map[int64]TeamRole`, read in `principalFor`
(`internal/api/principal.go:135`) in the same query as the role, so one request
has one consistent view of its memberships. A principal whose `UserID` is a
team (a team mirror token, a team officer's instance, the team chief) carries
`muster.uid` = the team and no `muster.teams`.

> **As shipped in phase 1 (#654), the encoding differs from the sketch above.**
> `authz.Principal.Teams` is `authz.Memberships`, a canonical **string** of
> `'<team>:<role>'` pairs sorted by team id, with the role spelled out:
> `'12:operator,15:viewer'`, not `map[int64]TeamRole` and not `'12:2'`. The
> reason is that `Principal` must stay comparable with `==`, which a map
> would break, and many tests and call sites compare principals by value.
> Phase 2's `muster.teams` setting carries that same string, so
> the function that reads it must parse the role **name** (for example, by
> mapping `viewer`/`operator`/`manager` to 1/2/3), never cast it `::int`. The
> function below is the phase-2 sketch. As built in 2a it is
> `muster_team_ids(r)`, a list of team ids; see
> [phase 2a](#phase-2a-the-access-core-built-second).
> `team_members.notify` (§1) was **not** created in phase 1. It arrives with
> phase 2's push fan-out.

A STABLE SQL function reads the setting, so a policy never queries
`team_members` per row:

```sql
CREATE FUNCTION public.muster_team_role(o bigint) RETURNS int
LANGUAGE sql STABLE SET search_path = pg_catalog AS $$
  SELECT coalesce(max(split_part(x, ':', 2)::int), 0)
  FROM unnest(string_to_array(nullif(current_setting('muster.teams', true), ''), ',')) x
  WHERE split_part(x, ':', 1)::bigint = o
$$;
```

🔴 **The setting is computed by the application, as `muster.uid` is today.**
Row-level security here defends against an application query that forgot its
owner, not against an application that sets a false membership: that is the
same trust it places in `muster.uid` today. A seam test pins that the setting a
principal produces equals `team_members` for that user, and fails when either
side changes. An alternative is a SECURITY DEFINER function that computes the
memberships from `team_members` on every acquire; it costs a query per acquire
and is [decision 3](#operator-decisions-2026-11-13).

#### The policy

Each shareable table gets `owner_or_team` instead of `owner_isolation`, with
two thresholds stated per table:

```sql
CREATE POLICY owner_or_team ON public.<t> AS PERMISSIVE FOR ALL TO PUBLIC
  USING (
      owner_id = nullif(current_setting('muster.uid', true), '')::bigint
      OR public.muster_team_role(owner_id) >= <read_role>
      OR current_setting('muster.scope', true) = 'all')
  WITH CHECK (
      owner_id = nullif(current_setting('muster.uid', true), '')::bigint
      OR public.muster_team_role(owner_id) >= <write_role>
      OR current_setting('muster.scope', true) = 'all');
```

`owner_id`'s default stays `muster.uid`, the person. **An insert lands private
unless the code names the team**, and naming the team succeeds only at the
write threshold. A viewer's write to a team row is refused by Postgres
(`42501`), not only by a handler.

#### Three ledgers instead of one

`TestRLSCoversExactlyTheOwnedTables` (`internal/db/migration_0007_rls_test.go:190`)
becomes three pinned sets that together equal every owned table, each failing
when it grows **or** shrinks:

| ledger | policy | tables (the phase that moves each) |
|---|---|---|
| `personalTables` | `owner_isolation`, unchanged | `user_credentials`, `connectors`, `connector_grants`, `connector_standing_grants`, `connector_audit`, `devices`, `device_pairing_codes`, `device_challenges`, `push_subscriptions`, `push_preferences`, `notify_preferences`, `suggest_preferences`, `suggest_usage`, `chief_preferences`, `pins`, `recents`, `session_tags`, `flow_*`, `cc_account_*`, `pages`, `page_*`, `jobs`, `host_dispatches`, … (everything not below) |
| `sharedTables` | `owner_or_team`, read ≥ viewer, write ≥ operator | phase 2: `send_queue`, `attention_asks`, `asks`, `ask_attempts`, `agent_focus`, `agent_focus_history`, `session_episodes`, `session_prs`, `uploads`; phase 3: `officer_items`, `officer_llm_usage`, `officer_judgement_sessions`, `officer_pr_loops`; phase 4: `notes`, `note_comments`, `note_attachments`, `task_sessions`, `task_ref_writes`, `agents`, `chat_sessions`, `chat_messages`, `session_routes`, `chief_nudges`, `chief_proposals`, `chief_turn_usage`, `routing_rules` |
| `managedTables` | `owner_or_team`, read ≥ operator (viewer for the first two), write ≥ manager | phase 2: `mirror_tokens`; phase 3: `officer_types`, `officer_defs`, `officer_schedules`, `plugins`, `plugin_messages`, `plugin_reply_drafts`, `plugin_threads`, `plugin_handoffs`, `plugin_pair_requests`, `plugin_message_handling` |

> **As built in phase 2a (0126):**
> - Every share-class table got its team policies at once, not table by table
>   in each phase. Nothing writes a team row until its phase does.
> - `agents` stays **personal until phase 4** (its secrets split first), and
>   `plugin_pair_requests` stays personal: an anonymous pairing names a
>   person.
> - Managed tables are read at viewer for `mirror_tokens` and the officer
>   types, defs and schedules, and at operator for the plugin tables.
> - The policy is four per-command policies, not one `owner_or_team` (see
>   [phase 2a](#phase-2a-the-access-core-built-second)).
> - The test ledger is `shareClasses` in `internal/db/share_ledger_test.go`.

Credentials never enter a shared ledger. A team's model key is stored as a
`user_credentials` row with `owner_id` = the team, written by one
manager-gated handler under System (added to the System ledger in
`internal/authz/ledger_test.go`) and read only by the System paths that use it
today. No member's principal can SELECT it, the manager who wrote it included.

`db.RLSOn` (`internal/db/rollback.go:288`) re-applies each migration body in
order, so each phase's migration must be idempotent and must re-create
`owner_or_team` after the earlier migration that created `owner_isolation` on
the same table; the existing re-apply tests cover that once the ledgers move.

#### Seeing teammates

`users` is `self_read` (`0009_per_user_scoping.sql:50-55`), so a member cannot
read a teammate's display name. A SECURITY DEFINER function
`muster_team_people(team bigint)` returns `(id, display_name)` for the members
of a team the caller is in, and nothing else; no email, no role, no
`disabled_at`. "Queued by ben" and the member list use it.

#### Attribution columns

| table | column | meaning |
|---|---|---|
| `audit_log` | `owner_id BIGINT NULL` | the owner of what was acted on, when it is not the actor; `self_read` gains an arm: managers of `owner_id` read it |
| `send_queue` | `queued_by BIGINT NOT NULL DEFAULT muster.uid` | the person who queued it; delivery checks **their** sign-in and membership |
| `attention_asks` | `answered_by_user BIGINT` | the person who answered (`answered_by` stays `operator`/`chief`, `0025_attention_asks.sql:46`) |
| `note_comments` | `author_id BIGINT` | the person (`author` stays the free-text label, `0001_init.sql:81-83`) |
| `plugin_reply_drafts` | `decided_by_user BIGINT`, `requested_by_user BIGINT` | who approved; who caused the draft, if a person did |
| `chat_messages` | `typed_by` (exists, 0081) | already the precedent |

Each defaults to `muster.uid`, so a write records its actor without every call
site having to remember.

### 3. Shared sessions

#### A team host, not a per-session grant

A host's sessions are shared by **connecting the host with a team mirror
token**: `muster-server mirror token create build-01 --team 12`, or a manager on
`/teams/12/hosts`. `mirror_tokens.owner_id` = 12. The hub then keys the host as
`(12, build-01)` (`internal/mirror/hub.go:68`) with no change: to the hub a
team is an owner.

Why not per-session grants on a personal host:

- An interactive session **is a shell on that host**
  ([sending-and-step-up.md](../sending-and-step-up.md);
  [multi-tenant.md](multi-tenant.md#which-tenant-gets-a-hosts-sessions)). A
  grant to type into one of `ana`'s sessions gives `ben` her Unix user, her
  files, her other tenants' tokens and her other sessions (`tmux send-keys`).
  The boundary a grant draws on the listing does not exist on the host.
- The routing unit an interactive attach already respects is the **tmux
  server**, not the session (`cmd/muster-mirror/main.go:207`: an
  interactive client "can switch to any session or open a new window"). A team host is a dedicated tmux server, and
  ideally a dedicated Unix user, which multi-tenant.md already supports
  (`muster-mirror@<tenant>`).

So the recommended setup is: a machine, or a Unix user on one, that the team
treats as shared; its own `muster-mirror` with the team's token. A person who
wants their own and the team's sessions on one laptop runs two mirror daemons
(multi-tenant.md's one process per tenant) against the **same** server, one
with a personal token and one with the team token, on two tmux servers.

#### The chokepoint

`owner(ctx)` in `internal/mirror/service.go:347` becomes
`access(ctx, owner, need)`, returning the owner if the principal is that owner
or holds `need` in it:

| Service method | need |
|---|---|
| `Hosts` | lists the principal's own hosts **plus** every team's where it is ≥ viewer, each tagged with its owner |
| `WatchWindow`, `CatalogWindow`, read-only `AttachWindow` | viewer |
| `ScrollWindow` | viewer (scrolling moves copy-mode for everyone watching; see [decision 7](#operator-decisions-2026-11-13)) |
| interactive `AttachWindow`, `PasteWindow`, `AnswerWindow`, `AnswerOpenCodeAsk`, `AnswerOpenCodeForm` | operator, **and** step-up, checked here as today (`:383`) |

Every page handler that takes `?host=&session=` gains an `owner` parameter (or
derives it from the host listing). A host name is unique per owner, not per
server, so `build-01` may exist for `ana` and for team 12 at once.

#### Typing, step-up and the send queue

- **Step-up is unchanged and already per person.** It is read from the
  browser session presenting the request
  ([sending-and-step-up.md:534-537](../sending-and-step-up.md)), so `ben`'s
  send into a team session needs `ben`'s fresh sign-in.
- A queued send is a `send_queue` row with `owner_id` = the team and
  `queued_by` = `ben`. **At delivery** the dispatcher checks, in the same
  conditional claim: `ben` is still a member with role ≥ operator, `ben` is
  not disabled, and `ben`'s `sessions_not_before` / `signed_out_at` are before
  the row's `stepup_at`. Today these checks read the row's `owner_id`; they move
  to `queued_by` (`internal/sendqueue`, `internal/api/send_queue.go`). A row
  that fails is `cancelled` with a reason naming the check.
- `CancelOwner` (`internal/sendqueue/store.go:652`) gains a sibling
  `CancelQueuedBy(user)`, called on disable, sign-out everywhere **and** on
  leaving or being removed from a team (only that team's rows).
- **One queue per window, shared.** Two operators queue into one window; the
  queue already serializes by window
  (`0019_send_queue.sql:117`). The window's composer shows "queued by ben" on
  rows that are not yours; you can cancel your own; a manager can cancel any.

#### Asks, Attention and push: to whom

- **A team session's question is the team's.** `/mirror/ask` takes the owner
  from the token (`internal/api/mirror_ask.go:179`), so a question from a team
  host is already opened under owner 12 with no change. Every member ≥ viewer
  sees it; operators can answer.
- **Push fans out.** `SendTo(ctx, owner, p)` (`internal/push/service.go:390`)
  learns one rule: an owner of kind `team` expands to its members with role ≥
  operator and `notify = true`. The payload's title carries the team's name
  ("platform · build-01 · tests are red"), so a phone with no app change still
  tells team pushes from personal ones.
- **First answer wins.** An ask has one row and its moves are conditional
  updates (`0025`), so two operators answering at once produce one answer and
  one "already answered". The withdraw push that clears a notification today
  goes to every member it was sent to.
- **Live views.** A `/events` stream subscribes to its person **and** to each
  team the person is a member of, computed when the stream opens and
  recomputed on a membership change (`internal/sse/broadcaster.go`;
  `Event.OwnerID` stays one owner).
- **The Android device route** answering an ask
  (`internal/api/device_actions.go:51`) admits a team ask when the device's
  person is ≥ operator in that team. The signed challenge already binds the
  ask id and the exact text.

#### The team host's MCP door

🔴 A team mirror token acts **as the team**. Every tool its MCP door offers
today would let any session on a team host act as the team: answer any team
ask (including as dispatch instructions), approve any team draft to an outside
party, destroy team agents, dispatch agents on the team's accounts, ask the
team chief about every team session ([auth.md:195](../auth.md),
[auth.md:488-504](../auth.md)). On a personal host those are accepted risks
because the session and the owner are one person. On a team host they are not.

**Recommendation:** a team token's MCP door offers only the tools that act on
**the calling session's own** objects: `attention_ask`, `attention_wait`,
`attention_cancel` for its own asks, the task tools for tasks it created, and
`route_reply` for a route to itself. It refuses `attention_answer`,
`connector_request_decide`, `plugin_drafts_list`, `plugin_draft_decide`,
`agent_destroy`, `agents_list`, `chief_restart`, `dispatch_request`,
`dispatch_approve`, `plugin_messages_list`, `plugin_message_route` and
`chief_ask`, by name, in the tool listing **and** at the call
(`internal/api/mirror_mcp.go`; a test pins the two lists). Each can be
re-admitted later by its own decision.

#### What a viewer sees

| surface | viewer | notes |
|---|---|---|
| Rendered view (the Claude Code transcript) | yes | everything Claude printed, including any secret a command echoed |
| Terminal, read-only | yes | the live pane, including scrollback |
| attachments sent into the session (`uploads`) | yes | they are part of the transcript |
| focus title and description | yes | |
| subagents, background shells, preview button | yes; opening a preview is viewer | a preview is a web server on the host (see [previews.md](../previews.md)); [decision 8](#operator-decisions-2026-11-13) |
| the composer, quick keys, live keys | no | the box says "viewer: you can watch this session" |
| drafts on this device | — | drafts stay per browser, as today |

#### Audit

Each send, attach with keys, answer, cancel and token mint on a team object
writes an `audit_log` row with `user_id` = the person and `owner_id` = the
team. Managers read the team's rows on `/teams/12/audit`.

### 4. Shared officers

- **A team officer** is a builtin officer whose `owner` in `MUSTER_OFFICERS`
  is the team id. Its store rows (`officer_types`, `officer_defs`,
  `officer_items`, `officer_schedules`) are owned by the team. Its instances
  act as the team. Nothing in `internal/officers/builtin` needs to know: every
  statement already names the owner.
- **Charter edits** are manager-only: `officer_types` and `officer_defs` are
  in `managedTables`, so a non-manager's `PUT /api/officers/types/{key}` for a
  team type fails in Postgres as well as at the handler. The admin routes
  today act on `adminOwner(r)` (`internal/api/officer_admin.go:235`) behind the
  hook tier ([officers-builtin.md:241](../officers-builtin.md)); they gain an
  `owner` parameter that a personal token may set to a team it manages. A
  charter edit already bumps `charter_version`; its audit row names the
  manager.
- **Who answers its asks.** `officer_ask_person{to: "owner"}` raises an
  Attention ask owned by the officer's owner. For a team officer that is one
  team-owned ask: every member sees it, an operator answers it, and the answer
  records `answered_by_user`.
- **Escalation groups are teams.** `escalate_to` holds user ids, and a team is
  a user id, so `--escalate-to 12` already passes validation
  (`internal/api/officer_admin.go:228-235`). `raiseOfficerNotice` creates the
  notice under `attentionPrincipal(ctx, 12)` (`internal/api/officer_builtin.go:250-252`):
  **one** ask owned by the team, shown to all of its members, answered once.
  Naming individual users keeps today's per-person copies. The only change:
  `attentionPrincipal` must build a principal for a team row (today
  `principalFor` reads a person's role), and a test pins that a team-owned
  escalation is visible to members and not to non-members.
- **Outbound drafts.** An officer draft (`plugin_reply_drafts` with
  `plugin_id` set, 0121) bound to a team plugin is approved by a **manager**,
  with a **fresh sign-in** (today approval needs none). Approval binds the body
  and media hashes as today.
- **Can one member approve another's draft?** Yes, that is the point of a
  team, with one exception: a reply draft a **person caused** (their session's
  `route_reply`, recorded in `requested_by_user`) cannot be approved by that
  same person. Officer drafts have no person behind them, so any manager may
  approve one. Over MCP, a team token cannot approve at all (above).
- **Who is notified of a draft.** Managers with `notify = true`.

### 5. Tasks and the chief

- **Team tasks** are `notes` rows owned by the team. Child rows (`note_comments`,
  `note_attachments`, `task_sessions`) must share the parent's owner (the
  composite keys), so a comment by `ben` on a team task has `owner_id` = 12 and
  `author_id` = 8.
- **Moving a task to a team** is one transaction under System, from a handler
  that checks the caller owns the task and is ≥ operator in the team: it
  rewrites `owner_id` on the task and every child row (the composite keys make
  a partial move impossible). It is listed in the System ledger. A task linked
  to a personal session route (`session_routes`, `attention_asks.task_id`)
  keeps those links only if their owner is the same; otherwise the move is
  refused, naming them.
- **A team chief** is the existing per-owner chief for owner 12: the reserved
  slug `chief-12` (`0009_per_user_scoping.sql:6-10`). It is provisioned by a
  manager, runs on the team's model credentials, and its fleet tools see the
  team's hosts, tasks and asks, because those are what its owner owns.
- **What a team chief may route:** team tasks and team plugin messages, to team
  sessions. Never to a person's session. Today's same-owner rules already
  enforce it: a route's task must be its owner's (0053's trigger, as 0027's).
- **A personal chief never sees team sessions.** `sessions_overview` and
  `focus_search` read the owner's hosts. The alternative (a personal chief that
  also reads your teams) would put team transcripts into a turn running on
  your personal credentials; [decision 10](#operator-decisions-2026-11-13).
- **Who may message the team chief:** operators. Each chat turn already
  records `typed_by` (0081).
- **Compose:** the Compose chief serves user 1 only
  (`internal/api/compose_chief.go:317`). A team chief needs Kubernetes, or a
  change to that rule ([decision 9](#operator-decisions-2026-11-13)).

### 6. UI surfaces

- **Sidebar and the session switcher:** your hosts, then one group per team,
  headed by the team's name, each with its own "waiting on you" counts. A
  team row shows a small team mark; a session someone else is typing into
  shows who ("ben is typing").
- **No acting-as switch.** A creation form (new task, new officer, mint token)
  has an **Owner** select, defaulting to you. Everything else acts on the
  owner of what you clicked.
- **Shared-state indicators:** a team badge on every team object; "queued by",
  "answered by", "approved by" on rows that are not yours; on a team session's
  page, the members watching right now (viewers included), since anyone typing
  is typing in front of them.
- **Attention:** your asks and each team's asks, grouped; a team ask says
  which members it was sent to and, once answered, who answered.
- **Team pages:** `/teams/12` (members and roles, manager-editable),
  `/teams/12/hosts` (tokens), `/teams/12/audit`. Admin: `/admin/teams`
  (create, delete, first manager).
- **The web server switcher** (being built separately) switches between
  **servers**; team grouping lives **inside** a server's pages. They compose:
  switch to the team server, then see your and the team's sessions there.
- **Android:** the server switcher is multi-tenant.md's phase 3, still
  proposed. Within one server the team grouping is web content in the WebView,
  so the app needs no change for phases 1-4; team pushes are labelled by the
  server ("platform · …"). A later app change could add a per-team
  notification channel within a tenant's channel group.

## Security and threat model

| threat | what this design does |
|---|---|
| 🔴 **An operator typing into a team session runs commands as that host's Unix user.** They can read every file it can, its credentials (cloud, git, the mirror token itself), and `tmux send-keys` into every session on that tmux server. | Operator on a team host = shell on that host, stated on the role picker and on `/teams/12/hosts`. Only team tokens share sessions; no per-session typing grant on a personal host exists. INSTALL.md's team section recommends a dedicated Unix user per team host. |
| A viewer sees a secret a command printed | Viewer = reads everything on screen and in the transcript, stated on the role picker. No redaction promise. |
| The team host's MCP door acts as the team | A reduced tool set for team tokens (above). |
| A leaked team mirror token | Same as today's leaked token, scoped to the team: revoke on `/teams/12/hosts`. Revocation already cuts the tunnel (`watchToken`, `internal/mirror/service.go:239`). |
| **A removed member keeps a copy of the team host token** (they had a shell there) | Removing an operator or manager marks every team host token **rotate**, shown to managers until rotated. Not auto-revoked: that would cut every team session at once ([decision 6](#operator-decisions-2026-11-13)). |
| Removal, leaving, role downgrade | The membership row goes in one statement. Next request: no access. Open terminal and Rendered streams re-check membership on the same timer that already cuts a disabled user's streams within 10 s ([auth.md:51-53](../auth.md)). Their queued sends to that team are cancelled. Pushes already on their phone stay; the withdraw push clears them. |
| A disabled user | Their principal fails, as today; `CancelQueuedBy` cancels their sends on every team; their memberships stay (re-enabling restores them) but grant nothing while disabled. |
| A deleted user | The team's rows are the team's, so nothing they shared is lost. Their `queued_by`/`answered_by_user` values stay as ids; the name function returns nothing for them ("a former member"). |
| A team row signing in | No `gotrue_sub`, no email, refused by every credential-minting route by kind, pinned by a test per route class. |
| A member reading a teammate's private rows | Untouched: personal tables keep `owner_isolation`, and the isolation walk (below) proves it. |
| A manager reading the team's model key | No member principal can SELECT `user_credentials`; it is written under System by one handler and read only by the paths that already decrypt it. |
| An approver approving their own reply | Refused when `requested_by_user` = the approver. |
| A stale `muster.teams` on a pooled connection | It is set on every acquire and cleared on release, with `muster.uid` (`internal/db/scope.go:26-32`). |

### The proof: the isolation walk grows

`internal/api/isolation_test.go` today proves B cannot reach A. It gains a team
T owned by nobody but its members, with A a manager, C an operator, V a viewer
and B outside it, a canary seeded into every T object, and:

- **B** (non-member) walks every route against T's ids: no canary, no 2xx, no
  SSE event, no row changed. The same negative control as today: A re-sends
  each request and is not refused.
- **V** sees T's canary on every list route with a team group, and every write
  route against T is refused; a mutation leg proves no T row changed.
- **C** can do the operator routes and is refused the manager routes.
- **C and V never see A's personal canary**, on any route, which is the
  "private by default" claim made mechanical.
- Every route gains a team class in the classification ledger
  (`internal/api/isolation_test.go:100-135`), so a new route cannot ship
  without saying what a team member may do with it.

## Migration and rollout

Each phase is one migration (numbers are placeholders after `0124`) and one PR
series, ships alone, and rolls back alone.

### Phase 1: teams, no shared data

- **Migration `teams`:** `users.kind` and its CHECKs; `team_members` under
  forced row-level security (a person reads their own membership rows; every
  write goes through the admin handlers under System, which the System ledger
  lists).
- **Code:** `authz.Principal.Teams`, read once per request by `principalFor`
  (`internal/api/principal.go:135`), with a seam test pinning it against
  `team_members`; a team row is refused at every sign-in, session, token,
  invite and device path, with a ledger test over every auth entry point that
  fails when a new one appears without the check; `internal/users` excludes
  team rows from `/admin/users` and the last-admin rule; `/admin/teams`
  (admin-only in this phase: create a team, add, remove and re-role members),
  each change an `audit_log` row naming the acting admin.
- **No table changes policy, and nothing is shared.** A test over every
  `owner_id` table proves a member cannot read a row the team owns.
- **Moved to phase 2a, where the first policy reads them:** the `muster.teams`
  setting and its function, the audit column with its manager read arm, and
  the managers' own `/teams/{id}` pages.
- **Acceptance:** a team row cannot sign in by any entry point; a membership
  change is visible to the next request.
- **Rollback:** an older image ignores the column and the table. To remove
  them: delete the team rows, drop `team_members` and `users.kind`.

### Phase 2a: the access core (built second)

The operator split phase 2 after phase 1: **2a is the access core only**, and
team hosts move last, as 2b. 2a creates **no team-owned row**; it lands the
rule the later phases' rows will meet.

- **Migration `team_access`** (0126):
  - `muster_team_ids(r)` (the ids of the teams held at role ≥ r) parses
    `muster.teams` by role NAME. §2's `muster_team_role()` was not kept: no
    policy or code path needed a per-owner role once the policies read a list.
  - The policies use `owner_id = ANY (coalesce((SELECT muster_team_ids(r)), '{}'))`.
    Postgres plans it as an InitPlan, so the setting is parsed once per query.
    Measured on 400k rows: ~22 ms, against ~26 ms under `owner_isolation`
    alone and ~200 ms with a per-row role function.
  - `muster_team_people()` is **deferred** to the phase that shows a member
    their teammates' names. The manager page reads its members after its own
    manager check, as the admin pages do.
  - **Four policies per share-class table, one per command**
    (`team_read` / `team_insert` / `team_update` / `team_delete`). §2's
    single `owner_or_team FOR ALL` would have let a VIEWER delete: a FOR ALL
    policy's USING is DELETE's too.
  - A trigger keeps `owner_id` fixed outside the system scope, so no member
    moves a row into or out of a team.
- **Delta from §2's sketch:**
  - The audit column is `audit_log.team_id`, not `owner_id`. An `owner_id`
    column marks an OWNED table everywhere (`RLSStatus`, the ledgers), and
    `audit_log` is user-keyed.
  - `answered_by_user` and `decided_by_user` are stamped by triggers when an
    ask or a draft changes state, so no code path can forget them.
  - `queued_by` and `author_id` default to the acting person.
- **Which doors carry the teams:** only doors where a PERSON is present:
  `ViaSession` and `ViaDevice` (`authz.Principal.TeamScope`, a closed set;
  `TestEveryDoorSaysWhetherItCarriesTeams` fails on a new `Via`). A personal
  API token (`ViaToken`, `mst_…`) carries **no** team access, by the
  operator's decision: scripts and agents hold such tokens as often as people
  do. A team-scoped token may come later.
  Every agent door (a member's PERSONAL chief included, decision 10), the
  mirror host, connect, relay and every background loop act with none, even
  though the principal carries the memberships. **Phase 4 extends
  `authz.WithPrincipal` to admit a team principal only for a closed set of
  `Via` values. Phase 3 adds `ViaOfficer` and phase 2b adds `ViaMirrorHost`
  to the team-carrying set.**
- **`agents` is not shared until phase 4.** Its rows carry an agent's live
  secrets (`hooks_token`, `gateway_secret`). Phase 4 first moves them into an
  owner-isolated table, then shares `agents`. Credentials, connectors,
  devices and push are never shared.
- **`/teams/{id}`:** the manager's page.
  - It lists the members and their roles, adds a person by email, re-roles
    and removes members, and shows the team's audit rows.
  - Only the team's manager reaches it. Everyone else gets the 404 a missing
    team gets.
  - Writes are same-origin, with no step-up: this is account management.
  - The last manager cannot leave.
- **Ledgers:**
  - `TestEveryOwnedTableHasAShareClass` pins every `owner_id` table to its
    class and roles, and fails on any change or on an unclassified new table.
  - `TestTheShareLedgerHoldsForEveryRole` runs every role on every table,
    plus every non-person door.
  - The isolation walk now walks B as a non-member, viewer, operator and
    manager of a team.
- **Rollback:** the header's block calls `muster_team_access_rollback()`,
  which puts `owner_isolation` back everywhere and drops the functions and
  columns.

### Phase 2b: team hosts (built last)

- **Migration:** a team mirror token is admitted by the mirror token lookup
  (today it refuses a team owner), and `ViaMirrorHost` joins the
  team-carrying doors.
- **Code:** `internal/mirror/service.go` `owner()` → `access()`;
  `Hosts` lists teams; every mirror page handler takes the owner
  (`internal/api/mirror.go`, `internal/api/mirror_view.go`); the send queue's
  delivery checks `queued_by` and `CancelQueuedBy`; `push.SendTo` team fan-out;
  SSE team subscriptions; `muster-server mirror token create --team`;
  `/teams/{id}/hosts`; the reduced MCP tool set for team tokens
  (`internal/api/mirror_mcp.go`); the device answer route's team check
  (`internal/api/device_actions.go`); the sidebar groups.
- **Acceptance:** the extended isolation walk; two operators answering one ask
  yield one answer; a removed operator's queued send is cancelled and its open
  terminal closes within 10 s; a team token's MCP listing omits the refused
  tools **and** a direct call to each is refused.
- **Rollback:** refuse team mirror tokens again. Team rows stay intact, and a
  team host's sessions are shown to nobody.

### Phase 3: team officers

- **Migration:** the phase-3 tables into `sharedTables` / `managedTables`;
  `plugin_reply_drafts.decided_by_user`, `requested_by_user`.
- **Code:** officer admin routes take `owner` for a managed team
  (`internal/api/officer_admin.go`); `attentionPrincipal` for a team owner
  (`internal/api/attention.go:316`); draft approval: manager, step-up, not the
  requester (`plugin_draft_decide` and the Reply drafts tab); `MUSTER_OFFICERS`
  accepts a team id as `owner` (boot validation in `internal/officers`).
- **Acceptance:** a team officer's ask is one ask seen by members and answered
  once; `--escalate-to <team>` raises one team ask; an operator cannot edit
  the charter (handler **and** Postgres); a member cannot approve a draft they
  requested.
- **Rollback:** as phase 2; the officer keeps running under its owner and its
  asks become invisible to people until roll-forward.

### Phase 4: team tasks and a team chief

- **Migration:** the phase-4 tables into `sharedTables`;
  `note_comments.author_id`; the team-credential path.
- **Code:** the task board's owner groups and Owner select; the move-to-team
  handler (System ledger); `POST /chief/provision` for a team by a manager;
  team model credentials (write-only); the team chief's chat page.
- **Team writes are phase 4's.** After 2a, existing routes do not write team
  rows yet: a member commenting on a team task fails closed on a database
  constraint (the comment's owner must be its task's). Phase 4 owns those
  writes.
- **Acceptance:** a team chief routes a team task to a team session and is
  refused a person's session; a personal chief's `sessions_overview` contains
  no team host; a moved task's children all carry the team owner.

### Phase 5 (optional): a read-only share of one personal session

A per-session **view** grant (`session_shares(owner, host, session, user,
expires_at)`), viewer only, never typing, time-limited, revocable. Only if
phase 2's "a team host" turns out to be too heavy for "show my teammate this
one run". See [decision 4](#operator-decisions-2026-11-13).

## Operator decisions (2026-11-13)

The operator accepted every recommendation this page made, and phase 1 is
the next step. Each question below is decided as stated.

| # | question | decision |
|---|---|---|
| 1 | Team as a `users` row of kind `team`, or a new `teams` table? | **A `users` row.** Every `owner_id` foreign key, composite key and owner-keyed map works unchanged. The cost is excluding team rows from the handful of user listings and sign-in lookups, each pinned by a test. |
| 2 | Several teams per server, or one implicit team per server? | **Several in the schema, one in the UI until a second exists.** A team server with one team then looks like "the instance is the team", and a later second team needs no migration. |
| 3 | Compute `muster.teams` in Go per request, or in a SECURITY DEFINER function per acquire? | **Go, per request**, with a seam test pinning it against `team_members`. It is the trust model `muster.uid` already has, and a per-acquire query multiplies across every pool operation. |
| 4 | Allow per-session grants on a personal host? | **No typing grants, ever.** A read-only, expiring share (phase 5) only if asked for after phase 2 ships. |
| 5 | Which MCP tools does a team host token keep? | **Only those acting on the calling session's own objects** (listed above); every other tool refused by name until its own decision. |
| 6 | Revoke a team's host tokens automatically when an operator or manager leaves? | **No; mark them "rotate" and tell the managers.** Auto-revoke cuts every team session. |
| 7 | May a viewer scroll a team session (it moves copy-mode for every watcher)? | **Yes**, as today scrolling needs no step-up; revisit if it disturbs operators. |
| 8 | May a viewer open a team session's preview (a web server on the host)? | **Yes** for a preview already running; starting one stays operator. |
| 9 | A team chief on the Compose stack? | **Kubernetes only in phase 4.** The Compose container serves one chief by design; a team server on Compose can run team officers on hosts once #649 (officers as host-spawned sessions) lands. |
| 10 | May a personal chief read your teams' sessions? | **No.** A team's transcripts should reach only the team's chief, on the team's credentials. |
| 11 | Step-up for answering a team ask? | **No**, as today, **except** an answer that becomes `agent_dispatch` instructions on a team agent, which needs it. |
| 12 | Push for team asks: every operator, or an on-call rota? | **Every operator and manager with `notify` on**, first answer wins, the rest withdrawn (the escalation precedent). A rota is a later feature. |
| 13 | Who creates teams: admins only, or any user? | **Admins**, naming the first manager. A team grants access to machines, so creating one is an instance decision. |
| 14 | Fix auth.md:56 to say the per-user chief is Kubernetes-only? | **Yes**, as a one-line docs PR, independent of this design. |
