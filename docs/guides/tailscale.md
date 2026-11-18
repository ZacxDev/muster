# Guide: Tailscale (your phone and other machines)

One guide per optional dependency; the list is in
[docs/README.md](../README.md#set-it-up-the-core-first-then-one-feature-at-a-time).
Words used here are in the [glossary](../glossary.md).

## What it adds

muster's core runs on one machine at a `http://localhost` address. Anything
that is not that machine's own browser needs HTTPS: your phone (the installable
app, notifications, the microphone) and the sessions of your other machines.
[Tailscale](https://tailscale.com) gives the hub an HTTPS address on your own
private network, reachable only by your own devices
([what works on localhost](../quickstart-local.md#what-works-on-localhost-and-what-needs-https)).

## What it needs

- Tailscale on the hub, on your phone and on each other machine, all signed in
  to the **same** tailnet.
- **MagicDNS** and **HTTPS Certificates** turned on in the Tailscale admin
  console.
- Nothing from muster's side beyond the core.

## Turn it on

1. **The hub:** `tailscale serve` forwards HTTPS to muster's local port, and
   the hub's `.env` gets that address as `MUSTER_PUBLIC_URL`
   ([configuration.md](../configuration.md#muster-server-core)). The exact
   commands are in [quickstart-local.md, "Add your phone"](../quickstart-local.md#add-your-phone)
   (by hand) and [INSTALL.md, "Add your phone and other machines (Tailscale)"](../../INSTALL.md#step-3-add-your-phone-and-other-machines-tailscale)
   (with an agent; it leaves any other `tailscale serve` mapping alone).
2. **The phone:** open the address, sign in, install muster to the home screen
   (on iOS, always open it from that icon), then turn on
   [push](push.md).
3. **Each other machine:** it needs its own mirror token, minted on the hub,
   then the installer in join mode:
   [INSTALL.md, "Another machine"](../../INSTALL.md#another-machine). With an
   agent, paste the same one prompt ([onboarding.md](../onboarding.md)) into
   the agent on that machine and say muster already runs.

## Check it

- `muster doctor` on the hub: the `server` check passes over **https**, and
  with `qrencode` installed it draws the phone's QR code.
- `muster doctor` on a joined machine: `this-host` passes; on the hub,
  `mirror-hosts` lists it ([cli.md](../cli.md#muster-doctor)).

## Turn it off

- **A machine:** stop its `muster-mirror` service and revoke its mirror token on
  the hub ([INSTALL.md, "Updating and removing"](../../INSTALL.md#updating-and-removing)).
- **The hub's HTTPS address:** remove muster's `tailscale serve` mapping, and
  only it, as "Updating and removing" shows; then take `MUSTER_PUBLIC_URL` out
  of `.env` and run `docker compose up -d --wait`. muster is back on this
  machine's browser only.
