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

## By hand, from a clone

```sh
git clone https://github.com/ZacxDev/muster && cd muster
docker compose -f deploy/docker-compose.yml up -d --wait
docker compose -f deploy/docker-compose.yml exec muster /muster-server show-secret ui-password
```

Open `http://localhost:8105` and sign in with that password. A clone runs the
image built from the newest commit on `main`; a release's own
`docker-compose.yml` runs that release.

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

## What muster generated

The first start runs a one-shot, `muster-init`, that generates every secret
the stack needs and keeps it on the `secrets` volume: the sign-in password,
the hook token (the admin credential of muster's machine API), the push
notification keys and the database password. The sign-in password is printed
once, in `docker compose logs muster-init`; show it again any time with
`… exec muster /muster-server show-secret ui-password` (or `hook-token`).

To set any of them yourself, put the line in a `.env` beside
`docker-compose.yml` (see `deploy/.env.example`) and run `up -d --wait` again:
a value in `.env` always wins. `docker compose down -v` deletes the database
**and** the generated secrets.

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
