# Guide: dictation (speech-to-text)

One guide per optional dependency; the list is in
[docs/README.md](../README.md#set-it-up-the-core-first-then-one-feature-at-a-time).
Words used here are in the [glossary](../glossary.md).

## What it adds

A microphone on every message box: speak, read the transcript in the box, then
send it. [dictation.md](../dictation.md).

## What it needs

A speech-to-text service that answers OpenAI's audio transcription API. Pick one
([choosing a service](../dictation.md#choosing-a-speech-to-text-service)):

1. **Bundled, CPU:** the `stt` Compose profile, a faster-whisper server. No
   GPU; it downloads a model on its first start, so allow disk for it.
2. **Bundled, GPU:** the `stt-cohere` profile. More accurate; needs an NVIDIA
   GPU, the NVIDIA container toolkit, amd64, and a Hugging Face token whose
   account accepted the model's licence.
3. **Your own API key** for any OpenAI-compatible service. No container; your
   recordings go to that provider.

On a phone the microphone also needs HTTPS: the [Tailscale guide](tailscale.md).

## Turn it on

In the hub's `.env`, then `docker compose up -d --wait`:

- **Option 1:** `COMPOSE_PROFILES=stt` and `MUSTER_STT_TOKEN=<a random secret>`.
  With only the token set, muster points itself at the bundled service.
- **Option 2:** `COMPOSE_PROFILES=stt-cohere`, `MUSTER_STT_URL`,
  `MUSTER_STT_TOKEN`, `MUSTER_STT_MODEL` and `HF_TOKEN`.
- **Option 3:** `MUSTER_STT_URL`, `MUSTER_STT_TOKEN` (your key) and
  `MUSTER_STT_MODEL`; no profile.

The exact values for options 2 and 3 are in
[features.md](../features.md#dictation); what each variable means, in
[dictation.md](../dictation.md#configuration). A profile goes on the one
`COMPOSE_PROFILES` line, beside any you already have.

## Check it

- `muster doctor`: the `dictation` check sends a bundled spoken sample through
  muster and expects text back; the feature list reads `ON dictation`
  ([cli.md](../cli.md#muster-doctor)).
- `docker compose logs muster | grep dictation:` names the service and model.

## Turn it off

1. Remove `MUSTER_STT_TOKEN` (it is what turns dictation on) and the other
   `MUSTER_STT_*` lines from `.env`, and the profile from `COMPOSE_PROFILES`.
2. `docker compose up -d --wait`. No microphone is drawn.
3. Compose does not stop a service whose profile you took out (the same holds
   for the chief: [features.md](../features.md#chief)): remove it with
   `docker compose --profile <profile> rm -sf <service>` (the services are
   named in `deploy/docker-compose.yml`).
