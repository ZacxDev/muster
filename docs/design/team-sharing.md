# Design: team sharing on one muster server

**Status: decided; phases 1, 2a, 3 and 4.1–4.4 are built (see each phase's "as built" note), and
phase 2b is [designed and decided](#phase-2b-design-team-hosts-decided-2026-11-14), with its slice 2b.1 (the fan-out, 0133) built
(build order 1 → 2a → 3 & 4 → 2b).** The
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

> **As built in phase 2a (0127):**
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

- **Migration `team_access`** (0127):
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
- **Usage tables are their own class** (`officer_llm_usage`,
  `officer_judgement_sessions`, `chief_turn_usage`): members read them, but
  no member writes them. Only the owner's own recording path or the system
  scope does, because an operator deleting the day's row would reset a
  manager's budget.
- **The attribution columns cannot be forged** (`muster_actor_column`):
  - The application role's insert is forced to the acting person, or to
    nobody for an ask or a draft not born answered or decided.
  - An update that rewrites the column without the stamping state change is
    refused.
  - The login role (a restore) is exempt, as it is from row-level security.
- **The personal ledger is explicit** (`personalOwned`): a new `owner_id`
  table fails the share ledger until someone classifies it. It is never
  personal by default.
- **The manager page adds no one without consent.**
  - The member-row form only re-roles an EXISTING member. A non-member and
    an id nobody has get the same refusal.
  - Adding is by email only.
  - What it claims, and only this: the POST's response does not reveal
    whether the email is registered. A disabled account gets the same
    response as an unregistered email (no row, no audit).
  - What it does NOT claim:
    - A successful add shows the person in the member list on the next page
      view.
    - The two paths are not constant-time: both are one transaction, but an
      add also writes a row and an audit entry.
    - The person is added **without their consent**.
  - **Consent-based team invites** (the person sees the invitation and
    accepts) are an open follow-up, listed under the later phases.
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
  `Via` values. Phase 3 adds `ViaOfficer`. Phase 2b adds `ViaMirrorHost`,
  `ViaAsk` and `ViaQueue`. All of them go to `teamActingVias`, the doors where
  something the team owns acts as the team, and not to `teamVias`, which
  carries a person's memberships** (see
  [the 2b design](#phase-2b-design-team-hosts-decided-2026-11-14), D).
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

> **The full design is [Phase 2b design: team hosts](#phase-2b-design-team-hosts-decided-2026-11-14)**
> (decided 2026-11-14), at the end of this page. The list below is the
> original scope; where they differ, the design section is the newer word.

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
- **Open streams keep the memberships they started with** (noted in 2a's
  round 1, no code there). Every request reads memberships afresh, but an
  SSE, terminal or chat stream opened before a removal or a downgrade keeps
  the principal it was admitted with. 2b, the first phase whose streams show
  team rows, must re-check membership on the timer that already ends a
  disabled user's streams.
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

> **As built in phase 3 (0130):**
> - **The door.** `authz.ViaOfficer` joined `teamActingVias`
>   (`TestTheTeamActingViasAreExactlyTheListedDoors`, now 9 doors). The
>   runtime of an officer whose `MUSTER_OFFICERS` owner is a team acts as the
>   team acting principal. That covers its wake loop, its builtin store, its
>   asks and its outbound drafts.
> - **Boot.** `officers.CheckOwners`, called from `api.EnableOfficers`,
>   refuses an owner that names nobody. It also refuses a team owning a
>   `"runtime": "host"` officer, because team hosts are 2b and the hub never
>   lists a team's host.
> - **The tables were already classed by 2a.** 0130 adds no share-class
>   change to them.
> - **Charter edits are a session page, not the JSON admin API.** This is a
>   delta from §4, which let "a personal token" set `owner` to a team it
>   manages. 2a's operator decision is that a personal API token carries no
>   team access, and `/api/officers/*` admits only a personal token or the
>   hook token. So those routes stay the caller's own and never name a team.
>   A manager edits a team's types and officers on `/teams/{id}/officers`,
>   signed in. Every member reads that page; only a manager writes. Postgres
>   refuses an operator too, because officer types and defs are managed.
> - **Escalation to a team.** One team-owned ask. A team id is admitted as
>   `escalate_to` only on that page, and only the page's team or a team the
>   manager operates. The JSON API's check (`UsersExist`) still refuses a
>   team.
> - **Drafts.**
>   - `plugin_reply_drafts.requested_by_user` is stamped by
>     `muster_actor_column`: the person, or the team for a team officer's
>     draft.
>   - `draft_approver` (a trigger, on a decision: an update) refuses a team
>     approving a draft it owns, and a person approving a team's draft they
>     caused. A draft FILED approved is a standing approval (a route's
>     allowlist) and is not refused (#668 round 0, D2).
>   - `decideDraftAs` refuses first (`draftDeciderRefusal`): the team
>     itself; a member below manager; and the requester, for an approval. A
>     door without the person's teams (token, agent, mirror host) is "not a
>     manager".
>   - The requester may still REJECT their own draft.
>   - Step-up is the Asks page's existing approval step-up, unchanged.
>   - `plugin_draft_decide` (the mirror door) carries no teams, so it never
>     reaches a team's draft. This holds until 2b's reduced team-token tool
>     set.
> - **Host spawns are lendable, personal by default** (operator decision; a
>   new share class, `lentOwned`).
>   - The owner lends one row to one team they operate, and unlends it.
>     There are two ways in: `/officers/spawns` (the session tier) and the
>     chief's `officer_spawn_share` (chief-only, and on MCP for a
>     claude-code chief).
>   - `lendHostSpawn` is the one gate. `spawnLendDoors` admits:
>     - the owner's browser session;
>     - the owner's own chief: its MCP door (`ViaAgent`), and a gateway
>       chief's machine-message (`ViaAgentMessage`) and kickoff
>       (`ViaKickoff`) turns.
>   - A personal API token never lends **directly**.
>   - 🔴 **This is an operator-approved exception to 2a's "a personal API
>     token carries no team access"** (#668, after round 1). A token, or the
>     hook token, that **messages the chief** can have it lend or unlend the
>     **owner's own** spawn rows to a team the owner operates.
>   - That is the whole exception. It reads nothing of the team's. It does
>     not reach team drafts (the draft guard reads `TeamScope`, which is
>     empty on every agent door) or team charters (session pages only).
>   - The role is read from `Principal.Teams`, not `TeamScope`, which is
>     empty on the chief's doors.
>   - Moving a lend from one team to another writes an unshare audit row for
>     the team it left.
>   - `lend_target` enforces it again in Postgres from `team_members`.
>   - `internal/users` ends a person's lends to a team when they leave it or
>     drop to viewer.
>   - A team-owned spawn (a team officer's) is not lendable. It is visible
>     only to the team acting principal until 2b.
> - **Audit rows** (#668 round 0, choice 12): sharing and unsharing a spawn
>   (`officer.spawn.share` / `.unshare`) and a team charter edit
>   (`officer.type.put`, `officer.def.put`) each write an `audit_log` row
>   naming the person and the team (`team_id`), read by its managers. The row
>   is written after the change, not in its transaction; a failed write is
>   logged.
> - **The minimal team fan-out** (#668 round 0, Q1, operator decision): a
>   team officer's asks, its escalations to the team and its outbound drafts
>   push to the team's current operators and managers, read from
>   `team_members` at send time (`users.PGStore.TeamNotifyMembers`, replaced
>   in 2b.1 by `TeamPushTargets` and goNotify's one fan-out). Viewers,
>   people who have left and disabled people get nothing; a personal item is
>   unchanged. Everything else is 2b's: other team-owned pushes, withdrawing
>   these from members' devices once answered or decided, the per-member
>   `notify` switch and team-labelled titles.

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
- **Shipped, slice 4.2 (agent secrets):** an agent's `hooks_token` and
  `gateway_secret` moved to `agent_secrets` (migration `0128`), owned and under
  `owner_isolation`. Ordinary reads of an agents row no longer carry them; the
  turn sender, the provisioner and the first-turn and delivery checks read them by
  agent id (`agents.Store.Secrets`), and `internal/agents/secrets_ledger_test.go`
  fails on any new query that names either column.
  - **What 4.2 guarantees:** the store's ordinary reads of an agent do not carry
    its credentials, and every query that does name them is in the ledger.
  - **What it does NOT yet guarantee:** the old `agents.hooks_token` and
    `agents.gateway_secret` columns are still dual-written for every owner (an
    older image reads them, so they stay for rollback). So the credentials are
    still in the `agents` row itself, and sharing `agents` today would let a
    member's `SELECT … FROM agents` read them.
  - 🔴 **So slice 4.4, which shares `agents`, must first** make team-owned agents
    never write those columns (or drop them), with a test that a member
    principal's `SELECT … FROM agents` on a team agent yields no secret.
  - ⚠ **Dropping the old columns must re-run the 0128 backfill first.** An
    agent minted by an older image after a rollback keeps its token only in the
    old column; `agents.Store.Secrets` finds it through the fallback, so
    `ensureHooksToken` sees a token and writes nothing to `agent_secrets`.
    Dropping the columns without the backfill would lose that token.
- **Shipped, slice 4.3 (team tasks):** no migration. Phase 2a's `0127`
  already shares `notes`, `note_comments`, `note_attachments`,
  `task_sessions` and `task_ref_writes` (read at viewer, written at
  operator) and adds `note_comments.author_id`, so slice 4.3 is code only.
  - **Filing:** the create form's Owner select lists the teams you are at
    least an operator in; `notes.Create` writes that owner explicitly, and
    the handler (403) and Postgres (42501) both refuse a viewer or an
    outsider. Comments take the task's owner and record `author_id`.
  - **Thresholds.** This table is the one statement of them; the code
    (`internal/api/team_tasks.go`) points here. A person's own task is
    theirs at every threshold. Only the browser session and a paired phone
    carry team memberships, so every other door meets no team task.

    | action on a team task | viewer | operator | manager | enforced by |
    |---|---|---|---|---|
    | read (page, board, attachments) | ✓ | ✓ | ✓ | RLS (0127) |
    | file in the team | | ✓ | ✓ | handler 403 + Postgres 42501 |
    | comment, change status, complete, edit, tag, retry/dismiss a ref write | | ✓ | ✓ | handler 403 (`requireTaskOperator`) + RLS |
    | retract a comment | | its author | ✓ | handler 403 (`softDeleteTaskComment`) + the store (`SoftDeleteComment`); RLS admits it at operator |
    | dismiss | | | ✓ | handler 403 only (`requireTaskManager`); RLS admits it at operator |
    | move one of **your own** tasks in | | ✓ | ✓ | handler only (the move is a System write) |

    A comment with no `author_id` (written under the system scope, or before
    0127) only a manager may retract. A merge needs both tasks to have one
    owner. The role gate answers 500, not a pass-through, when it cannot
    read the task; only "not found" passes through to the handler's own 404.
  - **Board:** grouped by owner (yours, then each team, by name) whenever a
    team task is on it; otherwise the flat list it always was.
  - **Move to team:** `POST /tasks/{id}/move`, by the task's owner into a team
    they operate in, one system-scope transaction (`notes.PGStore.MoveToTeam`,
    in the System ledger through `handleTaskMove`). It moves the task,
    its comments, attachments and session links. It is refused, naming the
    link, when an agent, an ask, a host dispatch, a plugin message, a session
    route or a queued plugin write points at the task, and for a plugin task
    reference. `task_ref_writes` is a refusal, not a moved child: each row
    has a composite key onto its personal plugin. Every foreign key onto
    `notes` is classified (`TestEveryForeignKeyOntoNotesIsClassified`).
  - **Known limitations:** a resolved ask, a finished session route or a
    completed host dispatch still blocks a move, exactly as a live one does;
    a link row inserted while a move is in flight (the racing-insert window)
    is not closed by this slice; and a comment on a team task notifies no
    other member. **(c) is closed by 2b.1:** a member's comment on a team
    task pushes the team's other operators and managers (see "As built in
    2b.1" under F).
  - **Not yet:** dispatching an agent on a team task. Agents are not shared
    until 4.4. A person's agent naming a team task would meet the
    `agents (note_id, owner_id)` key; this slice does not test that path.
- **Shipped, slice 4.4 (team agents visible):** migration `0131`
  (`0130` is phase 3's).
  - **`agents` is shared** (read at viewer, written at operator,
    `owner_fixed`). 🔴 A team-owned agent keeps no credential in its row:
    `agents_team_no_secret` forces `hooks_token` and `gateway_secret` to `''`
    for a team owner on every write, so they live only in `agent_secrets`,
    which stays personal. `agents.Store.Secrets` answers only the agent's
    owner or the system scope; a member gets `pgx.ErrNoRows`.
  - **Which doors see a team agent:** a member sees it only where a person is
    present (the browser session, a paired phone: `Principal.TeamScope`). A
    personal API token, an agent and a mirror host carry no team and see
    none. This slice adds no sharing door. Phase 3's operator-approved
    exception stays as it is: a token that messages the chief can have it lend
    or unlend the owner's own `officer_host_spawns` rows to a team the owner
    operates. Nothing else; team drafts and charters stay session-only.
  - **A member's turn on a team agent runs as the team.** `chatTurn` (every
    chat door) re-mints the team acting principal (`turnActingContext`,
    through `ViaAgent`) after a fresh read of the member's role, on every
    send attempt. A member removed or downgraded while their message is
    queued is answered "not sent", and the message never reaches the runtime.
    The authoritative read is the **send fence**: immediately before each
    request opens, the role is read again under a `FOR KEY SHARE` on the
    team's row (`users.Store.HoldTeamRole`). The lock is held until the
    first of: ccd's 103 (the text was accepted), the status line, a
    transport error, `agents.SendFenceHoldCap` (5s) after the request was
    written, or the hard bound `agents.SendFenceHoldMax` (8s) after the lock
    was taken, connect and TLS included. Every membership change waits on
    that row (`lockTeam`), so a removal lands wholly before the read (the
    message is refused) or after the runtime took the text. Both bounds are
    operator decisions (#731): a membership change never waits longer than
    the hard bound (8s) behind a member's turn, under the 10s statement
    timeout. **Accepted residual:** on any runtime whose 103 does not arrive
    within the cap — one that never sends it (an older ccd, the
    chat-completions fallback), or a ccd whose submit takes longer (its
    window runs to 20s) — a message already transmitted before a removal may
    still be delivered after it. Before the fence (#673 until the fix), a removal committing
    between the attempt's first role read and its request let the text
    through.
  - **One shared room.** A member types into the team chief's one
    conversation, behind the per-agent FIFO turn gate. The runtime is handed
    `[muster] typed by <name> (<role>) in team <team>` before the text, in
    memory only. The stored row is what was typed, with `typed_by`. The room
    is the agent's delivery session (the chief's primary, any other agent's
    latest). A member's send or chat socket naming any other session of it
    (an archived thread, a side chat, a plugin thread) gets 403
    (`resolveTurnSession`). Reading another thread is still allowed.
  - **A member's message answers the agent's open asks only once it is
    sent.** For the owner, the asks are still answered at the door. For a
    member, they are answered after the fresh role check passes and the
    runtime takes the text (ccd's 103 or the first stream event;
    `addressAsker`). A removed member's dropped message answers nothing.
  - **A member's task dismiss leaves a team's linked agent running.** The
    task is dismissed. The agent's teardown is left to the managers' door of
    4.5. The idle reaper and the team's own doors still tear it down.
  - **Mark-read** (`POST /agents/sessions/{sid}/read`) is under the route
    gate. A team session has one read state, which belongs to the team, so it
    is written at operator. A viewer gets 403.
  - 🔴 **The team trust model: team members have to trust each other**
    (operator decision, 2026-11-14: accept and document). A team operator or
    manager can steer a team agent that holds tools, a team officer included,
    into printing its token or OAuth credential into the team's chat, an ask
    or an escalation. Every member can read those, viewers included. That
    credential acts **as the team**, not as the member, and it keeps working
    after the member is removed. No code stops this. The remedy is to
    **rotate the agent's credentials** (its hooks token, gateway secret and
    any connector or OAuth credential it holds). Do it whenever a member who
    could steer the agent leaves, and whenever a credential shows up where
    members can read it. (Slice 4.5 adds the managers' rotation door.)
  - **Route rules** (`teamAgentRoutes`, decided in `admit` for every route
    naming an agent, ledgered against `routes.golden`): reads at viewer; the
    chat socket and the send form at operator (a viewer gets 403); lifecycle,
    settings, new threads and the terminal are refused to every member in
    this slice. Lifecycle is the manager door of 4.5 (`ViaTeamAdmin`), and
    the terminal needs the step-up of 4.7.
  - **A team's gateway agent turns toolless from every chat door**, answer
    delivery included (`teamGatewayToolless`). This closes the gap carried
    from 4.1.
  - **Owner filters:** `chiefAgent`, `homeTarget` and the sidebar's primary
    chief resolve among the caller's own agents only. Every `Agents.List`
    caller is classified own / visible / machine
    (`TestEveryAgentsListCallerIsClassified`). So is every caller of
    `Agents.Get`, `Agents.GetByName` and `Agents.AgentsByNoteIDs`
    (`TestEveryAgentReadCallerIsClassified`). A "visible" caller does not act
    on a team agent as the member.
  - **`outcomeViewed`:** an operator of the owning team opening the agent's
    page marks its wait seen. A viewer's look leaves the push owed.
  - **Not yet:** the viewer's disabled composer and the team badge in the
    sidebar (UI); home falling back to a team chief; stop and start by an
    operator (4.5, so the team's credentials are used, not the member's).

- **Slice 4.5: the team's credentials, its chief, and the managers' door.**
  - **`MUSTER_TEAMS_ON_SERVER_POOL=<team ids>`** lends the server's Claude
    pool (`MUSTER_AGENT_CC_ACCOUNTS`) to each listed team, as an admin's
    agents use it (`credentials.Plane.TeamsOnServerPool`). It lends the Claude
    pool only: no OpenRouter key, and the gateway kind is refused for a team.
    A team not listed has no Claude account. Boot is refused when a listed id
    is a person or names nobody, and beside the Compose chief runtime.
  - **`ViaTeamAdmin`, the tenth team-acting door.** `teamAdminContext` is its
    one site. After a fresh read of the person's role says manager of the
    team, the action runs as the team acting principal, so the provisioner
    uses the team's credentials and the team's rows, never the manager's.
  - **Lifecycle on a team agent:** start, stop, delete and
    `POST /agents/{id}/credentials/rotate` are a manager's (`teamAgentManage`
    in `teamAgentRoutes`); operators and viewers get 403. The team's chief is
    never deleted. Rotation writes a new hooks token and, for a claude-code
    agent, a new gateway secret; for a team owner they land in
    `agent_secrets` only (0131's trigger blanks the shared row's columns), and
    a live instance is re-applied with them. Rotation needs a fresh sign-in.
    Settings (model, name), new
    threads and the terminal are still refused to every member.
  - **`/teams/{id}/chief`:** every member reads its status and where it runs;
    a manager provisions (`chief-<team>`, owned by the team, `claude-code`),
    starts, stops and restarts it. Refused, naming why and creating nothing:
    the Compose install, a deployment without the `claude-code` kind, and a
    team not on the server pool.
  - **Team instructions:** a team's chief gets a section saying its owner is
    the team, that members talk to it in one shared room behind the
    `[muster] typed by <name> (<role>) in team <team>` line, and that every
    member reads what it prints.
  - **Dispatch from a team's chief** refuses a host target (team hosts are
    2b), the gateway kind and a worker pool, each by name.
  - **Audit:** every manager action (`agent.start`, `agent.stop`,
    `agent.destroy`, `agent.credentials.rotate`, `chief.provision`,
    `chief.start`, `chief.stop`, `chief.restart`) writes an `audit_log` row
    owned by the team and naming the manager, shown on `/teams/{id}`.
  - **Deviations from the plan:** plan §6 lets an operator stop and start a
    team agent; this slice keeps lifecycle at manager, per the slice brief.
    Start, stop, delete and the chief's provisioning take no step-up
    (plan §6 lists none); rotation, which §6 does not list, takes the
    fresh sign-in (`stepup.Fresh`) the plan gives a manager's grant
    approvals, for the owner and the manager alike. The team chief's on/off is the
    agent's start/stop, with no `chief_preferences` row for the team. No
    "replace".

- **Slice 4.7: team dispatch.** A team's chief starts team-owned
  `pod/claude-code` agents (owner: the team; only on a team task, by the
  composite key onto notes). The blanket refusal of 4.5 is gone; the host,
  gateway and worker-pool refusals stay.
  - **Typed turn (`owner_asked`):** `markOwnerTurn` marks a member's turn on a
    team agent when the typer is a person who is an operator or above on a
    fresh read and their sign-in is fresh **at typing** (`stepup.Fresh` on the
    typing request). The marker *is* the step-up stamp: it exists only for a
    fresh typer, so no column records the time. `ownerTurnFor` re-reads the
    typer's role when the chief calls, and the lane checks are unchanged (one
    room). Anything failing falls back to the approval card (with a
    `request_key`).
  - **Approval card:** answering a team's dispatch card needs a browser
    session, an operator or above on a fresh read, and a fresh sign-in at the
    answer (`teamCardRefusal`, before anything is recorded; it reads the hold
    as the team, because `host_dispatches` is personal). The phone's door is
    refused by name. The completing call re-reads `answered_by_user` and
    refuses a removed answerer.
  - **A team's task is not its say-so (operator decision Q1):** on a team's
    task, a dispatch with no counting typed turn needs the approval card, as
    one with no task does (`teamTaskNeedsCard`); with no `request_key` it is
    refused. A person's own task is still their say-so.
  - **The marker ends with the typer's sign-in:** `ownerTurnFor` refuses a
    member's marker when the typer's `sessions_not_before` / `signed_out_at`
    is at or after the typed row's `created_at` (`typerSignedOutSince`; the
    send queue's whole-second rule).
  - **A disabled member speaks for no team:** `teamOperatorNow`, the one
    role re-check behind the typer, the card answerer (at the answer and when
    the start completes) and the terminal, also refuses a disabled person — a
    disable keeps the membership row.
  - **Push:** a team's dispatch card's push has no chips or signed actions and
    says to open it in a browser (`teamDispatchCardPush`, the same hold test,
    `teamCardHold`, as the phone-door refusal). No "Change repo" on a team's
    card until 4.8 (4.8: only on a team lent the GitHub App).
  - **Verbatim attention answer as instructions:** no `answered_stepup_at`
    column was added, so on a team's task the path is closed (fail closed):
    the chief's brief goes on an approval card instead.
  - **Audit and push:** `task.chief_dispatch` detail carries
    `requested_by_user`; the push names the person. Fan-out is 2b's.
  - **Terminal:** open to an operator or above with a fresh sign-in
    (`teamAgentTerminal`). The shell holds the team's credentials; accepted
    under the trust model (docs/chief.md).
  - **Move to a team (4.3 limitation (a)):** a finished link no longer
    refuses: an ask that is answered (delivery settled), cancelled or
    expired; a session route that is finished; a host dispatch that ended.
    Those rows stay their owner's. Agents (a composite key) and a plugin's
    queue still refuse.

- **Slice 4.8: the GitHub App for team agents** (operator decision: "yes;
  managers approve each repository once for the team, with step-up").
  - **`MUSTER_TEAMS_ON_GITHUB_APP=<team ids>`** lends the deployment's GitHub
    App to each listed team, with `MUSTER_TEAMS_ON_SERVER_POOL`'s boot check:
    a person's id or an unknown id refuses to boot, and so does the list with
    no App. One predicate, `githubapp.Admits` (an admin, or the team acting
    principal of a listed team), is both the `github` virtual connector's
    `allow` and `githubApp`'s gate, so the team's chief sees the App's
    repositories (`repos_list`, `agent_dispatch`'s connected-repository
    check, the default branch) and the team's agents see the App-backed
    `github` connector. A member's own principal is never admitted, a
    member of a listed team included: a member's personal chief and agents
    still have no repositories.
  - **Pages decide by the object's owner, never the viewer**
    (`githubAppForOwner`): the task forms' repository picker asks
    `/ui/agents/repos?owner=<owner>` (the edit modal its task's owner, the
    new-task form its Owner select), answered with a team's repositories
    only to an operator or above of a listed team; an admin on an unlisted
    team's task gets none. A listed team's approval card offers "Change
    repo"; its repo card lists the App's repositories as the team and is a
    team dispatch card like the approval card (browser, operator or above,
    fresh sign-in).
  - **`/teams/{id}/connectors`:** a manager reads the team agents' pending
    connector requests and approves (`github` only, with a fresh sign-in) or
    denies them. It runs through `teamAdminContext` (`ViaTeamAdmin`, a fresh
    manager read) rather than a System handler: the connector tables are
    personal, and the team acting principal is their owner, so it reads and
    writes the team's rows and no one else's. The decision is THE operator
    decision (`decideConnectorGrant`), so the approval is shared as the
    team's standing grant for (`github`, the App, the repository): the
    team's later agents on that repository are approved when they ask, and
    one on another repository asks again. Every decision writes the team's
    audit row (`connector.approve`, `connector.deny`) naming the manager.
  - **A manager revokes** (round-0 ruling: "add a manager revoke in this
    slice"): the page lists the team's approved repositories, each with
    Revoke, through the same door with a fresh sign-in. The revoke runs
    `RevokeStandingGrant` as the team, so row-level security limits it to
    the team's own standing grants (another team's or a person's is a
    404), and it ends every agent grant the standing grant gave: the
    team's next agent on that repository asks again. It writes the team's
    audit row `connector.revoke` naming the manager. **Unlisting a team
    pauses its approvals and re-listing restores them**: while unlisted the
    App is not the team's connector, so its agents' grants re-open and
    nothing is served, but the team's standing grants stay, and once the
    team is listed again its agents are approved from them. Revoke is the
    way to remove an approval.
  - **A team agent's request is pushed to the team's managers only**
    (an operator cannot decide it), linking `/teams/{id}/connectors`: a
    member's `/connectors` lists only their own grants.
  - **`githubapp.Admits` admits a team only at an explicit set of doors**
    (its agents' tokens, the connect proxy, the manager's door). A team
    host (`ViaMirrorHost`), the question watcher, the approval relay, a
    team officer and the background loops are refused, so a team-host
    tool added later does not inherit the App.
  - **Not done:** no migration.

### Later: consent-based team invites

A manager's email add (2a) puts a person in a team without asking them. A
team invite would instead let the person see the invitation, which team it
is for and its role, and accept it, before any membership row exists. It
needs its own table (pending invites, expiry, revocation) and a page for the
invitee, and it replaces the email add rather than sitting beside it. Not
scheduled.

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

## Phase 2b design: team hosts (decided 2026-11-14)

**Status: decided; slices 2b.1, 2b.2 and 2b.3 built (see their "as built" notes in F).** The operator accepted every recommendation in
[G](#g-operator-decided-2026-11-14), and commit `5f848980` records the
decisions. The build waits for phase 4.5 (PR #680) to merge. This section is
the design for phase 2b. It replaces the bullet list under
[Phase 2b](#phase-2b-team-hosts-built-last), which stays as the original
scope.

The section is written in the style of the "as built" notes above. Where it
says how muster works today, it cites `file:line` at `main` `069cb681`, and
the code is the authority. Two kinds of operator decision are cited, and
they are numbered separately:

- **D-1113 #N** is row N of the earlier
  [operator decisions](#operator-decisions-2026-11-13) table.
- **G1–G6** are this section's own decisions, in
  [G](#g-operator-decided-2026-11-14).

Every
example is synthetic: team `platform` (id 12) with `ana` (7) as manager,
`ben` (8) as operator and `cal` (9) as viewer; a team host `build-01`; and
`dee` (10), who is not a member.

2b has four parts: team hosts (A), team officers on team hosts (B), the rest
of phase 3's deferred fan-out (C), and the schema and doors that carry them
(D). Then come the tests that pin each one (E), the slice plan (F) and the
operator's decisions on its forks (G).

### Rulings that bind this design

These are operator decisions. The design takes them as given and does not
reopen them.

1. **A personal API token carries no team access.** There is one exception,
   carried unchanged from phase 3: a token-driven chief turn may lend and
   unlend the owner's own `officer_host_spawns` rows to a team the owner
   operates. 2b adds **no** token door. A personal token cannot list, watch,
   type into, mint for, or hear about a team host.
2. **Team members have to trust each other** (4.4). An operator who can steer
   a team agent or a team session can make it print a credential that every
   member then reads.
   - This ruling covers exactly that case: a credential a **steerable agent**
     prints while the person is still a member. In 2b it applies to a team
     host officer instance (B).
   - It does **not** cover a team mirror token that a person keeps **after**
     they leave. Leaving that token live until a manager rotates it rests on
     D-1113 #6 (mark rotate, never auto-revoke), and A states what such a
     token can reach.
3. **Team officers' notifications go to operators and managers**, never to
   viewers. 2b keeps this rule and extends it to every team-owned push.

### What the code does today

| fact | where |
|---|---|
| A team-owned mirror token is refused as if unknown: the lookup admits persons only | `internal/mirror/store.go:111-124` (`users.PersonIDs`, `:118`) |
| The mirror token's MCP door attaches a principal for the token's owner at `ViaMirrorHost` | `internal/api/mirror_mcp.go:220-262` (`:261`) |
| `admit` refuses a team principal with 403 (`TeamSignInRefusal`) at every door outside `teamAgentDoors` (`ViaAgent`, `ViaConnect`, `ViaRelay`). A team token's `POST /mirror/mcp` is refused **here first**, before `WithPrincipal` is reached | `internal/api/principal.go:178`, `:200`; `TestOnlyTheAgentDoorsAdmitATeam` (`doorAdmitsATeam[ViaMirrorHost] = false`, `internal/api/principal_team_test.go`) |
| `ViaMirrorHost` is not a team-acting door either: `WithPrincipal` panics on a team principal there | `internal/authz/authz.go:277-281` (9 doors) |
| The AskUserQuestion watcher reads a window's transcript as the question's owner through `askPrincipal` (`ViaAsk`). For a team owner that panics in `WithPrincipal` | `internal/api/mirror_ask.go:719`, called at `:746`; `TestEveryPerOwnerPrincipalSiteIsTeamSafe` classes it `refusedByWithPrincipal` (`internal/api/team_acting_pg_test.go:198`) |
| The send-queue dispatcher probes and delivers as the row's owner through `queuedPrincipal` (`ViaQueue`). For a team-owned row that panics in `WithPrincipal`, before any delivery check | `internal/api/send_queue.go:782`; `internal/sendqueue/dispatcher.go:343`, `limit.go:190`; the same test (`team_acting_pg_test.go:202`) |
| The doors that carry a person's memberships are `ViaSession` and `ViaDevice` only | `internal/authz/teams.go:120-123` |
| One chokepoint decides whose hub entries a request touches: `owner(ctx)` is the principal's own id | `internal/mirror/service.go:347-353` |
| The hub keys a host by `(owner, host)` and lists one owner's hosts | `internal/mirror/hub.go:534` (`Hosts(owner)`) |
| A queued send is checked at delivery against the row's **owner**, not the person who queued it. For a team row this is **latent**: the dispatcher panics at `queuedPrincipal` (above) before this check is reached | `internal/api/send_queue.go:1067-1090` (`sendQueueOwnerRefusal`, `ByID(ctx, it.Owner)` at `:1071`) |
| `send_queue.queued_by` already exists and cannot be forged | `0127`; `TestTheActingPersonCannotBeForged` |
| `sendPush` is the one caller of `push.SendTo`, and it sends to one owner | `internal/api/push_routes.go:293-303` |
| The minimal team fan-out (phase 3) reaches operators and managers for officer asks, escalations and drafts only, and is not withdrawn from them | `internal/api/team_notify.go:11-25`, `:79-93`; `internal/users/teams.go:268` |
| An SSE stream subscribes to one owner | `internal/api/server.go:1133-1134` |
| The disable sweep ends a disabled user's open streams every 10 s | `internal/api/accounts.go:131`, `:300` (`sweepDisabled`) |
| Boot refuses a team owning a `"runtime": "host"` officer | `internal/officers/owners.go:58-59` (`ErrTeamHostOfficer`) |
| On a machine that runs a host officer, every owner-authority tool (10 of them) files a confirmation card, answered only from the owner's paired phone | `internal/api/host_stepup.go:57-68` (`hostStepUpTools`), `:184`, owner check at `:257` |
| `officer_host_spawns` is personal and lendable; a team-owned row is visible only to the team acting principal | `0130_team_officers.sql:112-143`; `lentOwned` in `internal/db/share_ledger_test.go:81` |
| Focus titles are refreshed for persons only, on each person's own key | `internal/focus/pgstore.go:401-415` |
| `muster-server mirror token create` mints as user 1 | `cmd/muster-server/mirror_cli.go:44-48` |
| `Hosts` is matched by host name alone at about 49 call sites | e.g. `internal/api/plugin_handoffs.go:139-151`, `chief_nudge.go:1365-1378` |
| Every notification's per-owner decisions (codename, title rule, mutes and rate cap, router) are made in `goNotify`, before `sendPush` | `internal/api/server.go:1431`, `:1462-1481`; `push_routes.go:268-292` |
| The session-state pass iterates persons only, so a team session gets no waiting or done push | `internal/sessionstate/store.go:240-250` → `internal/chiefnudge/store.go:197` |
| A newer connection for the same `(owner, host)` replaces the live one | `internal/mirror/hub.go:767`, `:876-894` |
| A disabled person's mirror token is refused: the token lookup and the live connection's recheck filter owners through `users.EnabledPersonIDs`, and a disable revokes the person's mirror tokens in its own transaction (#681) | `internal/users/users.go` (`EnabledPersonIDs`, `SetDisabledAs` → `revokeMirrorTokensTx`); `internal/mirror/store.go` (`Authenticate`, `TokenLive`) |

> 🔴 **A delta from phase 2a's note.** 2a's text said "phase 2b adds
> `ViaMirrorHost` to the team-carrying set", and is now corrected in place.
> There are two sets, and 2b needs the acting one. A team mirror token acts **as the team** (the team
> acting principal: `muster.uid` = the team, no `muster.teams`). So
> `ViaMirrorHost` joins **`teamActingVias`**, the doors where something the
> team owns acts as its owner. It does **not** join `teamVias`
> (`Principal.TeamScope`), the doors where a person's memberships reach the
> database. A person's mirror token still carries none of their teams.

### A. Team hosts

#### What a team host is

A team host is a `muster-mirror` connected with a **team mirror token**: a
`mirror_tokens` row with `owner_id` = the team. The hub then keys the host as
`(12, build-01)` with no change to the hub, because to the hub a team is just
another owner. `build-01` may also exist as `(7, build-01)`, a personal host
of `ana`: host names are unique per owner, not per server.

The recommended setup does not change (§3): a machine, or a dedicated Unix
user on one, that the team treats as shared, running its own tmux server and
its own `muster-mirror` with the team token. INSTALL.md's team section says
so in one paragraph.

#### Who sees and does what

| on a team host's sessions | viewer | operator | manager | enforced by |
|---|---|---|---|---|
| see the host and its sessions in the sidebar and the switcher, with state and focus | ✓ | ✓ | ✓ | `mirror.Service.access` (need viewer) |
| Rendered view, read-only terminal, subagents, uploads, the asks list | ✓ | ✓ | ✓ | `access` (viewer) + RLS (shared tables, read at viewer) |
| scroll the terminal (moves copy-mode for every watcher; D-1113 #7) | ✓ | ✓ | ✓ | `access` (viewer) |
| open a preview that is already running (D-1113 #8) | ✓ | ✓ | ✓ | `access` (viewer) |
| start a preview, use the composer catalog | | ✓ | ✓ | `access` (operator) |
| type: interactive terminal, send, queue a send, answer a session's question in the pane | | ✓ + step-up | ✓ + step-up | `access` (operator) + `stepup.Fresh`, checked in the plugin as today (`service.go:383`) |
| answer the session's Attention asks (`attention_ask`) | | ✓ | ✓ | RLS (`attention_asks`, write at operator); no step-up (D-1113 #11) |
| cancel or edit **your own** queued send | | ✓ | ✓ | handler (`queued_by` = you) + RLS |
| cancel **anyone's** queued send | | | ✓ | handler (a **new** `requireTeamManager` gate: nothing of that name exists on `main` or in #680; it is the queue's counterpart of `requireTaskManager`) + RLS |
| mint, revoke and rotate the team's host tokens | | | ✓ + step-up | handler + RLS (`mirror_tokens` is managed, write at manager) |
| the session page's chief panel, proposals, succession, suggested replies | | | | refused on a team session (below) |

A viewer reads **everything** on screen and in the transcript, including any
secret a command printed. An operator who types runs commands as the host's
Unix user, so an operator on a team host has a shell there. Both are stated
on the role picker and on `/teams/12/hosts` (§3, the threat model). Nothing
here changes that.

**Refused on a team session, at every role.** Each of these sends the
session's transcript to a model on a **person's** credentials or a person's
chief, which D-1113 #10 forbids for a team's transcripts:

- `/mirror/view/chief`, `/mirror/view/chief/log` (the viewer's personal chief);
- `/mirror/view/proposal`, `/mirror/view/successor` (that chief's proposals and
  succession);
- `/mirror/view/suggest`, `/mirror/view/suggest/event` (suggested replies, on
  the viewer's own suggest key).

The page shows no panel and no chips there, and the routes answer 403. A team
chief's panel on a team session is later work, once a team chief can bind to a
team host ([G4](#g-operator-decided-2026-11-14)).

#### Who can send, and the step-up

- **Step-up is per person and per browser session**, unchanged
  ([sending-and-step-up.md](../sending-and-step-up.md#step-up-confirm-its-you)).
  `ben`'s send into a team session needs `ben`'s fresh sign-in in the browser
  he sends from. A paired phone's signed action counts, as it does today for
  an ask's suggestion.
- **A queued send is checked against the person who queued it.**
  `sendQueueOwnerRefusal` (`send_queue.go:1067`) reads `it.Owner`. For a team
  row that is the team, which is never disabled and has no cutoffs. 🔴 **This
  is a latent bug, not a live one.** Today a team-owned row never reaches the
  check: the dispatcher panics first, at `queuedPrincipal` (`ViaQueue` is not
  team-acting). It becomes live the moment `ViaQueue` admits a team, and then
  a disabled `ben`'s team send would be delivered. So the fix lands **in the
  same slice that admits `ViaQueue`** (2b.4), never after it.
  - 2b reads `queued_by` instead.
  - It adds two checks in the same place: `ben` is still a member of the
    row's owner, and his role there is at least operator.
  - All of them are read fresh at delivery.
  - A row that fails is cancelled with a reason that names the check ("Not
    sent: ben is no longer an operator of platform").
  - **A row the team queues itself** has `queued_by` = the team, because the
    actor column forces the acting principal. The coming example is a team
    chief's forward to a host dispatch (`host_forward.go:93`, under the team
    acting principal). Such a row is the **team's own**: the membership and
    role checks are skipped for it, exactly as no person check applies to a
    personal owner's own row. A team row whose `queued_by` is **neither** the
    team nor a member is refused, and that check fails closed. No such row is
    written in 2b. The rule is stated now so that the chief forward does not
    meet a refusal built for persons.
    🔴 **Superseded in 2b.4 (round 0, #702 D1):** until a team-acting writer
    lands, a row queued by the team itself is **refused** at delivery,
    failing closed; that writer's own change opens the pass, with its own
    test.
- **Edits.** Only the person who queued a row may edit it, so
  `edit_stepup_at` always belongs to `queued_by` and needs no `edited_by`
  column. A manager may cancel any row, but not edit it.
- **Cancelling on leaving.** `removeMemberTx` and a downgrade below operator
  (`internal/users/teams.go:289`, `:397`, where `endLends` already runs)
  cancel that person's queued rows on that team in the same transaction
  (`CancelQueuedBy(team, user)`, under System). A disable and a sign-out
  everywhere already reach `CancelOwner` (`send_queue.go:1126`). For a team
  row that names the team, so they gain the sibling
  `CancelQueuedBy(0, user)`, which covers every team.
- **One queue per window, shared.** This is unchanged from §3. The composer
  shows "queued by ben" on rows that are not yours.

#### The chokepoint

`owner(ctx)` (`service.go:347`) becomes `access(ctx, owner, need)`. It
returns `owner` when the principal is that owner, or when the principal holds
`need` in it, read from `Principal.TeamScope()`. Otherwise it returns
`ErrNoPrincipal`, the error a missing session gets today, so it is no oracle.

- **Every Service method takes the owner explicitly.**
- 🔴 **`Hosts(ctx)` stays own-only.** About 49 non-test callers in
  `internal/api` pick an entry by `h.Host == host` with no owner check. Some
  examples:
  - `plugin_handoffs.go:139-151` ("the host must be the principal's");
  - `chief_nudge.go:1365-1378`;
  - `attention_push_target.go:165`, `:221`;
  - `succession.go:773`.

  If `Hosts` listed `(12, build-01)` beside `ana`'s `(7, build-01)`, each of
  them would take whichever came first: a personal plugin handoff could be
  handed a team window. `HostView.Owner` is a field, not a guard. So the
  existing method does not change.
- **A separate, team-aware listing, `VisibleHosts(ctx)`.** It returns the
  principal's own hosts plus every team host where `TeamScope` is at least
  viewer, each `HostView` carrying `Owner`. Only callers that need team hosts
  use it: the sidebar, the session switcher and the host list. Each of them
  acts on a picked entry through `access(ctx, entry.Owner, need)`, never by
  name alone.
- **Its callers are a ledger.** `visibleHostsCallers` lists every caller of
  `VisibleHosts`, each with why it may see team hosts.
  `TestEveryVisibleHostsCallerIsClassified` is an AST walk like
  `agentReadCallers`'. It fails when the set grows or shrinks, so a new
  consumer has to opt in to team hosts.
- **The own-only listing stays own-only.**
  `TestHostsNeverListsATeamHost`: `ana`, an operator of team 12, with both
  `build-01` entries online, gets one entry from `Hosts`, `(7, build-01)`, and
  two from `VisibleHosts`.
- **Every page handler that takes `?host=&session=` takes `owner=` too.** A
  missing `owner` means the principal's own, so existing links keep working.
- **`TeamScope` is empty on every door but the browser session and the
  phone.** So a personal token, an agent, a person's own mirror host and every
  background loop reach no team host through `access`.
- **The team acting principal reaches its own hub entries.** A principal whose
  `UserID` is the team is the owner. That covers the team's officer runtime
  spawning on its host, and the team chief (phase 4).

#### Live streams follow the membership

2a's round 1 carried this forward to 2b: an open stream keeps the principal it
was admitted with. 2b closes it. `gotrueState.live`
(`internal/api/accounts.go:159`) records, for each live request on a team
owner, the team and the role it needs. `sweepDisabled` (`:300`) also re-reads
those people's memberships each `DisableSweepInterval` (10 s), in one query.
A request whose role in that team fell below its need is ended: a terminal, a
Rendered stream, a send socket. An SSE stream subscribes to its person and
each of the person's teams when it opens. On a removal or a downgrade below
viewer the sweep ends the stream, and the client reconnects with the new set.
A person **added** to a team sees the team's events from their next reconnect
or page load. That is accepted, and stated.

#### How a host joins and leaves a team

- **Joining.** A manager mints a team token on `/teams/12/hosts`, signed in
  and with step-up. The token is shown once. Then on the machine, as the Unix
  user the team shares: install the token in that user's `muster-mirror`
  token file and restart the unit. `muster-server mirror token create
  build-01 --team 12` mints the same token from the server's shell, under
  System, and writes an audit row with `via = cli`. A server shell can already
  do anything, so this grants nothing new. It exists for a headless bootstrap.
- **A personal host becoming a team host.** This is the same machine
  reconnecting with a different token. Its old entry `(7, build-01)` goes
  offline, and a new entry `(12, build-01)` comes online. 🔴 **Nothing the
  sessions had moves.** Rows keyed by the old owner stay `ana`'s: her focus
  history, episodes, asks, queued sends, session routes, pins and tags. This
  is because no one moves a row into or out of a team (`owner_fixed`, 0127).
  Her open asks from those sessions expire under today's host-offline rules,
  and her queued sends are held and then cancelled under the same rules. The
  team's view of the sessions starts empty. To keep history, `ana` runs two
  daemons on two tmux servers (multi-tenant.md's one process per tenant), one
  personal and one team, so nothing has to move. There is no "move this
  host's history to the team" operation ([G5](#g-operator-decided-2026-11-14)).
- **Leaving.** A manager revokes the team token. `watchToken`
  (`service.go:314`) cuts the tunnel within the recheck, and the team's
  sessions on that host vanish from every member's listing. Team rows stay the
  team's. The machine may then reconnect with a personal token as a personal
  host, and the team's rows never reach it.
- **A member leaves while a host stays.** D-1113 #6 holds: the team's host
  tokens are **marked rotate** (`mirror_tokens.rotate_at`, 0133). Nothing is
  auto-revoked.
  - **When the mark is set.** In the same transaction that does any of these
    three things:
    - removes an operator or a manager;
    - downgrades one to viewer;
    - **disables** a person who is an operator or a manager of the team.
      `SetDisabledAs` (`internal/users/users.go:314`) does not touch
      memberships, so without this trigger a disable would leave no mark.
  - **What the manager sees.** "rotate: ben left on <date>" (or "ben was
    disabled"), on `/teams/12/hosts`, until the token is revoked and
    re-minted.
  - **The reason is derived, not stored.** The page reads the latest audit row
    at or before `rotate_at`: `team.member.remove` or `team.member.set` (the
    team's own rows, written in the same transaction by `removeMemberTx` and
    the re-role path, `internal/users/teams.go:289-301`, `:385-405`), or
    `user.disable` (`users.go:347`).
  - 🔴 **What a retained team token reaches.** This is why the mark is urgent,
    and it is more than "had a shell on the host". The token is portable, so
    its holder does not need the host at all:
    - **It connects from any machine** as `(12, build-01)`.
    - **It displaces the real host.** The hub lets a newer connection for the
      same `(owner, host)` replace the older one (`internal/mirror/hub.go:767`,
      `:876-894`). From then on, the holder:
      - receives every member's step-up'd sends and pastes;
      - shows the team whatever sessions and transcripts it likes.
    - **It binds officer tools as the team.** The mirror door proves only the
      host; the session is the caller's claim, believed when that host lists
      it (`mirror_mcp.go:68-75`). A team officer spawn's `tmux_window` and
      `agent_session` are readable at viewer under `team_owner_read`, so a
      holder that lists them binds to the instance's officer tools as the
      team. That includes the officer's items and its contacts (0132's
      `officer_contact_get`, `officer_contacts_list`).
  - **The basis for leaving it live is D-1113 #6, not ruling 2.** D-1113 #6
    chose "mark, don't revoke", because a revoke cuts every team session at
    once. The trust ruling (ruling 2 above) covers credentials a steerable
    agent prints while the person is a member, and that is a different case.
  - **Closing the displacement path cheaply (slice 2b.2).** A team host's
    connection is **pinned** once it is live. A second connection for the same
    `(team, host)` while one is live is **held, not swapped in**:
    - The hub answers it "pending a manager's confirmation".
    - The live host keeps serving.
    - `/teams/12/hosts` shows a "replace the connected host?" card.
    - On a manager's confirm, the hub takes the new connection, and the old
      one is cancelled the way a replacement is today.
    - A connection arriving while the host is **offline** is taken at once,
      as today, so a restart of the real host is unaffected.
    - A personal host keeps today's newer-replaces rule.

    This closes the silent take-over by a retained token. It does not close
    the offline window, which only rotation closes. Pinned by
    `TestASecondTeamHostConnectionWaitsForAManager`: the second connection
    sees no sends while pending, and a confirm swaps it in. The control is a
    personal host's reconnect, which still replaces.

#### The team host's MCP door

A team token's `POST /mirror/mcp` acts as the team acting principal. Row-level
security then shows it **every** team row. So the reduced tool set (D-1113 #5)
is the whole guard, and it must be a ledger, not a list in prose.

- **Two doors must open first, in the same PR as the tool ledger.**
  - `admit` refuses a team at every door outside `teamAgentDoors`
    (`principal.go:200`). That refusal fires before anything else, so it gains
    `ViaMirrorHost`.
  - `teamActingVias` gains `ViaMirrorHost`, so `WithPrincipal` accepts the
    team acting principal there.
  - Neither door is opened without the other, and neither without
    `teamHostTools`. Opening only `admit` would still panic. Opening both
    without the ledger would hand the team's whole power to every session on
    the host.

- **`teamHostTools`** (`internal/api/mirror_mcp.go`) classifies **every**
  mirror door tool as `kept` or `refused` for a team token.
  - **Kept:** `attention_ask`, `attention_wait`, `attention_cancel` (the
    session's own asks); `tasks_list`, `task_get`, `task_comment`,
    `task_set_status` (tasks the session created); `agent_report_done` (the
    session's own dispatch); the officer instance tools, which
    `hostOfficerMirrorTool` binds to a recorded spawn.
  - **Refused because of G3:** the connector instance tools
    (`connectors_list`, `connector_request`, `connector_status`,
    `postgres_query`). A team host officer that declares connectors is refused
    at boot (G3), so no team instance ever needs them. Classing them
    `refused` pins G3 in the ledger as well as at boot.
  - **Refused**, by name, in the listing **and** at the call:
    - all 10 tools in `hostStepUpTools` on `main`: `attention_answer`,
      `connector_request_decide`, `plugin_draft_decide`, `agent_destroy`,
      `chief_restart`, `dispatch_approve`, `route_reply`,
      `plugin_message_route`, `plugin_request_decide` and
      `plugin_outbound_batch_decide` (#671);
    - every read of the owner's whole estate: `agents_list`,
      `connector_requests_list`, `plugin_drafts_list`, `plugin_messages_list`,
      `plugin_requests_list` and `plugin_outbound_batch_get` (#671; in
      `hostStepUpExempt` as "a read");
    - `dispatch_request` and
    `chief_ask`; and the page tools (`page_create`, `page_update`, `page_get`,
    `page_list`, `page_share_request`, `page_responses`, `page_link_revoke`).
    `pages` is a personal table, so a team-owned page would be unreadable by
    every member.
  - **A new mirror tool fails the ledger** until it is classified. The test
    pins the listing and the call to the same set.
- **`route_reply` is refused in 2b.** D-1113 #5 kept it "for a route to
  itself". But on a team host it relays text to an outside party as the team,
  with no manager's approval, and that is the role table's manager-only
  "approve outbound text". The operator decided to refuse it
  ([G2](#g-operator-decided-2026-11-14)). It is re-admitted only by
  its own later decision, together with a team chief routing team plugin
  messages to team hosts.
- **Remote mode is refused for a team token.** A call marked
  `X-Muster-Client: remote` with a team token is refused (401, "a team host
  token is not served remotely"). 🔴 **This refusal does real work; it is not
  redundant with the ledger.**
  - `remoteMirrorTools` (`mirror_mcp.go:188-194`) also serves the four task
    tools: `tasks_list`, `task_get`, `task_comment` and `task_set_status`.
  - The ledger above **keeps** those four.
  - A remote call is served unbound. The session it names
    (`X-Muster-Remote-Session`) is recorded on the rows it writes, but it is
    not checked against any listing.
  - So without this refusal, a remote caller holding a team token could name
    any session and work every team task that session created, as the team.
  - Do not delete the refusal as a duplicate. Its test is in E.
- 🔴 **The agent-read ledgers move with it.** `agentReadCallers` and
  `agentsListCallers` (`internal/api/team_agents_test.go:328-506`) class
  several callers as "machine: a mirrored session's tool (no team
  memberships)". That was true while the mirror door never carried a team. On
  a team token it does: the principal *is* the team, and a team agent is its
  own row. Each such entry must name the `teamHostTools` refusal that keeps a
  team token from reaching it, or be re-classed. A new check fails when a
  mirror-door caller in either ledger is neither.

#### Asks, Attention and push from a team host

- **A session's question is the team's.** `/mirror/ask` takes the owner from
  the token (`mirror_ask.go:179`), so a team host's question opens under owner
  12.
  - 🔴 **That alone is not enough.** The watcher that follows the question
    (`watchAsks`, `:746`) reads the window's transcript through `askPrincipal`
    (`ViaAsk`, `:719`). For a team owner that call panics in `WithPrincipal`.
  - So `ViaAsk` joins `teamActingVias`: the watcher reads the team's window as
    the team.
  - This lands in **2b.2**, with the token itself. `/mirror/ask` authenticates
    the host directly (`AuthenticateHost`, not `admit`), so the first
    AskUserQuestion on a connected team host reaches the watcher.
  - Members see the question at viewer, and an operator answers it.
  Answering in the pane is a typed answer, so it needs step-up. Answering an
  `attention_ask` needs none (D-1113 #11).
- **The paired phone.** The device answer route (`device_actions.go:51`)
  admits a team ask when the device's person is at least operator in the team,
  read fresh. `ViaDevice` already carries `TeamScope`, so RLS lets the read
  through and the handler checks the role.
- **First answer wins**, as today: the asks are conditional updates.
- **Pushes** are part C.

#### Waiting and done pushes for team sessions

The session-state pass is what turns a session that is waiting on a person,
or done, into an episode and a push. Today it iterates **persons only**:
`sessionstate.PG.Owners` (`internal/sessionstate/store.go:240-250`) calls
`chiefnudge.PG.Owners` (`internal/chiefnudge/store.go:197`, `users.PersonIDs`).
With session state on, `attentionMirrorChanged` hands each waiting window to
an episode "pushed by the state pass" (`internal/api/attention.go`). So a team
host's episode would be opened as the team and then never settled or pushed.
That leaves an open `session_episodes` row for team 12, and no operator is
told.

- **The team arm (slice 2b.3).** `sessionstate.PG.Owners` also returns every
  team with an unrevoked team mirror token, read under System beside the
  persons. The pass already acts as each owner through `attentionPrincipal`
  (`internal/api/session_state.go:117-126`), which is `ViaAttention`, already
  team-acting. A team's episodes are therefore opened, settled and pushed as
  the team, and C's fan-out delivers the push to members.
- **Whose classifier: none.** For a person, the pass may run the stall
  classifier on the session's last message, on that person's model key. For a
  team the classifier handle is `nil` ("the owner's classifier does not
  run"): it is a model-backed extra, off on team sessions
  ([G4](#g-operator-decided-2026-11-14)). Team episodes use only the
  deterministic state: waiting on a prompt, needing you, done.
- **Before 2b.3 (from 2b.2, when team hosts first connect).**
  `attentionMirrorChanged` opens **no** episode for a team owner, so no
  orphan row is left behind. 2b.3 removes that branch in the same PR that
  adds the arm.
- **Pinned by:**
  - `TestATeamHostOpensNoEpisodeWhileTheStateArmIsOff` (2b.2; removed by
    2b.3);
  - `TestATeamSessionWaitingPushesItsOperatorsOnce`: a team window waits,
    `ben` and `ana` get one push each, `cal` (viewer) none, and the episode
    settles when the window moves on;
  - `TestTheStatePassRunsNoClassifierForATeam`, whose control is a person's
    session, where the classifier does run.

#### What a team host does not get in 2b

- **Chief nudges, and the host-dispatch reconciler's per-person sweeps, on
  team sessions.** The nudge sweep iterates persons
  (`internal/chiefnudge/store.go:197`). A team session gets no nudge until a
  team chief binds to team hosts.
- **Focus titles on team sessions.** The refresher iterates persons and
  spends each one's key (`internal/focus/pgstore.go:401-415`). A team session
  has no focus title in 2b ([G4](#g-operator-decided-2026-11-14)).
- **A team chief dispatching onto a team host.** `host_dispatches` is
  personal, and the team acting principal can write its own rows, so nothing
  blocks it structurally. It is **UNVERIFIED** end to end, and it is
  phase 4's.

### B. Team officers on team hosts

Phase 3 refused a team owning a `"runtime": "host"` officer
(`owners.go:58`), because the hub never listed a team's host. 2b lifts that
refusal.

- **Boot.** `CheckOwners` drops `ErrTeamHostOfficer`. It gains one refusal
  in its place: a team host officer that declares `connectors`
  (`ErrTeamHostOfficerConnectors`). A team's connector requests would be
  `connector_grants` rows owned by the team. Those are personal, and no
  member can read them, so nobody could decide one. **UNVERIFIED:** whether a
  team **pod** officer with `connectors` boots today. Nothing in phase 3
  decides who decides its requests ([G3](#g-operator-decided-2026-11-14)).
- **Which host it runs on.** The officer's `host` names a host connected
  **with the team's token**. The runtime acts as the team (`ViaOfficer`,
  already team-acting), so every spawn, close and listing goes through
  `access` with the team as owner and reaches only `(12, build-01)`. 🔴 It
  can never spawn on `(7, build-01)`, a member's personal host of the same
  name, even when that is the only `build-01` online. That is structural (the
  hub key), and a test pins it. Boot cannot know which hosts will connect, so
  it does not check the host. An offline host changes nothing until it is
  back, as today. The host's `muster-mirror` needs `-interactive
  -agent-spawn` and the officer's `repo`, as a personal host does
  ([officers-builtin.md](../officers-builtin.md#officers-in-the-compose-stack-host-sessions)).
- **The agent row.** Each instance's row is an `agents` row with `owner_id`
  = the team and `runs_on_host` = `build-01`. `agents` is shared (0131), so
  members read it at viewer. `agents_team_no_secret` keeps it credential-free,
  and a host row holds none anyway. It is not in the agent list
  (`hostRowConsumers`), and its drafts are the team's. A manager approves them
  with step-up, never the team itself (`draft_approver`, 0130). Nothing about
  this row is new: phase 3 already makes it for a team pod officer. What is new
  is that its window is on a team host.
- **Its spawn rows.** `officer_host_spawns` rows for a team officer are owned
  by the team. Phase 3 made them visible only to the team acting principal. 2b
  adds a read arm, `team_owner_read`: members read a team-owned spawn row at
  viewer. Nobody but the team acting principal writes it. A team-owned row is
  never lendable: `lend_target` already refuses it, because the actor is not
  its owner. `/officers/spawns` groups them under the team.
- **The step-up gate on decide tools.** On a machine that runs a host
  officer, `hostStepUpGate` turns every owner-authority tool into a
  confirmation card for the **owner's** paired phone (`host_stepup.go:257`
  checks `p.UserID == pending.owner`). A team has no phone. On a team host
  this never arises, because every tool in `hostStepUpTools` is already
  refused for a team token (A, `teamHostTools`). So 2b puts `teamHostTools`
  **before** the gate, a team machine files no card, and the gate's owner
  check stays as it is. A test pins the ordering: on a team host running a
  team officer, `plugin_draft_decide` answers the `teamHostTools` refusal and
  files no Attention row.
- **What the instance can do.** It runs as the team host's Unix user, so it
  can do what every operator there can already do (§3's threat model). It adds
  no reach a member lacks, but an operator can steer it, which is ruling 2,
  accepted. Its drafts wait for a manager. It cannot decide anything through
  the mirror door.
- **Its notifications.** Its asks, escalations and drafts reach operators and
  managers (ruling 3), now through part C's one fan-out, with the notify
  switch, the team title and the withdrawal.

### C. The rest of the fan-out

Phase 3 shipped the minimal fan-out at **four** call sites, each looping over
`teamOfficerRecipients`:

- `team_notify.go:85` (`officerAskFiled`: a team officer's asks and
  escalations);
- `reply_draft_tool.go:119` and `:144` (its drafts);
- `outreach_batch.go:321-327` (#671: its outreach batches).

Of these, only the outreach batch already **withdraws** from members
(`clearBatchNote` per member). The officer asks and drafts are withdrawn from
the team owner only, which has no devices, so members keep them. 2b makes the
fan-out one rule in one place.

- **The expansion is in `goNotify`, ahead of the per-owner decisions.**
  `goNotify` (`internal/api/server.go:1431`) is where every notification is
  decided. Before it calls `sendPush`, it does three things for `owner`
  (`:1462-1471`):
  - reads the codename-title preference (`notifyCodenameFirstOn`);
  - runs the title rule once (`textplain.TitleKeeping`, pinned by
    `TestTheTitleSinkLedger`);
  - runs `pushAllowed`, which reads the owner's muted events and spends the
    owner's rate cap (`push_routes.go:268-292`).

  Then the router's owner rule (`routerServes(owner)`, `:1478`) decides
  `router.Notify`. Expanding in `sendPush` would run all of these against the
  **team**, which has no mutes, one shared cap and no preferences, so 2b
  expands earlier.

  When `owner` is a team, `goNotify` first reads the recipients: the team's
  **operators and managers** with `notify = true`, persons only, not
  disabled, read fresh (`TeamNotifyMembers`, gaining the `notify` filter;
  as built, `TeamPushTargets`).
  Then it runs the **whole** per-owner decision once for each recipient `m`,
  in this order:
  1. **The team prefix, first.** `platform · ` is prepended to `Title` and
     to `CodenameTitle`, so the title rule then runs over the whole title,
     once, as `TestTheTitleSinkLedger` requires.
  2. **`m`'s codename preference** (`notifyCodenameFirstOn(m)`).
  3. **The title rule**, once, on `m`'s copy.
  4. **`pushAllowed(ctx, m, event)`**: `m`'s own muted events and `m`'s own
     rate cap. So a busy team spends each member's cap, never one shared cap,
     and cannot dodge a cap by being a new owner.
  5. **`sendPush(ctx, m, …)`**, unchanged: "owner, and only owner".
  6. **`routerServes(m)`** decides `router.Notify` for `m`'s copy.

  `sendPush` itself is not taught about teams. Handed a team id, it sends
  nothing, because a team has no subscriptions. A personal owner goes through
  `goNotify` exactly as today.
- **What this reaches.** Every team-owned notification goes through
  `goNotify`, so all of them now reach members with no per-site code:
  - a team ask, a routed message, a task status, a team agent's outcome;
  - a team officer's ask, escalation, draft or outreach batch;
  - a team session that is waiting or done, once 2b.3's state-pass arm runs
    for teams (A, "Waiting and done pushes").
- **All four call sites go.** Each one calls the rule once, with `owner` = the
  team. Kept beside the new rule, any one of them would send each member a
  second copy. This is a consolidation: the test that a member gets **exactly
  one** notification is run for an officer ask, a draft **and** an outreach
  batch, and it is red if any of the four loops remains.
- **Withdrawal.** A `TypeResolved` notification also goes through `goNotify`,
  so an answered ask or a decided draft is withdrawn from the same
  predicate's members. The outreach batch's own member loop is subsumed.
  A person removed, downgraded or disabled between the send and the
  withdrawal would keep the notification. So `removeMemberTx`, a downgrade
  below operator, and the disable of a team operator or manager each send
  that person a withdrawal for each of the team's open asks, pending drafts
  and pending batches. That is bounded by the open set, and it runs after the
  transaction commits ([G1](#g-operator-decided-2026-11-14)).
- **Per-member notify switch.** `team_members.notify BOOLEAN NOT NULL
  DEFAULT true` (0133). It is the member's own setting. A manager does not
  set it for them.
  - It is toggled on **Account → Teams**, a list of your teams, your role in
    each and the switch.
  - The route is `POST /account/teams/{id}/notify`, at session tier,
    same-origin.
  - It is written under System by `users.PGStore.SetTeamNotify`, which
    updates only `notify`, only on the caller's own row, and only while they
    are a member. That entry goes in the System ledger.
  - Off means no push from that team at all. The team's items are still on
    Attention.

### D. Schema and doors

**Migration `0133_team_hosts`** (2b reserves **0133**). Re-checked on
2026-11-14 against `main` at `069cb681`:

- The newest migration on `main` is `0132_officer_contacts.sql`. It came from
  #669, merged 2026-11-14.
- Phase 4.5 (PR #680, head `7f41043f`) adds no migration.
- No other open PR adds one.
- So **0133** is free.

🔴 **Re-check the next free number against `main` again before building**,
because the build waits for 4.5 and 4.5 may still gain a migration. If 0133
is taken by then, renumber the file and its version constant. Every
statement is idempotent for `db.RLSOn`.

**One migration, landed with 2b.1, ahead of the slices that use it.** The
choice was between this and one migration per slice (0133, 0134, 0135). One
migration was chosen for three reasons:
- **Each change is inert without its code.** The two columns are defaulted
  or nullable, and the policy is SELECT-only. Landing all three early changes
  no behaviour.
- **It saves migration numbers.** One number means one re-check against a
  `main` that other work is also numbering. Three numbers would need three
  re-checks, at three different times.
- **The cost is an ordering rule, which is stated here.** 2b.2 needs
  `rotate_at` and 2b.5 needs `team_owner_read`. So:
  - 0133 ships **with or before** 2b.2 and 2b.5. 2b.1 lands first, which
    gives that.
  - 🔴 **Once 2b.2 is live, 0133 cannot be rolled back on its own.**
    `removeMemberTx` and the disable path write `rotate_at`, so dropping the
    column would make removing a member fail. Rolling 0133 back
    (`muster_team_hosts_rollback()`) needs 2b.5's code and then 2b.2's code
    rolled back first, in that order. Rolling back 2b.1's **code** alone
    (the fan-out) needs nothing else.

| change | what | rollback |
|---|---|---|
| `team_members.notify` | `BOOLEAN NOT NULL DEFAULT true` | drop the column; an older image never reads it |
| `mirror_tokens.rotate_at` | `TIMESTAMPTZ`, set when an operator or a manager leaves, drops to viewer or is disabled. Nothing clears it: a token marked rotate stays marked until it is revoked. There is **no reason column**: the page derives "ben left" from the team's audit rows (A, "A member leaves while a host stays") | drop the column |
| `officer_host_spawns` policy `team_owner_read` | `FOR SELECT USING (owner_id = ANY (coalesce((SELECT muster_team_ids(1)), '{}')))` | drop the policy; team spawn rows are visible only to the team again |
| `muster_team_hosts_rollback()` | drops all three, called from the header's rollback block as 2a's is | |

No share class changes. `mirror_tokens` has been managed (read viewer, write
manager) since 0127, and `send_queue`, `attention_asks`, `agent_focus`,
`session_episodes` and `uploads` have been shared since 0127. 2b writes the
first team rows into them.

**Code-only door changes:**

| door | change |
|---|---|
| `mirror.PGStore.Authenticate` (and `TokenLive`, the live connection's recheck, which applies the same rule) | admits a team-owned token. The owner filter, `users.EnabledPersonIDs`, becomes "**an enabled person, or a team**". Keep the disabled clause on the person arm: since #681 a disabled person's mirror token is refused there, and a disable also revokes the person's mirror tokens (`SetDisabledAs` → `revokeMirrorTokensTx`). Do not widen it back to `PersonIDs`. A team token held by a disabled operator is handled by 2b's rotate mark on disable (A), not by this filter: a team has no `disabled_at`. |
| `mirror.Service.Hosts` / new `VisibleHosts` | `Hosts` stays own-only. `VisibleHosts` adds the principal's team hosts, for a ledgered set of callers (`visibleHostsCallers`). **2b.3.** |
| the hub's newer-replaces rule, for a team host | a second connection for a live `(team, host)` is held until a manager confirms (A). **2b.2.** |
| `admit`'s team arm (`teamAgentDoors`, `internal/api/principal.go:178`, `:200`) | gains `ViaMirrorHost`, so a team token's `POST /mirror/mcp` reaches the handler **as the team acting principal** instead of a 403. **Slice 2b.2**, in the same PR as `teamHostTools`. Pinned by `TestOnlyTheAgentDoorsAdmitATeam` (`internal/api/principal_team_test.go`): `doorAdmitsATeam[ViaMirrorHost]` flips from `false` to `true`, and the walk then requires every `/mirror/mcp` request with a team token to arrive as the team. Every other door stays refused. |
| `authz.teamActingVias` | gains `ViaMirrorHost` (acts as the team), **2b.2**; `ViaAsk` (the question watcher reads the team's window **as the team**), **2b.2**; and `ViaQueue` (the dispatcher probes and pastes into the team's window **as the team**; who may still send is checked against `queued_by`, below), **2b.4**. That is 12 doors, or 13 after 4.5's `ViaTeamAdmin`, reached in two steps. Pinned by `TestTheTeamActingViasAreExactlyTheListedDoors` (`internal/authz/teamacting_test.go`), whose expected set grows in each of those two slices. |
| `askPrincipal`, `queuedPrincipal` | their rows in `TestEveryPerOwnerPrincipalSiteIsTeamSafe` (`internal/api/team_acting_pg_test.go:198`, `:202`) move from `refusedByWithPrincipal` to acting as the team, each in its own slice (2b.2, 2b.4). `focusPrincipal` and `stepUpPrincipal` **stay** `refusedByWithPrincipal`: focus is off on team sessions (G4), and a team host files no step-up card (B). Their unchanged rows are invariants. |
| `sendQueueOwnerRefusal` | reads `queued_by`, plus a fresh membership and role check. **2b.4, in the same PR as `ViaQueue`.** The bug is latent until `ViaQueue` admits a team (A, "Who can send"). |
| `authz.teamVias` | **unchanged**: `ViaSession`, `ViaDevice`. |
| `/teams/{id}/hosts` (GET; POST mint, revoke) | new manager page: tokens, last use, online state, rotate marks. Mint and revoke need step-up. |
| `/account/teams` (GET), `/account/teams/{id}/notify` (POST) | new member page and switch. |
| mirror page and send-queue routes | take `owner=`, with a rule per route (`mirrorTeamRoutes`). |

### E. What pins each change

Each test below is named before it exists. "Red at base" means the test must
be shown failing at the commit before its fix and green after it. "Invariant"
means it pins a property the base never violated. Name it as such, and do not
count it as regression coverage.

| change | pinned by | kind |
|---|---|---|
| `admit` lets a team token through at the mirror door, and nowhere new | `TestOnlyTheAgentDoorsAdmitATeam` (`internal/api/principal_team_test.go`), `doorAdmitsATeam[ViaMirrorHost]` → `true` | ledger; red at base (403 `TeamSignInRefusal`) |
| `ViaMirrorHost`, `ViaAsk` (2b.2) and `ViaQueue` (2b.4) are team-acting | `TestTheTeamActingViasAreExactlyTheListedDoors` (`internal/authz/teamacting_test.go`), set +2 then +1 | ledger |
| the ask watcher and the dispatcher act as the team | `TestEveryPerOwnerPrincipalSiteIsTeamSafe` (`internal/api/team_acting_pg_test.go`): `askPrincipal` (2b.2) and `queuedPrincipal` (2b.4) leave `refusedByWithPrincipal`. Plus `TestATeamHostsQuestionIsWatchedAsTheTeam`: an AskUserQuestion on `(12, build-01)` opens one team ask and its watcher runs | red at base (panic) |
| …and still carries no person's teams | `TestEveryDoorSaysWhetherItCarriesTeams` (`internal/authz/teamscope_test.go`), unchanged | invariant |
| a team token authenticates as the team | `TestATeamMirrorTokenAuthenticatesAsTheTeam` (`internal/mirror`) | red at base (it is refused) |
| the reduced tool set | `TestEveryMirrorToolIsClassifiedForATeamHost` (the `teamHostTools` ledger, failing when the mirror tool set grows or shrinks) **and** `TestATeamHostRefusesEachRefusedToolAtTheCall`, which calls each refused tool directly, expects the `teamHostTools` refusal text, and checks that no row changed | ledger + behaviour |
| remote mode is refused | `TestATeamTokenIsNotServedRemotely`. 🔴 **The fixture host must be unknown to this hub**: never connected here, or past `offlineRetention`. A remote-marked call from a host the hub knows is already refused earlier, by "ONLY A HOST THIS MUSTER DOES NOT MIRROR" (`mirror_mcp.go:143`), as a tool error. With such a host, a mutant that removes this refusal would still fail, and only on the status code. The case: an unknown `(12, build-01)`, a remote `task_get` with a team token naming a session that created a team task. It gets 401 and no row is read. The control is the same call with a personal token for an unknown personal host, which is served | behaviour. Red at base is not meaningful (base refuses every team token at `Authenticate`), so show it red against the 2b.2 tree with only this refusal removed: the call is then **served** (a row is read), not refused elsewhere |
| no mirror-door reader of a team agent is unrefused | `TestNoMirrorDoorAgentReaderIsReachableByATeamToken`, over `agentReadCallers` / `agentsListCallers` | seam ledger |
| `access()` is the one chokepoint | `TestEveryMirrorServiceCallerNamesItsOwner`, an AST walk of `internal/api` calls to `mirror.Service`'s owner-taking methods, each classed `viewer-owner` / `own-only` / `team-acting`, failing on growth or shrinkage | ledger |
| `Hosts` stays own-only; team hosts only through `VisibleHosts` | `TestHostsNeverListsATeamHost` (`ana` with both `build-01` entries online: one from `Hosts`, two from `VisibleHosts`), and `TestEveryVisibleHostsCallerIsClassified` (the `visibleHostsCallers` ledger, an AST walk that fails on growth or shrinkage) | invariant + ledger |
| a retained team token cannot silently displace the host | `TestASecondTeamHostConnectionWaitsForAManager`: while pending, the second connection receives no send and lists nothing to members, and a manager's confirm swaps it in. The control is a personal host's reconnect, which still replaces | red at base (base replaces) |
| waiting and done pushes for team sessions | `TestATeamHostOpensNoEpisodeWhileTheStateArmIsOff` (2b.2), `TestATeamSessionWaitingPushesItsOperatorsOnce` and `TestTheStatePassRunsNoClassifierForATeam` (2b.3) | red at base (persons only) |
| route rules | `TestEveryMirrorRouteHasATeamRule` (`mirrorTeamRoutes` against `routes.golden`, every `/mirror/`, `/ui/mirror/` and `/send-queue` route) | ledger |
| the refused personal-model routes | a row each in the isolation walk: viewer, operator and manager all get 403 on `/mirror/view/chief` and `/mirror/view/suggest` for a team session | behaviour |
| who sees and types | the isolation walk (`internal/api/isolation_test.go`): `dee` sees no `(12, build-01)` canary on any route or SSE event; `cal` watches and is refused every write; `ben` types with step-up and is refused the manager routes; nobody sees `ana`'s personal `build-01` | behaviour |
| delivery checks `queued_by` | `TestATeamSendIsCheckedAgainstWhoQueuedIt`. `ben` queues into `(12, build-01)`. 🔴 **The fixture must change him without the cancelling paths.** In 2b.4, `removeMemberTx` (`CancelQueuedBy(team, user)`) and a disable (`CancelQueuedBy(0, user)`) cancel his rows first. Through them the delivery check never runs, and the half-applied mutant cancels instead of delivering, so it stays green. So both cases change the database **directly**, under the migration role, and never call `SetDisabledAs` or `removeMemberTx`: (1) `UPDATE users SET disabled_at = now()`; (2) `DELETE FROM team_members` for `ben`. Only the delivery check now stands between the row and the pane. The fixed tree cancels it with `SendQueueOwnerDisabled` (or "no longer an operator"), and nothing is pasted. A separate case through the real `removeMemberTx` asserts the **cancel-path** reason, so the two guards are told apart by their detail text. 🔴 **The honest red conditions:** at base the dispatcher **panics** at `queuedPrincipal`, so a red-at-base run shows that panic, never a delivery. On the half-applied mutant (`ViaQueue` admitted, `sendQueueOwnerRefusal` still reading `it.Owner`) the row is **delivered**, and the test goes red: that is the bug this row exists for. A row with `queued_by` = the team is the team's own and is delivered (control) | red at base (panic) **and** red on the half-applied mutant (delivers) |
| a removed operator's sends | `TestLeavingATeamCancelsYourQueuedSendsToIt`: remove, then downgrade to viewer, in two cases | red at base |
| streams follow the membership | `TestARemovedOperatorsTerminalClosesWithinTheSweep` (≤ one `DisableSweepInterval`), plus the same for a Rendered stream and an SSE stream | red at base |
| rotate marks | `TestRemovingAnOperatorMarksTheTeamsHostTokensRotate`, with a viewer's removal as the negative control: no mark. `TestDisablingAnOperatorMarksTheTeamsHostTokensRotate`: `SetDisabledAs` on `ben` marks team 12's tokens and names `user.disable` as the reason. The control is disabling `cal` (a viewer), which leaves no mark. Plus `TestTheRotateReasonIsReadFromTheTeamsAudit`: the page's "ben left" names the `team.member.remove` row's user, with a fixture user id that no constant equals | red at base (no column) |
| team spawn rows readable | `TestTheShareLedgerHoldsForEveryRole` (`internal/db/share_ledger_test.go`): the `lentOwned` check now expects `owner_isolation`, `team_lent_read` **and** `team_owner_read` on `officer_host_spawns`, a viewer reads a team-owned row, and no member writes one | ledger |
| a team may own a host officer | `TestCheckOwnersAdmitsATeamHostOfficer`, and `TestCheckOwnersRefusesATeamHostOfficerWithConnectors` | red at base |
| it spawns only on the team's host | `TestATeamHostOfficerNeverSpawnsOnAPersonsHost`: only `(7, build-01)` is online, and the spawn is refused as "host offline", with nothing sent to it | invariant of the hub key: state it as one |
| no card on a team machine | `TestATeamHostFilesNoStepUpCard`: `plugin_draft_decide` on a team host running a team officer returns the `teamHostTools` refusal, and no Attention row is filed | behaviour |
| one fan-out | `TestATeamPushReachesItsOperatorsAndManagers`: a viewer, `notify = false`, removed and disabled members get none. `TestATeamNotificationReachesEachMemberOnce` (the consolidation), run for an officer ask, a draft **and** an outreach batch: red if any of the four `teamOfficerRecipients` loops (`team_notify.go:85`, `reply_draft_tool.go:119`, `:144`, `outreach_batch.go:323`) remains | red at base for the non-officer kinds |
| per-recipient decisions | `TestATeamPushHonoursEachMembersMutesAndCap`: `ben` mutes the event and gets nothing while `ana` is pushed; `ana` at her cap is throttled while `ben` is not; each member's codename switch picks their own title. `sendPush` handed a team id sends nothing (invariant) | red at base (base decides once, for the team) |
| withdrawal | `TestAnAnsweredTeamAskIsWithdrawnFromItsMembers`, the same for a decided draft, and the removed-, downgraded- and disabled-member sweep cases | red at base for asks and drafts (the outreach batch already withdraws: its case is an invariant) |
| titles | `TestATeamPushTitleNamesTheTeam`, with a personal push as the control. The team name is a fixture value that no constant in the code equals. The prefix is applied **before** the title rule: a title whose kept part runs past the limit still starts `platform · `. `TestTheTitleSinkLedger` (`internal/textplain`) still sees the rule run once per payload | red at base |
| the notify switch | `TestTheNotifySwitchIsTheMembersOwn`: a member flips only their own row; a non-member gets 404. `SetTeamNotify` goes in the System ledger (`internal/authz/ledger_test.go`) | ledger + behaviour |
| the device route | `TestAPhoneAnswersATeamAskAtOperator`: viewer refused, operator answers | red at base |
| 0133 rollback | the migration re-apply tests (`db.RLSOn`) and a `muster_team_hosts_rollback()` round trip | invariant |

The acceptance checks are §2b's original list: the extended isolation walk;
two operators answering one ask produce one answer; a removed operator's
queued send is cancelled and their terminal closes within 10 s; and a team
token's listing omits each refused tool **and** a direct call to it is
refused. To that list 2b adds one live check on a real two-daemon host: one
personal and one team `muster-mirror` on one machine show as two hosts, and
neither's sessions appear under the other owner.

### F. Slice plan (ranked)

🔴 **No slice starts before phase 4.5 (PR #680) merges.** Each slice begins
by re-checking the migration number (D). The slices ship **in order**.

1. **2b.1: the fan-out, and migration 0133.** Part C, plus the whole
   migration (D: the migration lands here, ahead of 2b.2 and 2b.5, which use
   it).
   - It needs no team host.
   - It fixes phase 3's team officers today: their asks and drafts are not
     withdrawn from members, there is no switch and there are no titles.
   - It is the smallest slice.
   - The expansion is in `goNotify`, ahead of the per-owner decisions, which
     then run per recipient: mutes, cap, codename, the title rule after the
     team prefix, and the router.
   - All four `teamOfficerRecipients` call sites are folded in.
   - Withdrawal goes to the team's current operators and managers. A person
     who is removed, downgraded below operator or disabled gets a withdrawal
     for each of the team's open asks, pending drafts and pending batches.
     There is no recipient table (G1).
   - There is one notify switch per team, `team_members.notify`, on
     Account → Teams (G6).

   > **As built in 2b.1 (0133).**
   > - **Migration `0133_team_hosts`**, as D specifies: `team_members.notify`,
   >   `mirror_tokens.rotate_at` (no reason column), and `team_owner_read` on
   >   `officer_host_spawns`, with `muster_team_hosts_rollback()`. Re-applied by
   >   `db.RLSOn`, keyed on `team_members.notify`. The share ledger pins the
   >   new arm (`teamOwnerRead` in `internal/db/share_ledger_test.go`): a
   >   viewer reads a team-owned spawn row through the browser and a phone,
   >   no machine door does, and no member writes one.
   > - **The fan-out** is in `goNotify` (`internal/api/server.go`), reading
   >   `users.PGStore.TeamPushTargets` (`teamPushRecipients`,
   >   `internal/api/team_notify.go`). Per recipient, in this order: that
   >   member's codename switch picks `Title` or `CodenameTitle`; the team
   >   prefix `<team> · ` goes on the title it picked; the title rule runs
   >   once; `pushAllowed` with the member's mutes and cap; `sendPush`
   >   (unchanged, owner-only); `routerServes` for the member. The prefix is
   >   applied after the codename pick rather than to both titles first: the
   >   title sent is the same, and the sink then writes no `CodenameTitle`
   >   (the codename ledger's rule for `goNotify`).
   > - **Recipients:** operators and managers, persons, not disabled, with
   >   `notify` on. A **withdrawal** (`resolved`) goes to the operators and
   >   managers whatever their switch says, so a member who turns it off
   >   after a push still has it closed (G1 names "current operators and
   >   managers", not the switch). A read that fails sends to the owner
   >   alone, which for a team is nobody: the pre-2b behaviour.
   > - **The four `teamOfficerRecipients` loops are gone**, and so are
   >   `teamOfficerRecipients` and `officerAskFiled`: an officer's ask is
   >   `askFiled`. Every team-owned push now fans out, a team's **reply**
   >   drafts and task pushes included (phase 3's test that a team reply
   >   draft did not fan out is inverted).
   > - **Decided drafts and batches are withdrawn from their owner**, not
   >   from the deciding manager (`plugin_drafts.go`, `outreach_batch.go`):
   >   that was the bug that left a team draft on every other member's
   >   phone.
   > - **The sweep** (`internal/api/team_withdraw.go`): a removal (manager's
   >   or admin's), a role set below operator (manager's re-role or admin's
   >   set) and a disable (`teamItemsWithdrawnBeforeDisable`, called by both
   >   `DisableUser` and `DisableUserAs`) send that person a withdrawal for
   >   each of the team's open items of every withdrawable kind (below), read
   >   as the team. A removal and a downgrade sweep after the store's
   >   transaction commits; a disable sweeps before it. A disable through the
   >   CLI (`SetUserDisabledAs` with no server) sends no sweep: that process
   >   has no push service.
   > - **The switch:** `GET /account/teams` and `POST
   >   /account/teams/{id}/notify` (`on=1|0`), linked from `/account`.
   >   `users.PGStore.SetTeamNotify` updates only `notify` on the caller's
   >   own row and answers `ErrNotAMember` (one 404) otherwise. It runs
   >   under System through `internal/users.sys`, which the System ledger
   >   already lists; no new ledger row was needed.
   > - **A team task comment (4.3's limitation c, agreed with the phase-4
   >   session):** a member's comment through the browser session
   >   (`handleNoteComment` → `teamTaskCommented`, `push_task.go`) pushes the
   >   team's operators and managers through the same fan-out, under the
   >   `comment` toggle, at once (not coalesced), excluding its author
   >   (`RouterNotification.skip`). An agent's comment on a team task keeps
   >   the agents' no-push rule, as on a personal task.
   > - **Page refresh for team items (#683 round 0, Q1, operator decision).**
   >   `attentionChanged` is the one place: for a team owner it also sends
   >   `attention.changed` to the team's current operators and managers
   >   (`teamViewRecipients`: the fan-out's read, without the notify switch).
   >   That keeps #671's per-member refresh of an outreach batch arriving and
   >   being decided, and gives team asks and drafts the same. A person's id
   >   is remembered as a person, so their changes read nothing more. 2b.3's
   >   stream subscriptions replace it; delete it then.
   > - **A failed recipient read** is counted in
   >   `muster_push_suppressed_total`: `team_read_failed` for an owner known
   >   to be a team, `owner_kind_unread` for one whose kind was never read (a
   >   person's push still went). A known person is never read again
   >   (`personOwners`; `users.kind` never changes).
   > - **The sweep's kinds are one list** (#683 round 1),
   >   `withdrawableTeamKinds` in `team_withdraw.go`: asks, drafts and
   >   outreach batches, tasks in review, officer cap holds. Cap holds are
   >   recorded in memory as the officer loop shows and ends them, as the
   >   episode itself is; a restart forgets an open one.
   >   `TestEveryPushIsClassifiedForTheTeamSweep` classes every push the
   >   package builds (the codename ledger's set) as one of those kinds or as
   >   never withdrawn from a team member, so a new kind must choose.
   > - **A disable sweeps before it commits** (#683 round 1):
   >   `SetDisabledAs` deletes the person's paired phones and `ForOwner`
   >   refuses a disabled owner's native rows, so the server's disable
   >   (`DisableUser`, `DisableUserAs`) sends the withdrawals first and waits
   >   for them (`RouterNotification.wait`). A disable then refused (the last
   >   admin) has closed notifications that were still the person's; the next
   >   change to each item shows it again. The CLI's disable sends none.
   > - **Known windows, recorded, not closed** (#683 round 1, 🟢6 and 🟢7):
   >   - a push that read its recipients just before a removal committed can
   >     be delivered after the sweep's withdrawal, and then stays on the
   >     ex-member's devices: inherent to G1's "no recipient table";
   >   - `attentionChanged`'s team arm reads the team's members
   >     synchronously (a 5 s timeout) on every change to a team item,
   >     inline in the request or loop that made it. A person costs nothing
   >     after the first read. No per-request cache was added: the refresh has
   >     no request to hang one on, and a longer-lived one would refresh a
   >     member who just left. 2b.3's stream subscriptions delete it.
   > - **The machine comment route** (`POST /api/tasks/{id}/comments`)
   >   carries no team, so it never reaches a team task (404, pinned in
   >   `TestACommentOnATeamTaskNotifiesItsOtherOperators`). It was left as
   >   it is: 4.3's limitation (c) asked that members' comments notify the
   >   team, and a member's comment goes through the browser session.
   > - **Not in 2b.1:** SSE subscriptions for teams. Part C assigns no SSE
   >   work; 2b.3 owns it ("SSE team subscriptions, the membership sweep on
   >   streams"). `TestAPhoneAnswersATeamAskAtOperator` is 2b.4's (the device
   >   route) and is not here.
   > - **Tests:** `TestATeamPushReachesItsOperatorsAndManagers`,
   >   `TestATeamNotificationReachesEachMemberOnce`,
   >   `TestATeamPushHonoursEachMembersMutesAndCap`,
   >   `TestATeamPushTitleNamesTheTeam`,
   >   `TestAnAnsweredTeamAskIsWithdrawnFromItsMembers`,
   >   `TestADecidedTeamDraftIsWithdrawnFromItsMembers`,
   >   `TestLeavingATeamWithdrawsItsOpenItems`,
   >   `TestTheNotifySwitchIsTheMembersOwn`,
   >   `TestACommentOnATeamTaskNotifiesItsOtherOperators`,
   >   `TestATeamItemRefreshesItsMembersPages`
   >   (`internal/api/team_fanout_pg_test.go`), and
   >   `TestMigration0133TeamHosts` (`internal/db`).
2. **2b.2: the team token and its door.**
   - `Authenticate` admits a team.
   - `admit` lets a team through at `ViaMirrorHost` (`teamAgentDoors`), and
     `ViaMirrorHost` becomes team-acting.
   - `ViaAsk` becomes team-acting, so the first AskUserQuestion on a connected
     team host does not panic the watcher.
   - `teamHostTools`:
     - refuses every owner-authority tool, **`route_reply` included** (G2);
     - refuses the estate reads and the page tools;
     - refuses the connector instance tools (G3).
   - Remote mode is refused.
   - The agent-read ledger seam.
   - `attentionMirrorChanged` opens no episode for a team owner, until 2b.3.
   - `/teams/{id}/hosts` and `--team`.
   - Rotate marks, on removal, downgrade **and disable**.
   - A team host's second connection is held for a manager's confirmation
     (A).

   🔴 `teamHostTools`, `admit`'s mirror arm and `ViaAsk` land in the **same**
   PR as admitting the token. Admitting the token without the ledger would
   hand every team host session the team's whole power. Admitting it without
   `ViaAsk` would let a team host panic the ask watcher.

   After this slice a team host connects, but no member sees its **sessions**
   yet. Its **asks** do appear: `attention_asks` is read at viewer, so a team
   host's question shows on members' Attention from this slice on, and an
   operator may answer it there (D-1113 #11).

   > **As built in 2b.2 (no migration; 0133's `rotate_at` is its column).**
   > - **The token lookup.** `mirror.PGStore.Authenticate` and `TokenLive`
   >   filter owners through a NEW constant, `users.EnabledPersonOrTeamIDs` =
   >   `EnabledPersonIDs` + a team arm (`UNION ALL … kind = 'team'`).
   >   🔴 **Deviation:** `EnabledPersonIDs` itself is not widened. On main it
   >   also filters the focus refresher's owners, the flow runner token and
   >   plugin tokens and pairing, none of which may yield a team (G4, and the
   >   users-read ledger). `TestTheTeamArmKeepsThePersonRule` pins the new
   >   constant to the person rule plus that arm, and the users-read ledger
   >   gains the class "enabled persons, or teams". `Authenticate` also
   >   reports `Token.Team`, which pins the connection (below).
   > - **The doors.** `admit`'s team arm (`teamAgentDoors`) gains
   >   `ViaMirrorHost`; `teamActingVias` gains `ViaMirrorHost` and `ViaAsk`
   >   (12 doors). `ViaMirrorHost` also fronts `GET /mirror/host` (the
   >   doctor's this-host check), which a team token now opens too: it names
   >   only the token's own host. `askPrincipal` acts as the team.
   >   🔴 **Deviation:** D's table says `stepUpPrincipal` stays
   >   `refusedByWithPrincipal`. It uses `ViaMirrorHost`, so WithPrincipal no
   >   longer refuses a team there; `stepUpPrincipal` now refuses a team
   >   itself (`stepUpTeamRefusal`, a panic — unreachable: a team host files
   >   no card), and its per-owner row is the new class `refusedStepUpNoTeam`.
   > - **`teamHostTools`** (`internal/api/team_host_tools.go`): 19 kept
   >   (the three attention tools, the four task tools, `agent_report_done`,
   >   the 11 officer instance tools), 29 refused, as A lists them. A team
   >   token's `tools/list` is the kept tools only (also over the opencode
   >   set, for an opencode session on a team host). 🔴 That listing filter is
   >   the ONE guard (#685 round 1, G3): serveMCP runs a listed name only, so
   >   a refused or unclassified name never reaches the call; serveMCP's
   >   disabled hook only words the refusal (`teamHostToolRefusal`) instead of
   >   "unknown tool". There is no second check at the call. Being unlisted,
   >   a refused name never reaches `hostOfficerMirrorTool` or the step-up
   >   gate.
   > - **Remote mode** answers 401 `TeamHostRemoteRefusal` to every
   >   remote-marked request of a team token (initialize and tools/list
   >   included), before serveMCP.
   > - **The agent-read seam** (`TestNoMirrorDoorAgentReaderIsReachableByATeamToken`,
   >   `mirrorDoorAgentReaders`): the nine ledger entries that serve a mirror
   >   tool name the `teamHostTools` refusal, or, for the officer reads, what
   >   they read on a team host. ⚠ Its growth check is a convention (an entry
   >   whose line names the mirror door must be listed), not a call graph.
   > - **The second connection** (`internal/mirror/teamhold.go`). For a team
   >   token, `ServeConnect` asks the hub before it touches the connection's
   >   token record: with `(team, host)` live and no confirm armed, the
   >   connection is refused at once (`ErrHeldForManager`, "pending a
   >   manager's confirmation") and the attempt recorded.
   >   🔴 **Deviation from "held, not swapped in … on a confirm the hub
   >   takes the new connection":** the held socket is refused, not parked,
   >   because muster-mirror waits 15 s for a welcome and would otherwise
   >   churn. Each held attempt is recorded by its TCP peer address, with
   >   when it was first and last seen, and the card lists every one. A
   >   manager's confirm names ONE attempt by its address and arms a one-shot
   >   replacement for `ReplaceConfirmWindow` (3 min; muster-mirror retries at
   >   most a minute apart) that admits only a reconnect from that address;
   >   it replaces the live one the way a newer connection does today. There
   >   is no confirm without an address (#685 round 0: a blanket confirm let
   >   a kept token retrying every 2 s win the race). ⚠ The address is the
   >   TCP peer, not X-Forwarded-For: behind a proxy chain every machine is
   >   one address, and the binding cannot tell them apart. No protocol
   >   change. A connection while the host is offline is taken at once; a
   >   personal host keeps newer-replaces. `muster doctor` on a held machine
   >   reports `held` (`GET /mirror/host`, `Service.HeldHere`: an attempt
   >   from its address is held and the live connection is from another),
   >   not `online`.
   >   🔴 **Round 1 (Y1), measured on a NodePort deployment: every host
   >   showed as the same pod-network address.** So in the supported deployments (a proxy chain or
   >   a NodePort) the address binding tells NO machines apart: the held
   >   machine and the live host are one address. A confirm for an attempt
   >   whose address equals the live connection's is therefore REFUSED
   >   (`ErrIndistinguishable`), and the card says "this machine can't be told
   >   apart from the live host by address; stop the live host first, or
   >   replace the token". `muster doctor` there reports `held_unsure` (FAIL:
   >   the server cannot tell which of the two this machine is), not online.
   >   A per-attempt code that binds the confirm to one machine is a separate
   >   follow-up PR, not built here.
   >   🔴 **The follow-up (#688): the ATTEMPT CODE, and the address binding
   >   above is GONE (no migration; the hold is in memory).** Round 1's
   >   proposal, built, then cut down by its own round 0 (D1 to D3).
   >   `internal/attemptcode` (a stdlib-only leaf, so the CLI links it
   >   without the mirror package) draws a code per muster-mirror PROCESS:
   >   12 Crockford base32 characters (60 bits from crypto/rand), shown
   >   `XXXX-XXXX-XXXX`; typed forms (case, dashes, spaces, I/L/O)
   >   normalise. muster-mirror sends it on every `/mirror/connect` as
   >   `X-Muster-Attempt-Code` (no tunnel protocol change), logs "held: …
   >   give this code to a team manager: <code>" when held, and writes it
   >   0600 to `-attempt-code-file` (`MUSTER_MIRROR_ATTEMPT_CODE_FILE`,
   >   default `internal/mirrorpath.DefaultAttemptCodeFile`), removed at exit
   >   while it is still its own. The hub records a coded attempt by the
   >   code's SHA-256 (domain-separated), never the code: `heldAttempt.code`
   >   is the hash, the map key is its hex, and logs and the audit row carry
   >   `attemptcode.Fingerprint` (16 hex). The card has no per-row button; one
   >   field takes a typed code (`POST …/hosts/replace` with `code`, same
   >   route, same step-up), and `Hub.ConfirmCode` arms the held coded
   >   attempt with that hash; `heldAttempt.admits` then lets in only a
   >   reconnect presenting that code, one-shot, within
   >   `ReplaceConfirmWindow`. A wrong code arms nothing and is audited
   >   `team.host.replace.refused` (with the typed code's fingerprint).
   >   🔴 **No code, no confirm:** an attempt with no code (or a malformed
   >   one) for a LIVE team host is held with `ErrHeldNoCode` ("upgrade
   >   muster-mirror on this machine, or stop the live host"), and the card
   >   says the same; nothing can confirm it. #685's address confirm
   >   (`ConfirmReplacement`, `SameAsLive`, `ErrIndistinguishable`, the
   >   per-row address form, `Service.SetPeerAddr`) and doctor's
   >   `held_unsure` are deleted: in the supported deployments it always
   >   refused, and team hosts had never shipped, so no older muster-mirror
   >   needs it. Offline arrival is unchanged, code or no code. `muster
   >   doctor` sends the code it reads from the file to the token's own
   >   server on `GET /mirror/host`, and `Service.HeldHere` answers by the
   >   code alone; with no code it answers not held.
   >   Round 0 also deleted a per-team wrong-code rate limit (only a
   >   step-up'd, trusted manager can type a code, and a guess gains no one
   >   anything) and a structural constant-time test (the compared values
   >   are hashes); the `subtle.ConstantTimeCompare` call stays.
   >   **Round 1 (#688):** past `maxHeldAttempts` (16) per host the attempt
   >   forgotten is the oldest seen only ONCE, else the oldest, and never
   >   the confirmed one; a real muster-mirror retries with one code, so
   >   once the moved machine has retried, a spray of fresh codes (each seen
   >   once) evicts only sprayed ones. Holds are announced at most once per
   >   `heldAnnounceEvery` per (team, host), not per code. The live
   >   connection's code hash is kept with its token, so `GET /mirror/host`
   >   answers held, connected, or `held_unknown` (machines are held and the
   >   request's code is missing or neither), and doctor FAILs on unknown
   >   rather than PASS "connected". muster-mirror prints the code once per
   >   process; later holds point at `muster doctor`. Fingerprints are an
   >   HMAC under a per-process key, so a log or audit reader cannot test
   >   guesses offline (they match only within one server process).
   >   `heldCode` honours `heldRetention` like `ConfirmCode`.
   >   ⚠ **Not closed:** a kept token can still spray. Before the moved
   >   machine's SECOND attempt its record is seen once, so a spray can
   >   evict it, and its correct code then answers "no such code" until its
   >   next retry (at most a minute) records it again; the answer to a
   >   sprayer is still revoke and re-mint. The code is per process, so a
   >   restart of the moved machine draws a new one. Anyone who can read the
   >   moved machine's muster-mirror log (its user's journal, admins, any log
   >   shipping) sees the code once; with it, a kept copy retrying fast can
   >   race the moved machine inside the confirm window.
   >   Tests: `TestASecondTeamHostConnectionWaitsForAManager`,
   >   `TestTwoHeldAttemptsArmNothingUntilOneIsChosen`,
   >   `TestACodeConfirmAdmitsTheMachineShowingItAndNoOther`,
   >   `TestAWrongCodeArmsNothing`,
   >   `TestACodelessAttemptIsHeldAndCannotBeConfirmed`,
   >   `TestAHeldMachineLogsItsCodeAndTheServerOnlyTheHash`,
   >   `TestDoctorsQuestionIsAnsweredByTheCode` — over the REAL loopback
   >   peer, every connection one address — and
   >   `TestTheHubKeepsOnlyTheCodesHash` (`internal/mirror`);
   >   `TestTheCardTakesATypedCodeAndOffersNoRowButtonForACodedMachine`
   >   (`internal/api`); `TestDoctorShowsAHeldMachinesCode`
   >   (`cmd/muster`); `TestTheAttemptCodeFileIsPrivateAndOnlyItsOwnerRemovesIt`
   >   (`cmd/muster-mirror`); `TestACodeIsTypeableAndTypedFormsAgree`
   >   (`internal/attemptcode`).
   >   Held attempts are logged and nudge members' views at most once per
   >   `heldAnnounceEvery` (30 s) per (team, host) since #688 (G2 bounded it
   >   per address); every attempt is still counted on the card.
   >   🔴 **The hold guards the tunnel only (G1).** A held machine, or any
   >   kept copy of the token, can still call `/mirror/mcp` and `/mirror/ask`
   >   as any session the LIVE host lists: those doors bind a call to the live
   >   host's listing, and the session id is the caller's claim. The rotate
   >   mark (revoke and re-mint) is the answer to a kept token.
   > - **`/teams/{id}/hosts`** (`team_hosts_page.go`): GET, `POST` (mint),
   >   `POST …/{token}/revoke`, `POST …/replace`. Managers only (the request's
   >   memberships, then `teamAdminContext`'s fresh read), and the token rows
   >   are read and written AS THE TEAM. Mint, revoke **and the replace
   >   confirm** need step-up (the design was silent on the confirm; it decides
   >   who receives every member's sends). Each is audited as the manager
   >   (`team.host.token.create`, `.revoke`, `team.host.replace`). Linked from
   >   `/teams/{id}`.
   > - **`muster-server mirror token create <host> --team <id>`**:
   >   `mirror.PGStore.CreateTeamToken` under System, which mints only when
   >   the id names a team (`ErrNotATeam`), with the team's audit row
   >   (`via = cli`) in the mint's own transaction. `mirror token ls` still
   >   lists the operator's tokens only; a team's are on its page.
   > - **Rotate marks** (`internal/users/team_hosts.go`): `removeMemberTx`
   >   (an operator or a manager), `setMemberTx` (operator or manager →
   >   viewer) and `SetDisabledAs` (an operator or manager of any team) set
   >   `rotate_at = now()` on the team's unrevoked tokens in their own
   >   transaction. A later trigger moves the mark forward, so the page names
   >   the latest reason. The reason (`TeamRotateReason`, under System: a
   >   `user.disable` row names no team) is the newest of the three triggering
   >   audit rows **written in the mark's own transaction** (`at =
   >   rotate_at`, round 1 G4: "the newest row before it" showed a stale
   >   disable of someone who was a viewer then), each condition reading the
   >   row's own role, never today's membership. A disable through
   >   `SetDisabled` with no actor writes no audit row; the page then says
   >   "rotate (marked <date>)".
   > - **No episode for a team owner** (`attentionMirrorChanged`): with
   >   session state on, a team's waiting windows take today's diff path, not
   >   the episode one. An owner whose account could not be read keeps the
   >   person path. 2b.3 removes the branch.
   > - **The phase 4.7 seam (agreed with that session):** 2b.2 adds no answer
   >   path for team asks. `attention_answer` is refused to a team token, and
   >   `attention_wait` / `attention_cancel` are bound to the session's own
   >   asks; `askPrincipal`'s callers only read transcripts.
   >   `TestATeamHostCannotAnswerATeamAskThatIsNotItsOwn` pins it over a
   >   dispatch-shaped System card, a team agent's ask and another session's.
   > - **Tests:** `TestATeamMirrorTokenAuthenticatesAsTheTeam`,
   >   `TestAuthenticateSaysWhetherTheOwnerIsATeam`,
   >   `TestTheCLIMintsATeamTokenOnlyForATeam`,
   >   `TestASecondTeamHostConnectionWaitsForAManager`,
   >   `TestATeamHostReconnectingWhileOfflineIsTakenAtOnce`,
   >   `TestTwoHeldAttemptsArmNothingUntilOneIsChosen` (`internal/mirror`;
   >   rewritten for codes in #688, which deleted the address tests
   >   `TestAConfirmForOneAddressDoesNotLetAnotherIn`,
   >   `TestAHeldMachineIsToldItIsHeld`,
   >   `TestAnAttemptFromTheLiveAddressCannotBeConfirmed`,
   >   `TestTheReplaceConfirmNamesOneHeldMachine` and
   >   `TestTheCardCannotConfirmAMachineAtTheLiveAddress`);
   >   `TestDoctorTellsAHeldMachineItIsHeld` (`internal/api`);
   >   `TestRemovingAnOperatorMarksTheTeamsHostTokensRotate`,
   >   `TestDowngradingAnOperatorToViewerMarksTheTeamsHostTokensRotate`,
   >   `TestDisablingAnOperatorMarksTheTeamsHostTokensRotate`,
   >   `TestTheRotateReasonIsReadFromTheTeamsAudit`,
   >   `TestTheTeamArmKeepsThePersonRule` (`internal/users`);
   >   `TestEveryMirrorToolIsClassifiedForATeamHost`,
   >   `TestATeamHostIsListedOnlyItsKeptTools`,
   >   `TestATeamHostRefusesEachRefusedToolAtTheCall`,
   >   `TestATeamHostCannotAnswerATeamAskThatIsNotItsOwn`,
   >   `TestATeamTokenIsNotServedRemotely`,
   >   `TestNoMirrorDoorAgentReaderIsReachableByATeamToken`,
   >   `TestATeamHostsQuestionIsWatchedAsTheTeam`,
   >   `TestATeamHostOpensNoEpisodeWhileTheStateArmIsOff`,
   >   `TestTheTeamHostsPageIsTheManagersAndRunsAsTheTeam` (`internal/api`);
   >   and the ledgers `TestOnlyTheAgentDoorsAdmitATeam` (`ViaMirrorHost` →
   >   true), `TestTheTeamActingViasAreExactlyTheListedDoors` (12),
   >   `TestEveryPerOwnerPrincipalSiteIsTeamSafe` (`askPrincipal` acts; 10
   >   act, 4 refuse).
3. **2b.3: viewers.**
   - `access()`, the new `VisibleHosts` and its caller ledger (`Hosts` stays
     own-only), the read routes and `mirrorTeamRoutes`.
   - SSE team subscriptions, the membership sweep on streams, and the sidebar
     groups.
   - The session-state pass's team arm, with no classifier. Waiting and done
     pushes then reach operators and managers, and the 2b.2 episode branch is
     removed.
   - The model-backed extras are **off** on team sessions (G4):
     - the chief panel, proposals, succession and suggested-reply routes
       answer 403;
     - the focus refresher and the nudge sweep stay persons-only;
     - the state pass runs no classifier for a team.
   - A personal host that reconnects with a team token starts the team's view
     empty. Nothing of its history moves (G5). This slice's docs say so, and
     recommend two daemons.

   > **As built in 2b.3 (no migration).**
   > - **The chokepoint** (`internal/mirror/service.go`). `access(ctx, need)`
   >   decides every owner-taking Service method: the request's TARGET
   >   (`mirror.ForOwner`; none = the principal's own) when the principal is
   >   that owner, or holds `need` in it through `Principal.TeamScope`, and
   >   `need` is one `teamNeedsOpen` opens — viewer only in 2b.3, so an
   >   interactive attach, a paste, an answer or a spawn on a team host is
   >   refused (`ErrNoPrincipal`, no oracle) at every role until 2b.4 adds
   >   operator. 🔴 **Deviation:** D says "every Service method takes the owner
   >   explicitly". The owner rides the context instead (`ForOwner`), set in
   >   ONE place, the route gate below — so ~75 own-only callers and ~20 test
   >   fakes are untouched, and a caller that sets nothing is own-only by
   >   construction. The design's `TestEveryMirrorServiceCallerNamesItsOwn`
   >   is two ledgers instead: `serviceNeeds`
   >   (`TestEveryServiceMethodSaysItsNeed`, `internal/mirror`: every method
   >   that reads its owner, by its need or "own") and `mirrorTargetSetters`
   >   (`TestOnlyTheMirrorTeamGateAimsAMirrorCall`, `internal/api`: the gate
   >   is `ForOwner`'s only caller). ⚠ `role.AtLeast(need)` above viewer is
   >   unreachable while `teamNeedsOpen` is viewer-only; its half of the
   >   membership arm is redundant with `!in` for a non-member (mutation
   >   M2 removes both).
   > - **`Hosts` stays own-only; `VisibleHosts`** adds each team host where
   >   `TeamScope` is viewer and up, and `HostView`/`SessionView` carry
   >   `Owner`. `visibleHostsCallers` (`TestEveryVisibleHostsCallerIsClassified`)
   >   is two: `mirrorEntry` (only for a request the gate aimed at a team) and
   >   `teamMirrorCards` (the sidebar's and switcher's team groups).
   > - **A 2b.2-era bug, fixed here:** `HostRanks` and `LiveTokenHosts` ranked
   >   and listed every token the session reads, and since 0127 a member reads
   >   their teams' tokens (`mirror_tokens` is managed), so a member's own
   >   host icons moved. Both now take the owner's own tokens only
   >   (`TestHostRanksRankOnlyTheOwnersTokens`).
   > - **The routes** (`internal/api/mirror_team_routes.go`).
   >   `mirrorTeamRoutes` classes every `/mirror/`, `/ui/mirror/` and
   >   `/send-queue` route (`TestEveryMirrorRouteHasATeamRule`, against
   >   routes.golden): `mirrorTeamView` (the two pages, the read-only
   >   terminal socket, the Rendered socket, subagents, the question cards,
   >   the scroll), `mirrorTeamRefused` (chief, chief log, proposal,
   >   successor, suggest, suggest event: 403 at every role),
   >   `mirrorTeamOwnOnly` (typing, the queue, the composer's catalog: 403 to
   >   a member until 2b.4) and `mirrorTeamHostDoor` (the token doors).
   >   `mirrorTeamGate` runs in `admit`; a non-member naming a team is 404 on
   >   every route. A member's view is aimed at the team (`ForOwner`) and
   >   registered for the stream sweep. 🔴 **The gate reads a POST body only
   >   on a view route (the scroll):** parsing every POST's form ahead of its
   >   handler spent bodies the handlers bound themselves (it broke the
   >   "too-large" answer path, `TestEveryAnswerPathRecordsOneRow`). An
   >   `owner=` in any other route's body is refused by `mirrorEntry`, which
   >   never resolves an owner the gate did not aim the request at (404) — so a
   >   POST to a refused route with `owner=` only in its body is 404, not 403.
   >   🔴 **Deviation:** the role table gives a viewer "open a preview that
   >   is already running". The Preview button reads the viewer's PERSONAL
   >   viewer plugins, so it is `mirrorTeamOwnOnly` here, and a team session
   >   draws none. **Operator ruling (#690 round 0, Q1, 2026-11-15):** a team
   >   viewer may open it, on the TEAM's plugins, never the viewer's; a team
   >   with none is refused with a sentence. ⚠ **NOT BUILT YET — stopped on a
   >   fork:** no surface mints a team-owned plugin (a plugin is minted by a
   >   person, owned by them), `plugins` is read at operator (0127), not
   >   viewer, and a team-owned plugin's token is refused at `admit`
   >   (`ViaPlugin` is not a team door), so the plugin could not redeem the
   >   hand-off. Building it needs a manager's mint surface, a decision on
   >   that door, and a server-side read of the team's viewer plugins.
   >   **Operator ruling (2026-11-15):** team plugins and team previews are
   >   their OWN slice, built next — after #690 merges, before 2b.4. 2b.3
   >   merges with the preview own-only: a team session draws no Preview
   >   button, and `GET /ui/mirror/preview-button` is `mirrorTeamOwnOnly`.
   > - **The pages.** On a team session the terminal and the Rendered view
   >   leave out everything that is the viewer's own and keyed by host name or
   >   conversation (recents, tags, pins, names, focus history, drafts, the
   >   queue, chief mode, succession, suggestions, the asks block, plugins),
   >   draw the composer disabled (`MirrorTeamComposerNote`), and do not
   >   settle the team's episode on a view (`episodeViewedMirror` is not
   >   called: the stop settles when the window moves on; the design was
   >   silent). The terminal socket refuses live keys before the upgrade
   >   (403). The question cards are the TEAM's (read by the target) and drawn
   >   unanswerable. Every link of a team entry carries `owner=`
   >   (`ui.MirrorRef.Owner`; an own entry's URLs are byte-identical), card
   >   ids include the owner, and a team row offers no pin/tag menu. ⚠ Not
   >   pinned one by one: the page leave-outs, the Rendered socket's
   >   suggestion skip and the home filter (below).
   > - **The sidebar.** Team host sessions are drawn in a group per team, under
   >   the team's name, below the viewer's own rows (`navTeamGroups`), added
   >   after every decorator that reads the viewer's own pins, names, tags,
   >   attention, idle state or succession. Home (`/`) never lands on a team
   >   session.
   > - **Streams follow the membership** (`internal/api/team_live.go`).
   >   🔴 **Deviation:** not `gotrueState.live` but a server-level registry,
   >   `teamLive`, by (team, person) with the role each request needs, so it
   >   also runs in password mode. It replaces 4.7's team-terminal set (a
   >   team agent's terminal is registered at operator in it). A removal or
   >   a role change in this process ends what the new role no longer reaches
   >   at once (`teamMembershipEnded`, `teamRoleSet`); `sweepTeamLive` re-reads
   >   every registered person's memberships in one query
   >   (`users.PGStore.MembershipsAmong`, under System) each
   >   `DisableSweepInterval`, for a removal made elsewhere.
   > - **SSE team subscriptions.** `/events` subscribes to its person and each
   >   of their teams at viewer and up (`streamOwners`; the bus subscribes to
   >   several owners, and the board document's mark reads the same list), and
   >   registers each team for the sweep, so leaving a team ends the stream
   >   and the page reconnects with the teams as they are. A viewer's stream
   >   now carries the team's events (ids only); 2b.1's `teamViewRecipients`
   >   (operators and managers, read per change) is deleted, and
   >   `TestATeamItemRefreshesItsMembersPages` now expects the viewer too.
   > - **The state pass's team arm.** `sessionstate.PG.Owners` adds every
   >   team with an unrevoked mirror token (`chiefnudge.PG.TeamHostOwners`,
   >   under System; users-read ledger `readTeam`). A team's pass runs no
   >   classifier and no live chief judging, whatever chief the team has (G4).
   >   2b.2's "no episode for a team" branch in `attentionMirrorChanged` is
   >   removed. 🔴 **Decision (the design was silent):** a team's AGENTS'
   >   conversations are not episodes in its pass — the arm visits only teams
   >   holding a mirror token, so including them would give team agents
   >   stall pushes only where the team happens to own a host token. ⚠ Not
   >   pinned by a test. **Kept by the operator (#690 round 0, Q2).**
   > - **Tests:** `TestHostsNeverListsATeamHost` (invariant),
   >   `TestAccessReachesATeamHostOnlyAsFarAsTheMembership`,
   >   `TestEveryServiceMethodSaysItsNeed`,
   >   `TestHostRanksRankOnlyTheOwnersTokens` (`internal/mirror`);
   >   `TestATeamSessionWaitingPushesItsOperatorsOnce`,
   >   `TestTheStatePassRunsNoClassifierForATeam`,
   >   `TestATeamHostIsWatchedByItsMembersAndNoOneElse`,
   >   `TestATeamsEventsReachItsMembersStreams`,
   >   `TestARemovedOperatorsTerminalClosesWithinTheSweep` (a read-only
   >   terminal and `/events`; the Rendered socket is registered by the same
   >   gate and is not separately tested), and the ledgers
   >   `TestEveryMirrorRouteHasATeamRule`,
   >   `TestEveryVisibleHostsCallerIsClassified`,
   >   `TestOnlyTheMirrorTeamGateAimsAMirrorCall` (`internal/api`).
   >   `TestATeamHostOpensNoEpisodeWhileTheStateArmIsOff` is removed.
   >   The end-to-end tests above run a REAL mirror plugin with two REAL
   >   `muster-mirror` clients (an own and a team `build-01`) over Postgres.
   > - **The isolation walk's team-host leg** (#690 round 0, D1;
   >   `teamRoleWalks`): the walk's plugin lists a TEAM host, named with the
   >   team's canary, to the team's members only (`isoMirror.teamVisible`,
   >   by `TeamScope`), online for every route. In each of B's roles (none,
   >   viewer, operator, manager) every mirror route is walked again naming
   >   that host (`owner=`): as a non-member B sees its canary on no route
   >   (the main walk's sidebar included) and is answered exactly as for a
   >   host the team does not have; as a member B sees it on at least one
   >   (control). Shown red: a listing that ignores membership puts the
   >   team's canary in a non-member's sidebar (`GET /ui/nav/agents`).
   >   ⚠ The membership decision there is the walk's fake plugin's, as for
   >   every mirror row of the walk; the real plugin's is pinned in
   >   `internal/mirror` (`TestHostsNeverListsATeamHost`, mutant M3).
   > - `teamNeedsOpen` is kept (#690 round 0, D2): it fails closed.
   > - **Round 1 (#690), all fixed:**
   >   - a member who leaves, drops to viewer or is disabled is sent the
   >     withdrawal of the team's shown episode pushes (`withdrawableTeamKinds`
   >     gains "session episodes"; `TestLeavingATeamWithdrawsItsEpisodePushes`);
   >   - a team keeps being visited while it has an open or shown episode, its
   >     last token revoked: its pass then ends every open episode 'gone' and
   >     clears it (`chiefnudge.PG.TeamHostOwners` reports `Live`;
   >     `TestRevokingATeamsLastTokenEndsItsEpisodes`);
   >   - team-ness comes from the owner LIST (`sessionstate.PG.OwnerKinds`):
   >     a team's pass runs as the team acting principal built from its row,
   >     never from an account read, whose failure would build a member
   >     (`TestAFailedAccountReadNeverRunsATeamsPassAsAPerson`). Two halves
   >     say "team" (the principal and the pass's flag); either alone keeps
   >     the classifier off, so mutating one survives and both together dies;
   >   - a team episode's tap opens the TEAM's window (`owner=`), and its
   >     Attention row is drawn only for the team's operators and managers —
   >     whom its push reaches, never a viewer — named over the team's hosts,
   >     never the member's own same-named one (`teamEpisodeShown`,
   >     `teamHostsOf`; `TestATeamEpisodeOpensTheTeamSessionForItsOperators`);
   >   - `mirrorEntry`'s owner filter is pinned by a manager with her own
   >     `build-01` opening the team's;
   >   - a view and an event stream read the role again after registering, as
   >     the team terminal does (`TestARemovalJustBeforeAViewIsNotMissed`);
   >   - the gate reads the scroll's body under the handler's 4 KiB bound and
   >     answers 413 itself (`TestAnOversizedScrollIsRefusedBeforeItIsRead`);
   >   - `Service.Hosts` gives every entry Owner 0, so an own entry's URLs
   >     carry no `owner=` from any caller (the sidebar's highlight matches).
3b. **2b.3b: team plugins and team previews** (operator ruling,
   2026-11-15: its own slice, after 2b.3 merges and before 2b.4). **Built;
   the orchestrator decided the fork below (ii) and accepted the two
   sign-offs — see "As built in 2b.3b" at its end.**
   - **What exists.** A viewer plugin is a person's: minted on their own form
     (`pluginCreateInput`), owned by them, read at operator under 0127
     (`plugins` is shared, read 2, write 3). A plugin's token is refused for a
     team at `admit` (`ViaPlugin` is neither in `teamAgentDoors` nor in
     `authz.teamActingVias`), and `plugins.PGStore.Authenticate` admits enabled
     persons only (`users.EnabledPersonIDs`). The hand-off mint
     (`POST /plugin-handoffs`) resolves the window over the principal's OWN
     hosts and the plugin under its own principal. The preview gateway
     (`cmd/muster-preview`, `internal/previewgw`) learns each token's owner
     from `GET /plugin/v1/self` and mints host tokens by the owner's
     **login** (`ResolveOwner`), and routes a hand-off to a host tunnel by
     `(owner, host)`.
   - **(a) Mint.** `/teams/{id}/plugins` (GET; POST mint, revoke): managers
     only, a fresh role read (`teamAdminContext`), step-up on mint and revoke,
     audited as the manager (`team.plugin.create`, `.revoke`). The row is
     written AS THE TEAM, capability `viewer` and nothing else (no messages,
     no draft checks, no `deliver_to`); the origin and entry URL pass
     `plugins.ValidViewerLink` as a person's do.
   - **(b) The door.** `ViaPlugin` joins `teamAgentDoors` and
     `teamActingVias` (12 → 13 doors) for a team-owned token, and
     `Authenticate` admits a team through `users.EnabledPersonOrTeamIDs`
     (users-read ledger: `readEnabledPersonOrTeam`). A new ledger,
     `teamPluginRoutes`, classes every `/plugin/v1` route for a team token:
     the redeem and `/self` are kept, every other route is refused at the
     door whatever capabilities the row claims. `githubapp.Admits` stays shut
     there: `ViaPlugin` is not in `teamAppDoors`
     (`TestAdmitsLendsAListedTeamTheAppOnlyAtItsAgentAndManagerDoors`).
   - **(c) The read.** On a team session the server reads the team's live
     viewer plugins AS THE TEAM (the team acting principal), after a fresh
     viewer check of the member (`teamMemberNow`) — never the viewer's own.
   - **(d) The preview.** `GET /ui/mirror/preview-button` becomes
     `mirrorTeamView`; on a team session it draws the button over the team's
     plugins, and with none it draws "This team has no viewer plugin set up;
     a manager adds one on the team's plugins page." The mint takes `owner=`,
     re-checks the member at viewer, resolves the window over the team's
     hosts, and writes the hand-off AS THE TEAM (a viewer's action writing a
     team-owned `plugin_handoffs` row through the team acting principal — the
     ruling's "a team viewer may open it"). Redeem answers `owner_id` = the
     team. With no team plugin the mint is a 404 with the same sentence.
   - **No migration for (a)–(d)** (re-checked against `main` at `2789489a`:
     the newest is 0133): every column and policy above exists. Round 0's
     rotate mark later added 0134 (`plugins.rotate_at`; see the as-built
     note).
   - 🔴 **FORK (stopped here):** muster's half alone does not make a team
     preview open. The gateway resolves a host token's owner by LOGIN, and a
     team has none, so no gateway host token can be minted for a team host,
     and no hand-off for `(team, host)` reaches a tunnel. Either:
     - **(i)** this slice is muster's half only (a–d), and the gateway's team
       owners are the next PR: the Preview button works end to end only
       after it; or
     - **(ii)** this slice also teaches the gateway team owners: `/self`
       answers the owner's kind and team name, and `host-token create`
       takes `team:<id>` (still the gateway admin's assertion, as for a
       person). The team plugin's token goes in the gateway's
       `PLUGIN_TOKENS_FILE` by the gateway's admin, out of band.
     Recommendation: **(ii)**. The gateway change is small and in this repo,
     and without it (i) ships a button that opens nothing on a team session.

   > **As built in 2b.3b (migration 0134, round 0).** Who decided what (#696 round 0):
   > the OPERATOR ruled that a team session's previews run on the TEAM's
   > plugins, never a viewer's, and that this is its own slice, next. The
   > ORCHESTRATOR decided the fork (ii: teach the gateway team owners in this
   > slice) and accepted both sign-offs (a viewer opening a preview writes the
   > hand-off as the team; a team plugin token is served the redeem and
   > `/self` alone).
   > - **The token.** `plugins.PGStore.Authenticate` admits an enabled person
   >   or a team (`users.EnabledPersonOrTeamIDs`; users-read ledger
   >   `readEnabledPersonOrTeam`) and reports `Plugin.OwnerTeam`. `ViaPlugin`
   >   joins `teamAgentDoors` and `authz.teamActingVias` (13 doors), and
   >   `requirePluginToken` refuses a team's token on every route outside
   >   `teamPluginRoutes`' kept two (`POST /plugin/v1/handoffs/redeem`,
   >   `GET /plugin/v1/self`) BEFORE its capability check
   >   (`TeamPluginRouteRefusal`; `TestATeamPluginTokenIsServedOnlyRedeemAndSelf`,
   >   pinned to routes.golden, fails when the set grows or shrinks).
   >   `githubapp.Admits` is untouched: `ViaPlugin` is not in `teamAppDoors`.
   > - **`/self`** answers `owner_kind` (`person` / `team`); a team's
   >   `owner_login` is its name (no separate field: #696 round 0, D1). The
   >   gateway (`internal/previewgw`) parses it;
   >   `ResolveOwner` takes `team:<id>` for a team, matching only an answer
   >   whose owner IS that team, and a login never matches a team's answer
   >   (`TestResolveOwnerNamesATeamByItsID`). The manual
   >   `PLUGIN_TOKENS_FILE` step is in `docs/previews.md` ("Team previews").
   > - **The page** `/teams/{id}/plugins` (`team_plugins_page.go`): the hosts
   >   page's gate (managers, a fresh role read, AS THE TEAM), step-up on
   >   mint and revoke, audited `team.plugin.create` / `.revoke`; the row has
   >   `viewer` and nothing else (`TestOnlyAManagerMintsATeamViewerPlugin`).
   > - **The preview.** `GET /ui/mirror/preview-button` is `mirrorTeamView`.
   >   On a team session the pages and the button read the TEAM's viewer
   >   plugins AS THE TEAM (`teamViewerPlugins` → `teamPreviewPrincipal`,
   >   `ViaPlugin`, withPrincipalLedger), after a fresh viewer read
   >   (`teamMemberNow`); with none the button says `TeamNoViewerPlugin`. The
   >   hand-off mint takes `owner`, resolves the window over the team's hosts
   >   (`teamHostsOf`) and mints AS THE TEAM, refusing any other owner's
   >   plugin (`TestATeamSessionsPreviewRunsOnTheTeamsPlugin`).
   > - 🔴 **Found while building: personal plugin paths read team rows.** A
   >   member's session reads their teams' `plugins` rows (shared, read at
   >   operator) and a manager's writes them, so a team's viewer plugin would
   >   have been a chip of a member's OWN window and listed, revocable and
   >   reconfigurable on their own `/plugins` page. **One owner filter, in the
   >   store** (#696 round 0, D2): `plugins.ownedByPrincipal` (`owner_id =`
   >   the principal) is in every listing and change a page reaches — List,
   >   Revoke, SetDraftChecks, SetDeliverTo, MintHandoff — so a new personal
   >   path is filtered by default; the five open-coded API checks and both
   >   ownership helpers are gone (`TestEveryPersonalPluginPathKeepsThePrincipalsOwn`;
   >   `TestEveryPluginRowReadSaysWhoseRowsItReads` classes every other
   >   `FROM`/`UPDATE`/`INTO plugins` function of `internal/plugins`, and
   >   no `JOIN`, package constant or other package: #696 round 1).
   >   A team's rows are reached only under the team acting principal.
   > - **Round 0 (#696):** `/self` has no `team_name` (D1); the plugins page
   >   answers a no-longer-manager with its own notice (`teamManagerPage`); a
   >   team hand-off is audited `team.plugin.handoff`, naming the member who
   >   opened it (an audit row, not a column). **(b), approved: migration
   >   0134** adds `plugins.rotate_at`, set in the same transactions that mark
   >   the team's host tokens (an operator or a manager removed, dropped to
   >   viewer or disabled; `users` team_hosts.go), and shown on
   >   `/teams/<id>/plugins` with the hosts page's reason
   >   (`TestLeavingATeamMarksItsViewerPluginsRotate`). The slice is
   >   therefore NOT migration-free: 0134 is its one migration.
   > - **Round 1 (#696):** the gateway reads `PLUGIN_TOKENS_FILE` only at
   >   start, so adding or rotating a team's token takes a gateway restart
   >   (docs/previews.md). A removed member keeps a team preview they opened
   >   until its owner cookie expires: for a team owner (`/self` `owner_kind`
   >   `team`) that cookie is **shortened to 15 minutes**
   >   (`previewgw.TeamOwnerCookieTTL`; `TestATeamOwnersCookieIsShort`), and
   >   renewing it goes through muster's Preview button, where the role is
   >   read fresh. The hand-off cap (60 a minute) and the gateway's entry-code
   >   cap (64) are per owner, so per TEAM, shared by its members; every team
   >   hand-off writes a `team.plugin.handoff` row, and the team page shows
   >   the last 25. 0134's rollback revokes the team's plugin rows before the
   >   image rollback (its header). Two guard pins:
   >   `TestATeamPreviewReadsTheMembersRoleFresh` (the fresh role read, not
   >   the request's cached memberships) and
   >   `TestATeamHandoffTakesTheTeamsWindowNotTheMembersOwn` (the window over
   >   the team's hosts alone).

4. **2b.4: operators.**
   - Typing and step-up through `access`.
   - `ViaQueue` becomes team-acting **and** `sendQueueOwnerRefusal` reads
     `queued_by`, in one PR. Either alone is wrong:
     - `ViaQueue` alone delivers a disabled member's send;
     - the check alone is unreachable.

     The red-at-base test names the base's panic, and its fixture bypasses
     the cancelling paths (E).
   - A row queued by the team itself is the team's own. **Changed in round 0
     (#702, D1): refused, failing closed, until a team-acting writer lands
     with its own test** — no path queues one today.
   - `CancelQueuedBy`, and the edit and cancel rules (the new
     `requireTeamManager`).
   - The pane answer, and the device route.

   It needs 2b.3's stream sweep, so that a removed operator's terminal closes.
   **Built (no migration: 0127's `queued_by`, its actor trigger and the share
   class of `send_queue` already carry it). See "As built in 2b.4".**

   > **As built in 2b.4.**
   > - **`ViaQueue` and the delivery check, in one change.** `ViaQueue`
   >   joins `teamActingVias` (14 doors), so `queuedPrincipal` builds the team
   >   acting principal for a team's row and the dispatcher probes and pastes
   >   as the team. `sendQueueOwnerRefusal` now judges `Item.Queuer()`
   >   (`queued_by`, or the owner for a row the system scope or pre-0127 code
   >   wrote): a person's own row keeps its account checks; a member's row in
   >   a team's queue gets the member's account checks AND
   >   `teamOperatorNow` (an enabled person, operator or above, fresh read);
   >   a row whose owner's kind cannot be read, or whose queuer is not its
   >   owner and whose owner is no team, fails closed. 🔴 **Round 0 (#702,
   >   D1):** a team's row queued by the TEAM ITSELF (`queued_by` = the team)
   >   is REFUSED too (`SendQueueTeamQueued`), not passed: no writer makes one
   >   today (the POST route queues as the member; the team-acting writers —
   >   a team chief's host forward, a route reply — are refused for a team
   >   elsewhere), so a pass would open for whichever writer came first with
   >   no check behind it. That writer's change opens it, with its own test. `TestATeamSendIsCheckedAgainstWhoQueuedIt`
   >   changes ben in the database directly (disable; delete his membership)
   >   and, separately, through the real removal (cancel-path words): red at
   >   base with `queuedPrincipal`'s panic, and red on the half-applied mutant
   >   (the check reading the owner) because the disabled member's row is
   >   pasted. `TestEveryPerOwnerPrincipalSiteIsTeamSafe`: `queuedPrincipal`
   >   acts as the team (12 act, 3 refuse).
   >   🔴 **Deviation:** a refused row ends **failed**, not cancelled, as every
   >   other delivery refusal does, and its words name the check without
   >   names: "Not sent: the person who queued it is no longer an operator of
   >   the team that owns this session." (no extra reads at delivery).
   > - 🔴 **Found while building: the queue was read by address alone.**
   >   Since 0127 a member's principal reads (and at operator writes) their
   >   teams' `send_queue` rows, and a team's `build-01` shares its address
   >   with the member's own, so a member's own window's list, head and paste
   >   stamp read the team's rows too, and every `{id}` route reached a team
   >   row with no `owner=` at all. Fixed in the store: `sendqueue.Target`
   >   gains `Owner` (0 = the caller's own), which `targetWhere`, `Enqueue`,
   >   `Retry` (into the row's own owner's queue), `Delete`'s turn carry and
   >   `SentBodies` pin (`TestAMembersOwnQueueNeverReadsTheTeamsSameNamedWindow`,
   >   red at base; `TestATeamsQueueIsAddressedByItsOwner`). In the handlers,
   >   `sendQueueRowRefusal`: a row is acted on only in the queue the request
   >   is aimed at (`windowOwner`: the gate's `owner=`, else the principal's
   >   own), else 404. The send hold, the nonce claim and the queue's turn
   >   stamps are keyed by `windowOwner` too, so a member's Send and the
   >   dispatcher's delivery into one team window meet in one place.
   > - **Typing through `access`.** `teamNeedsOpen` opens operator. The route
   >   table's `mirrorTeamOwnOnly` is now `mirrorTeamOperate` (a member at
   >   operator, aimed and registered for the sweep at operator; a viewer is
   >   403 `MirrorTeamViewerRefusal`), and the terminal socket is
   >   `mirrorTeamViewOrKeys` (operator for `mode=rw`). The step-up is each
   >   handler's own `stepup.Fresh` on the member's own sign-in, and the
   >   plugin's (`AttachWindow`, the paste) again
   >   (`TestAccessReachesATeamHostOnlyAsFarAsTheMembership`,
   >   `TestATeamHostIsWatchedByItsMembersAndNoOneElse`). Opening operator in
   >   `access` reaches only the routes the gate aims: no route aims a spawn
   >   or a close at a team.
   > - **Cancelling on leaving.** `users.CancelQueuedByTx(team, user)` (THE
   >   statement) runs in `removeMemberTx` and in `setMemberTx`'s drop to
   >   viewer; `sendqueue.PGStore.CancelQueuedBy` (team 0 = every team)
   >   delegates to it. A disable and a sign-out everywhere make ONE call,
   >   `sendqueue.PGStore.CancelUser` (`CancelOwner` + `CancelQueuedBy(0, …)`;
   >   round 0, D3), from the server (`cancelQueuedFor`) and the CLI's disable
   >   alike
   >   (`TestLeavingATeamCancelsYourQueuedSendsToIt`,
   >   `TestDisablingAMemberCancelsWhatTheyQueuedIntoTheirTeams`).
   > - **Edit and cancel rules.** `requireTeamManager` (a fresh manager read).
   >   🔴 **Decided (the design named cancel and edit only):** edit, Retry and
   >   Send now are the queuer's alone; Cancel and Delete are the queuer's or
   >   a manager's. The list draws another's row with "Queued by <name>." and
   >   only what the viewer may do to it
   >   (`TestOnlyItsQueuerOrAManagerChangesATeamSend`). A viewer is drawn no
   >   queue at all (`GET /send-queue` is an operator route).
   > - **The pane answer.** An operator's question cards on a team session are
   >   answerable; the ask key is the window's owner (`mirrorTarget`), so the
   >   answer types into the TEAM's picker. A viewer's are drawn unanswerable
   >   (`MirrorTeamAskNote`).
   > - **The device route.** `teamPhoneAskRefusal` runs before the answer
   >   and the dismissal: a team's question needs an operator or a manager
   >   on a fresh read (`teamOperatorNow`), else 403 `not_operator`. A team's
   >   dispatch card is refused by `refuseTeamDispatchCardOnPhone`: inside
   >   the answer path (`teamCardRefusal`), and by name on the dismissal,
   >   which had no such check (⚠ the dismissal's refusal is not pinned by a
   >   test)
   >   (`TestAPhoneAnswersATeamAskAtOperator`: red at base, where a viewer's
   >   answer met row-level security's filtered write and was told the
   >   question was not open).
   > - **The pages.** An operator's composer on a team session posts with
   >   `owner=` in its action, keeps no draft (drafts are keyed by host name)
   >   and has no paperclip (an upload is the uploader's own file);
   >   dictation stays.
   > - **A team chief's other say-so (operator decision 2026-11-15, its own
   >   PR).** On a team's chief, end_session_propose, a text route and
   >   agent_dispatch's gate release take a typed turn on the team dispatch
   >   rule: a fresh typed turn from an operator or above of the team (the
   >   marker is set only then), the role re-read when the chief calls, else
   >   the call's own card path. The base already sent all four uses through
   >   ONE predicate, `ownerTurnFor` (`typerSpeaksForOwner`,
   >   `typerSignedOutSince`); `TestEverySaySoIsTheOneTypedTurnPredicate`
   >   pins it (its `ownerTurnFor` half fails only when a use stops calling
   >   it — round 0 on #703, D2: growth is the safe direction —; the
   >   `typerSpeaksForOwner` half is exact). The gate release is exercised on
   >   a team task (`TestATeamChiefsGateReleaseTakesTheTeamDispatchRule`).
   > - **One wording table (round 0 on #703).** `ownerTurnWords` is THE
   >   translation of an `ownerTurnFor` refusal into the chief's sentence, by
   >   use (gated release, no-task and ungated dispatch, route, end) and by
   >   whose chief it is; agent_dispatch, a route and end_session_propose
   >   call it (`ownerTurnRefusal`; host_dispatch keeps the person's words: a
   >   team's chief is refused a host target first, `agentDispatchTeamHost`).
   >   A gate a member set (`chiefReleaseNotChiefsGate`) is worded for the
   >   team too (round 1). A PERSON's chief
   >   reads each use's sentences unchanged. A TEAM's chief never reads "your
   >   owner": every refusal that said it — a stale sign-in's no-marker
   >   included — is worded for an operator or a manager of the team, with
   >   the use's action and fallback (`teamTurnWords`); the member refusals
   >   (removed or demoted, signed out since) were team words already.
   > - **The team's END CARD (operator decision 2026-11-15, "Operator+, fresh
   >   sign-in").** An operator or a manager of the team ends a team session
   >   from the chief's card, from a signed-in BROWSER with a fresh sign-in;
   >   a viewer, a stale sign-in and a paired phone are refused by name and
   >   the card stays open (`teamEndCardRefusal`; the dispatch card's shape).
   >   The card's proposal is read, decided and accepted AS THE TEAM
   >   (`attentionPrincipal`), so `acceptEndSession` resolves and closes the
   >   TEAM's window — never the tapper's own window of the same address
   >   (`TestATeamsEndCardIsAnOperatorsWithAFreshSignIn`).
   >   🔴 **Found while building:** proposals are read owner by owner, so at
   >   base a member's tap on a team's end card found no proposal and was
   >   recorded as a PLAIN answer — the card closed with nothing ended
   >   (red at base: a stale operator's tap "answered" it).
   >   🔴 **Decided (fail closed):** a team chief's end card for one of the
   >   team's AGENTS is not filed (`endSessionTeamAgent`): stopping and
   >   removing a team agent is a manager's, on the team's agents page (4.5),
   >   and an operator's tap would bypass that. A card filed before this
   >   release is refused on the tap too (`TestATeamAgentIsNeverEndedFromACard`).
   >   An operator's "Not now" declines a team's card as the team
   >   (`TestATeamMemberDeclinesATeamsEndCard`).
   >   Its push carries no answer actions: every proposal card's filing push
   >   is built with none (`proposalCardFiled`), so a phone has nothing to tap
   >   (corrected in round 1: the earlier note said otherwise).
   > - ⚠ **Known, not changed:** an answer typed into a team window's picker
   >   records its `ask_attempts` row as the member's own (the table's
   >   `owner_id` default), keyed by host name, so it shows on the member's
   >   own answers record, not the team's.
5. **2b.5: team officers on team hosts.**
   - The `CheckOwners` change, with a team host officer that declares
     `connectors` refused at boot (G3).
   - `team_owner_read` in use on `/officers/spawns`.
   - The gate-ordering test.
   - Docs: `officers-builtin.md`, `auth.md`'s "team hosts come later" lines,
     and `mirroring.md`'s team host section.

**Rollback.** The order is the reverse of shipping.

- **2b.5, 2b.4, 2b.3:** each rolls back its code alone.
- **2b.2:** refuse team tokens again. Team rows stay intact, and a team
  host's sessions are shown to nobody.
- **2b.1:** its **code** rolls back alone at any time.
- 🔴 **Its migration does not.** `muster_team_hosts_rollback()` (0133) runs
  only after 2b.5 and 2b.2 have been rolled back. While 2b.2 is live, removing
  a member writes `rotate_at`, and dropping that column would make the
  removal fail (D).

### G. Operator decided (2026-11-14)

The operator accepted every recommendation, (a) in each case, as recorded in
commit `5f848980`. Each fork is kept with its rejected option, so that the
reason survives. The items are cited elsewhere as G1–G6.

1. **G1 — Who a withdrawal reaches.** **Decided: (a).** Withdrawals go to the
   team's current operators and managers. A withdrawal sweep goes to a person
   when they are removed or downgraded. There is no new table.
   - Rejected: (b), the exact recipients of each push recorded in a
     `push_fanout(team, tag, user)` table.
   - Why: (a) is correct for everyone who still holds the role, and it reaches
     a removed member through the sweep. A new member gets a withdrawal for a
     push they never had, which is harmless. (b) costs a table that grows with
     every push, for a tighter guarantee no one has asked for.
2. **G2 — `route_reply` on a team host.** **Decided: (a).** It is refused on team
   hosts in 2b.
   - Rejected: (b), a team-owned confirmation card answerable only from a
     manager's paired phone.
   - Why: (b) widens `hostDecisionForAnswer`'s one-owner check
     (`host_stepup.go:257`), which is the gate's core property. It would do
     so for a flow that also needs a team chief routing team plugin messages,
     and that chief is Kubernetes-only and not yet bound to team hosts.
     `route_reply` is re-admitted only by its own later decision, with that
     work.
3. **G3 — A team host officer that declares `connectors`.** **Decided: (a).** It
   is refused at boot until team connector requests have a decider.
   - Rejected: (b), a team manager deciding them. That would share
     `connector_grants` for team owners, against the ruling that connectors
     are never shared.
   - Follow-up: a team officer that needs a connector is its own design. That
     design should also settle the same open question for team **pod**
     officers.
4. **G4 — Model-backed extras on team sessions** (focus titles, suggested
   replies, the chief panel). **Decided: (a).** They are off on team sessions
   in 2b. They come on later only on the team's own credential and the team
   chief.
   - Rejected: (b), running them on the viewing member's own key and chief.
   - Why: (b) would put the team's transcript into a person's model calls,
     which D-1113 #10 refuses for the chief, and the same reasoning applies
     to the others.
5. **G5 — A personal host joining a team: its history.** **Decided: (a).** The
   history stays personal, and the team starts empty.
   - Rejected: (b), a System "move host to team" operation.
   - Why: the host's rows hang off a dozen tables, some of them personal by
     class (`session_tags`, `pins`, `recents`), so a move would be partial by
     design. Two daemons are the supported way to have both.
6. **G6 — The notify switch's scope.** **Decided: (a).** There is one switch per
   team.
   - Rejected: (b), one switch per team and per kind (asks, drafts, session
     waits).
   - Why: (a) is one column. A per-kind switch is a second table, and it waits
     until a member asks for one.
