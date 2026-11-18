# Guide: per-user sign-in (GoTrue)

One guide per optional dependency; the list is in
[docs/README.md](../README.md#set-it-up-the-core-first-then-one-feature-at-a-time).
Words used here are in the [glossary](../glossary.md).

## What it adds

Per-user accounts in place of the core's one shared password: passkeys,
invites, admin and member roles, each person's rows kept apart by row-level
security. [Teams](../features.md#teams) build on it. [auth.md](../auth.md).

## What it needs

- **A [GoTrue](https://github.com/supabase/auth) server you run yourself**
  (Supabase's auth server), reachable from the muster container and issuing
  tokens as [auth.md](../auth.md#gotrue) describes. The Compose stack does not
  include one; [auth.md](../auth.md#a-gotrue-server-beside-the-compose-stack)
  has a `docker-compose.override.yml` that adds one beside it, with its `.env`
  lines.
- **A second listener for browsers.** In this mode only `MUSTER_BROWSER_PORT`
  serves browser sessions; the main port serves machine tokens only. Pick a
  port nothing else on the hub uses (the chief's connect port included).
- `MUSTER_PUBLIC_URL` for invite links and passkeys, set to the **browser
  listener's** address: `http://localhost` on that port on this machine only,
  or the `https://` address of the [Tailscale guide](tailscale.md) forwarded to
  it. The main port refuses a browser in this mode.

## Turn it on

1. In the hub's `.env`: `MUSTER_AUTH_MODE=gotrue`, `MUSTER_GOTRUE_URL`,
   `MUSTER_GOTRUE_JWT_SECRET`, `MUSTER_BROWSER_PORT`, and the client topology:
   `MUSTER_DIRECT_CLIENTS=1` when browsers reach the port directly, or
   `MUSTER_TRUSTED_PROXIES` with `MUSTER_CLIENT_IP_HEADER` for a proxy in front
   (muster refuses to start without one; [auth.md](../auth.md#gotrue)).
   With `tailscale serve` in front (as below), it is the proxy: muster's log
   line `client address: ignored … from <address>` names the address to trust
   and the headers that arrived.
   ([features.md](../features.md#accounts) has the lines;
   [configuration.md](../configuration.md#muster-server-sign-in-and-machine-credentials)
   what each requires).
2. Publish the browser port with a `docker-compose.override.yml` beside
   `docker-compose.yml`, and point `tailscale serve` at that port instead of the
   main one ([features.md](../features.md#accounts) shows the file).
3. `mi_dc up -d --wait --quiet-pull` (INSTALL.md's helper, which reads the
   override; a `docker compose -f docker-compose.yml` without it drops the
   browser port).
4. Bind your GoTrue login to the admin:
   `docker compose exec muster /muster-server invite --claim-admin` prints a
   single-use link ([auth.md](../auth.md#setting-up-the-first-admin)). The
   claim link also works before you switch modes, so you can bind it first.
5. Invite everyone else on `/admin/users`
   ([auth.md](../auth.md#users-roles-and-invites)).

## Check it

- `muster doctor`: in its feature list (on stderr), the `accounts` line
  starts with `ON`: `muster doctor 2>&1 | grep -E '^ +ON +accounts( |$)'`
  ([cli.md](../cli.md#muster-doctor)).
- `docker compose logs muster | grep -E 'human web tier:|gotrue:'` reads
  `GOTRUE` and `CONFIGURED`.
- This mode is not exercised by the Compose stack's own tests; check your
  first sign-in by hand.

## Turn it off

1. Remove `MUSTER_AUTH_MODE` (password mode is the default), and point
   `tailscale serve` back at the main port.
2. Delete the `docker-compose.override.yml` **and** the `GOTRUE_*` /
   `MUSTER_GOTRUE_*` / `MUSTER_BROWSER_PORT` lines in `.env`, and set
   `MUSTER_PUBLIC_URL` back to the main port's address. Both halves: an
   override left behind without its `.env` lines does not parse (the one in
   [auth.md](../auth.md#a-gotrue-server-beside-the-compose-stack)), and
   `install.sh` refuses to update until it is gone.
3. `mi_dc up -d --wait --remove-orphans` (stops a GoTrue the override ran).
   Everyone signs in with the one shared password again, as the admin.
