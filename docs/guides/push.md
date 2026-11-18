# Guide: push notifications (VAPID)

One guide per optional dependency; the list is in
[docs/README.md](../README.md#set-it-up-the-core-first-then-one-feature-at-a-time).
Words used here are in the [glossary](../glossary.md).

## What it adds

A Web Push notification on your phone and browsers when something needs you:
a session waiting, an agent's question, a task ready for review. Which events
are pushed, and the per-event switches: [push.md](../push.md#web-push).

## What it needs

- A **VAPID** key pair, muster's identity towards the browsers' push services.
  The Compose stack generates one on its first start
  ([what muster generated](../quickstart-local.md#what-muster-generated)),
  except on an older install whose `.env` sets `POSTGRES_PASSWORD` (every
  `.env` from before generated secrets does): that install gets no generated
  keys and keeps its own setting (`deploy/docker-compose.yml`, the Web Push
  block).
- **HTTPS** for any device other than the hub's own browser: the
  [Tailscale guide](tailscale.md).
- **On iOS:** muster installed to the Home Screen; iOS allows notifications
  only for an installed app.

## Turn it on

1. Nothing to set in the Compose stack: the generated keys are used. On an
   older install whose `.env` sets `POSTGRES_PASSWORD`, generate a pair
   (`muster-server vapid generate`) and add it to `.env` yourself.
2. On each device: tap **Enable** in muster's header and allow notifications.
3. Optional settings, each a line in `.env`: your own key pair
   (`muster-server vapid generate`), the contact address push services are given
   (`MUSTER_VAPID_SUBJECT`), and your own push hosts
   (`MUSTER_PUSH_EXTRA_HOSTS`). What each does and its default:
   [configuration.md](../configuration.md#muster-server-optional-features);
   how to set them: [features.md](../features.md#push).
   ⚠ A new key pair strands every existing subscription
   ([push.md](../push.md#web-push)).

## Check it

- `muster doctor`: the `push` check passes, and in the feature list (on
  stderr) the `push` line starts with `ON`: `muster doctor 2>&1 | grep -E '^ +ON +push( |$)'`
  ([cli.md](../cli.md#muster-doctor)).
- **Account → Notifications → Send a test notification** on each device.

## Turn it off

- **A device:** **Remove** it under **Account → Notifications**; or turn off
  the event types you do not want there.
- **The server:** the Compose stack hands muster its generated keys whenever
  it generated them (`deploy/docker-compose.yml`, the Web Push block), so it has no off switch of
  its own; remove each device instead. What a server without keys does:
  [push.md](../push.md#web-push).
