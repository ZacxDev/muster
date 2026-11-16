# Try muster on this machine

muster runs on your own machine first, at `http://localhost:8105`, with no
account anywhere and no `.env` to fill in. Your phone comes later, in one step:
[Add your phone](#add-your-phone).

You need Docker with Compose v2 and tmux, on Linux or macOS (amd64 or arm64).

## One line

```sh
curl -fsSL https://github.com/ZacxDev/muster/releases/latest/download/install.sh | sh
```

It starts the server in Docker (in `~/muster`), installs `muster-mirror` and
the `muster` CLI in `~/.local/bin`, runs `muster-mirror` as a user service so
this machine's tmux windows show up, and finishes with `muster doctor`. It
prints the address and the sign-in password at the end.

It is safe to run again: it keeps the password, the mirror token and any line
you added to `~/.config/muster-mirror/env`. It refuses, and changes nothing,
when it finds a muster it did not install (another `~/muster`, another compose
project of the same name, something already on the port, or muster-mirror
settings someone else wrote).

Then start an agent in tmux, for example `tmux new -A -s work claude`, and
open the address.

**After a reboot.** The server comes back on its own (Docker restarts it).
`muster-mirror` is a user service, and a user service runs only while you are
logged in unless your user has *linger* on. The installer turns linger on with
`loginctl enable-linger` (it asks for `sudo` when your system requires it, and
says so if it could not). Without linger the mirror stops when you log out and
does not start at boot, so this machine's sessions stay missing from muster
until you log in again. Turn it on yourself any time:

```sh
sudo loginctl enable-linger "$USER"
```

(macOS: the installer uses a launchd agent, which starts when you log in.)

## By hand, from a clone

```sh
git clone https://github.com/ZacxDev/muster && cd muster
docker compose -f deploy/docker-compose.yml up -d --wait
docker compose -f deploy/docker-compose.yml exec muster /muster-server show-secret ui-password
```

Open `http://localhost:8105` and sign in with that password. A clone runs the
latest release: `deploy/docker-compose.yml` names it, and the release flow
opens the pull request that bumps it right after each release (CI warns for 3
days, then fails, while it is behind). A release's own `docker-compose.yml`
runs that release. Set `MUSTER_VERSION` in `.env` to run another — not one
older than the file's `x-muster-min-version`, which cannot start it.

To show this machine's tmux windows, mint a token for it and run
`muster-mirror` (from the release's `muster-mirror_<os>_<arch>.tar.gz`):

```sh
mkdir -p ~/.config/muster-mirror
docker compose -f deploy/docker-compose.yml exec -T muster /muster-server mirror token create "$(hostname -s)" > ~/.config/muster-mirror/token
muster-mirror -url http://localhost:8105 -token-file ~/.config/muster-mirror/token
```

Plain `http://` is accepted to this machine only (`localhost`, `127.0.0.1`,
`[::1]`): the token never leaves it. To any other address muster-mirror wants
`https://`.

### This machine is yours alone

The localhost setup assumes **you are the only person logged in to this
machine.** Plain `http://localhost` is private only from other machines, not
from other users of this one:

- whoever holds port 8105 receives what muster-mirror sends. While muster is
  down, another local user could start a listener on `127.0.0.1:8105` and
  receive this machine's mirror token (its `Authorization: Bearer` header) on
  the mirror's next connect;
- the sign-in password is shown in `docker compose logs muster-init`, which
  anyone in the `docker` group can read (they can also read the secrets volume
  itself: Docker access is root on the machine).

On a shared machine (a lab box, a multi-user server) put muster behind HTTPS
with [Add your phone](#add-your-phone) and point muster-mirror at that
`https://` address instead. `muster doctor` warns when it sees other users
logged in.

## What muster generated

The first start runs a one-shot, `muster-init`, that generates every secret
the stack needs and keeps it on the `secrets` volume: the sign-in password,
the hook token (the admin credential of muster's machine API), the push
notification keys and the database password. The sign-in password is printed
once, in `docker compose logs muster-init`; show it again any time with
`… exec muster /muster-server show-secret ui-password` (or `hook-token`).

⚠ That log line stays in `muster-init`'s container log until the container is
removed (`docker compose rm muster-init`, or `down`). If you ship Docker logs
somewhere else (a log driver, Loki, a hosted service), the sign-in password
goes with them: change it (set your own in `.env`, below) or keep those logs
private.

To set the sign-in password or the hook token yourself, put the line in a
`.env` beside `docker-compose.yml` (see `deploy/.env.example`) and run
`up -d --wait` again: a value in `.env` is used instead of the generated one,
on every start. **The database password is different**: see the next section.
`docker compose down -v` deletes the database **and** the generated secrets.

### The database password

Postgres reads its password only when it first creates the database. So the
stack keeps the first one it had (the `.env`'s `POSTGRES_PASSWORD` on that
first start, else a generated one), and **never** changes it from `.env`
afterwards. A `POSTGRES_PASSWORD` in `.env` that differs from the kept one
stops `up --wait` on `muster-init`, which changes nothing and says so. Either:

- **remove `POSTGRES_PASSWORD` from `.env`** (the usual fix: it came from an
  old example) and run `up -d --wait` again; or
- **change the database's password to it**, in this order, with the variable
  still removed and the stack up:

  ```sh
  # 1. the database itself, through Postgres's local socket (no password needed);
  #    psql asks for the new password twice (psql's \password runs ALTER USER for you)
  docker compose exec postgres psql -U muster -d muster -c '\password muster'
  # 2. drop the kept copy (the volume is <project>_secrets; the project is `muster` unless you set another)
  docker run --rm -v muster_secrets:/s --entrypoint rm postgres:16-alpine /s/postgres_password
  # 3. put POSTGRES_PASSWORD=the-new-password back in .env, then
  docker compose up -d --wait && docker compose restart muster
  ```

  Step 3 keeps the new password and gives muster a `DATABASE_URL` built from it.

  `\password` quotes the password itself, so any character is safe. If you
  write `ALTER USER muster PASSWORD '…'` by hand instead, a single quote in
  the password must be doubled (`o''brien`), or the statement breaks.

**An install from before generated secrets** (its `.env` set
`POSTGRES_PASSWORD`, which created its database) keeps working unchanged: on
its first start of this compose file `muster-init` keeps that password. If
that `.env` no longer sets `POSTGRES_PASSWORD` by then, `up --wait` stops on
`muster-init` with *"this stack's database already exists, but its password
was never kept here"*, and changes nothing: no password is generated for a
database that already exists, because it could not log in. Put the line back
in `.env` with the value the database was created with (your old `.env`'s) and
run `up -d --wait` again; it is kept from then on and the line can go. Lost
it? Set a new one with step 1 above, then put that value in `.env`.

## What works on localhost, and what needs HTTPS

On this machine's browser the core works: browsers treat `http://localhost`
as a secure origin, and muster sets its sign-in cookie there so every browser
keeps it (CI signs in on `http://localhost` with Chromium and WebKit, Safari's
engine). Push is on as well: its keys are generated with the rest.

What needs HTTPS is anything that is not this machine: your phone (installing
the app, notifications, the microphone), and other machines' sessions.
`muster doctor` says so on its `server` line, and draws no phone QR code for a
localhost address.

## When it does not start

A setting muster refuses (in your `.env`) stops `up --wait` within seconds,
on `muster-init`. The reason is the last line of:

```sh
docker compose -f deploy/docker-compose.yml logs muster-init
```

## Add your phone

HTTPS for your phone comes from [Tailscale](https://tailscale.com): a private
network between your devices, with a real certificate for this machine's name.

1. Install Tailscale on this machine and on your phone, and sign in to the same
   account on both. In the [admin console](https://login.tailscale.com/admin/dns),
   turn on **MagicDNS** and **HTTPS Certificates**.
2. On this machine: `tailscale serve --bg 8105`. It prints this machine's
   `https://<name>.<tailnet>.ts.net` address.
3. Put that address in the `.env` beside `docker-compose.yml`
   (`MUSTER_PUBLIC_URL=https://…`) and run `docker compose up -d --wait` again.
4. Open the address on your phone, sign in, and add muster to the home screen.

To connect another machine's sessions too, follow [INSTALL.md](../INSTALL.md)
on it: it joins this server over the same HTTPS address.
