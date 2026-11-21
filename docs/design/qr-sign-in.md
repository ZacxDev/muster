# Design: sign in with the muster app (QR code)

The ask: on the sign-in page, choose **Sign in with the muster app**, see a QR
code, scan it with a phone that is already signed in to muster, approve, and
the browser is signed in. This note is the research pass behind the design,
and the design as built. The user-facing description is in
[auth.md](../auth.md#sign-in-with-the-muster-app).

## Operator decisions

- **The phone.** Any signed-in muster on the phone approves: the installed web
  app (PWA) or a phone browser signed in to muster. The native Android app
  ([android.md](android.md)) should open the same URL as an app link later.
  Nothing here waits on it.
- **What approval takes.** A tap, after seeing the details. The phone shows the
  requesting browser (a User-Agent summary), its address and, if muster can
  get it cheaply, its rough location. Then Approve or Deny.
- **The code.** It works once and expires in about two minutes.
- **Which phones may approve.** Only a known device: a browser that signed in
  with a credential before.

## Research

### RFC 8628, the OAuth device grant

[RFC 8628](https://www.rfc-editor.org/rfc/rfc8628.html) is the open standard
for "sign in on one device by acting on another". A device that cannot type
asks the authorization server for a `device_code` (its own secret) and a
`user_code` (§3.2). It shows `verification_uri`, or
`verification_uri_complete`, which already carries the user code and exists
for QR codes. Then it polls the token endpoint:

- It waits `interval` seconds between polls, 5 if none is given.
- `authorization_pending` means keep going.
- `slow_down` means add 5 seconds to the interval, now and from then on.
- `access_denied` and `expired_token` end it (§3.5).

The security considerations that apply here:

- The device code is a high-entropy secret (§5.2).
- The user code is rate-limited against guessing (§5.1).
- The approval page should say plainly that the person is authorizing a
  device, and show what it is, against remote phishing (§5.4).

**How it maps.** muster's flow is the same shape run the other way round. In
RFC 8628 the device that cannot sign in shows a code, and a signed-in person
approves it somewhere else. The same is true here, except the "somewhere else"
is the phone the person already uses for muster, and what gets approved is a
browser session, not an OAuth token:

| RFC 8628 | here |
|---|---|
| `device_code`, the device's secret, polled with | the **browser binding**: a random value in an HttpOnly cookie on the browser that asked; never shown |
| `verification_uri_complete` in the QR | `<public>/auth/qr#<code>`, the code in the URL fragment |
| polling with `interval` | `GET /login/app/status` every 2 s (`interval: 2`) |
| `slow_down` | a 429 with `"error": "slow_down"` and a 10 s interval; the page backs off on any error, up to 10 s |
| `authorization_pending` / `access_denied` / `expired_token` | `pending`/`scanned` · `denied` · `expired` |
| `expires_in` | `expires_in`: 120 s |

The user code (§5.1) has no counterpart. The phone never types a short code:
it opens the full 256-bit code from the QR, or from the link shown under it.
That leaves nothing short enough to guess.

### How Discord, WhatsApp, Telegram and Steam do it

- **Discord** ([desktop](https://docs.discord.food/remote-authentication/desktop),
  [mobile](https://docs.discord.food/remote-authentication/mobile), community
  docs):
  - The browser opens a WebSocket to a "remote auth" gateway and generates an
    RSA-OAEP key pair. It proves it holds the private key by decrypting a nonce.
  - The QR carries `https://discord.com/ra/<fingerprint>`, the SHA-256 of the
    public key.
  - The phone posts the fingerprint and asks the person to confirm. The browser
    is first shown who is approving, then gets a ticket and trades it for a
    token **encrypted to its own public key**.
  - The encryption binds the result to the browser that showed the code, and
    nothing in transit can use the token.
  - It does nothing against an attacker who started the session and relays the
    QR to a victim. That relay is how Discord accounts are stolen in practice
    ([anti-scam write-up](https://discord-antiscam.github.io/scam-docs/scams/qr-code-logging)).
- **WhatsApp Web**
  ([security whitepaper](https://www.whatsapp.com/security/WhatsApp-Security-Whitepaper.pdf),
  "Link Using a QR-Code"):
  - The companion's QR holds its public identity key and a linking secret that
    is never sent to the server.
  - The phone signs the companion's key, and sends the result with an HMAC
    keyed by that secret. Only the screen that showed the QR can use the
    result.
  - The phone asks for a biometric or the device PIN first
    ([FAQ](https://faq.whatsapp.com/1317564962315842)).
- **Telegram** ([QR login](https://core.telegram.org/api/qr-login)):
  - The QR token lives about 30 seconds.
  - The phone's `auth.acceptLoginToken` returns the requesting device's model,
    platform, app, address and region.
  - The token is bound to the client's own encrypted session.
- **Steam**
  ([IAuthenticationService](https://steamapi.xpaw.me/IAuthenticationService)):
  - The QR carries a `client_id`. The `request_id` the browser polls with "must
    not be rendered in QR".
  - Before confirming, the phone fetches the requester's address, location,
    platform and device name and shows them.

The one pattern they all share: **the secret that collects the result is not
the one in the QR**, and the phone shows what is asking before anyone taps.

### Known attacks: QRLjacking, and a study of real deployments

**QRLjacking** ([OWASP](https://owasp.org/www-community/attacks/Qrljacking)):

- The attack: an attacker opens the real sign-in page, copies its live QR code
  onto a page of their own, and gets the victim to scan it. The victim's phone
  approves, and the attacker's browser is signed in as the victim.
- No cryptography stops this. The QR is real, and the victim is the one who
  approves it.
- OWASP's mitigations: show the requesting session's details on the phone,
  short lifetimes, and same-network or same-location checks.

**["Demystifying the (In)Security of QR Code-based Login in Real-world Deployments"](https://www.usenix.org/conference/usenixsecurity25/presentation/zhang-xin)**
(USENIX Security) studied QR login on 109 real sites. 47 of them had 75
flaws, of six kinds:

- **F1, an unbound session (34% of sites).** Anyone holding the QR id can poll
  and collect the result.
- **F2, a reusable QR id.** The "logged in" state is not cleared after use.
- **F3, a predictable QR id.**
- **F4, a client-chosen QR id.**
- **F5, phone-supplied identity.** The server believes a user id the phone
  sends.
- **F6, secrets leaked in the poll response.**

Their advice:

- Generate the id on the server with high entropy.
- Bind it to the session that asked for it, and accept polls only from that
  session.
- Clear the approved state as soon as it is used.
- Authorize only the account the phone's own credential proves.
- Put nothing secret in responses.

| attack or flaw | what stops it here |
|---|---|
| F1 unbound / a code read off a screen, a log or a photo | the redeem needs the code **and** the requesting browser's binding cookie (HttpOnly, `SameSite=Strict`, only its sha256 stored); the status poll reads only by the binding |
| F2 reusable | approved → redeemed is one conditional update under a row lock; a second redeem, even a concurrent one, is refused |
| F3/F4 predictable or client-chosen | 32 random bytes, minted by the server |
| F5 phone-supplied identity | the approver is the session's principal (`requireSession`), never a field; the issued session is checked to be that user's |
| F6 leakage | the poll answers a state and an interval; the approver's identity and the session are only ever set as cookies on the redeem |
| QRLjacking | the phone shows the browser, its address and the phone's own address side by side, says "Approve only if you started this yourself, on a screen in front of you", and the code lives 2 minutes |
| login CSRF (a sibling page starts or spends a sign-in in your browser) | every POST checks Origin, like the other sign-in doors |
| a stolen session cookie approving | the approving browser must also be a **known device** (below) |

**What it does not stop.** Like Discord, WhatsApp and every app-approval
scheme, this flow cannot tell a relayed QR from a real one. A person who
approves a sign-in they did not start has signed someone in. The defences are
the details, the warning and the short lifetime. Proximity is FIDO's answer,
below. muster has no geolocation, and the decision was to skip the location
if it needs a new external service, so the phone shows addresses but no place.

### FIDO's "use a passkey from my phone" (hybrid)

muster already offers **Use a passkey from my phone** on the gotrue sign-in
page. That is FIDO's cross-device "hybrid" transport, formerly caBLE
([CTAP 2.2 §11.5](https://fidoalliance.org/specs/fido-v2.2-ps-20250714/fido-client-to-authenticator-protocol-v2.2-ps-20250714.html#sctn-hybrid)):

- The browser shows a QR code, the phone scans it, and the two prove they are
  near each other over **Bluetooth LE** before the phone's passkey signs.
- That proximity check is exactly what defeats the relayed QR above: an
  attacker would need a Bluetooth radio next to the victim
  ([proximity draft](https://fidoalliance.org/specs/hybrid/proximity-exchange-protocol-v1.0-wd-20260717.html),
  [Google](https://developers.google.com/identity/passkeys/use-cases)).

It is the stronger mechanism when it works, so it stays, and it stays first.

It needs four things the app flow does not:

- Bluetooth on, on both machines. Many desktops and VMs have none, or have it
  off.
- A passkey for this muster on the phone.
- Browser and OS support on both ends.
- gotrue mode. Password mode has no passkeys at all.

The app flow works in both modes, on any desktop, for anyone signed in on a
phone. In exchange it gives up proximity, which is why every defence in the
table above matters.

### Issuing a GoTrue session without a password

The hard part in gotrue mode: muster never mints sessions itself. GoTrue
issues them, from a password, a passkey, or a recovery token
([accounts](../auth.md#gotrue)). The options:

- **muster signs its own access token.** It holds the HS256 secret
  (`AdminSignOutEverywhere` already signs a short-lived one to call
  `/logout`). But that token has no refresh token and no GoTrue session
  behind it: a new token format. **Rejected.**
- **A magic link, used server-side.** GoTrue's admin
  [`/admin/generate_link`](https://github.com/supabase/auth/blob/master/internal/api/mail.go)
  with `type: magiclink` returns a `hashed_token` and sends no email. muster
  exchanges it at
  [`/verify`](https://github.com/supabase/auth/blob/master/internal/api/verify.go)
  for an ordinary GoTrue session. This is exactly what the password reset does
  with `type: recovery`. **Chosen.**

Measured against GoTrue v2.197.0, the deployment's pin:

- The exchange returns a session with a refresh token, `amr: otp`, for the
  right subject. The token is single-use: a replay is refused with 403
  `otp_expired`.
- Two mints back to back both succeed. The admin path has no send-frequency
  limit, which matches the source: `SMTP_MAX_FREQUENCY` is checked only where
  an email is sent.
- **A magic link and a recovery link share one slot per user.** Minting either
  ends an outstanding one of the other. So signing in with the app ends a
  password-reset link an admin handed that user and they have not used yet. A
  new link is one click on the admin page, so this is accepted, and auth.md
  says so.
- A banned (disabled) user gets no session: 403 `user_banned`.

Why this is safe:

- The token is minted for the email GoTrue holds for the approver's own login
  (`gotrue_sub`). It is exchanged in the same request and never leaves muster.
- The subject of the session that comes back is compared with that
  `gotrue_sub`. On a mismatch, muster ends the session and signs nothing in.
- The session then passes the same admission every sign-in does
  (`admitFreshSession`).

### The QR code library

No QR encoder was in `go.mod`. The candidates, all pure Go:

- `rsc.io/qr`: BSD-3, no dependencies, but unreleased for years.
- `skip2/go-qrcode`: MIT, no dependencies, but no tagged release and unchanged
  for years.
- `yeqown/go-qrcode`: MIT, active, but it pulls in `x/text` and more.
- `boombuler/barcode`: MIT, no dependencies, still released (v1.1.0).

**Chosen: `boombuler/barcode`**, which is maintained, has no dependencies and
is permissively licensed. muster reads its module grid and draws the SVG
itself, one `<path>` with a white quiet zone (`qrSVG`).

## The design, as built

**The table.** `signin_requests` (migration 0173) holds one row per request:

- the sha256 of the **code** (in the QR) and of the **binding** (in the
  browser's cookie);
- the state: `pending` → `scanned` → `approved` | `denied`, then `approved` →
  `redeemed`. Expiry is not a state: every transition checks `expires_at` on
  the database's clock;
- the requester's address and User-Agent summary;
- the approver, and the **sign-in time of the session that approved**.

It has no owner, because a request is anonymous until a phone approves it. It
is under forced row-level security with a `scope_all` policy only, and
`internal/signinreq` reaches it through its one system-scope helper.

**The browser** (`/login` in both modes):

- **Sign in with the muster app** posts `/login/app`. muster creates the
  request, sets the binding cookie and answers with the code, the link, the QR
  as SVG, `expires_in` and `interval`.
- The page draws the QR, the link under it for a phone with no camera, and a
  status line: waiting → scanned, approve on your phone → approved, signing you
  in. Denied or expired shows **Get a new code**.
- It polls `/login/app/status` and, once approved, posts `/login/app/redeem`
  with the code. The cookie goes with it.
- A redeem in password mode sets the operator session. In gotrue mode it gets
  a GoTrue session for the approver as above.
- The session it sets does **not** mark the browser a known device, so an
  approval cannot create another approver.

**The phone** (`/auth/qr#<code>`):

- The page moves the fragment into a form and posts it, so the code never
  appears in a path, a query or the access log. A hand-made `/auth/qr/<x>` is
  cut from the log too.
- With no session, it shows **Sign in** with a link to
  `/login?next=/auth/qr%23<code>`, and the sign-in returns there with the
  fragment intact.
- With a session, the request goes through `requireSession` and the origin
  check. The browser must be a **known device**, and then the details card
  shows the browser, its address, the phone's own address, and **Approve** /
  **Deny**.
- Each decision is written to the audit log (`signin.app.approve`,
  `signin.app.deny`, `signin.app.redeem`) and to the server log.

**Known devices.** Password mode already sets `muster_device` on every
operator-password sign-in. gotrue mode now sets it too, on every sign-in that
proved a credential: password, passkey, claim, invite, reset. It is signed
with a key derived from `MUSTER_GOTRUE_JWT_SECRET` **and the user's id**, so it
vouches for one person in one browser. A session alone, whether a copied cookie
or one this flow issued, cannot approve.

**An approval dies with its session.** The redeem holds the approver's
credentials lock and refuses when the approver is disabled, or when the
approving session's sign-in is at or before the account's session cutoff or
latest Sign out everywhere. That is the rule a paired phone is held to.

**Throttles**, all per client address:

- 10 requests started a minute, and 300 across the whole server.
- 10 failed look-ups or redeems a minute, counted after the look-up, so a live
  code is never refused for its address.
- 90 polls a minute, then `slow_down`.

## The Android app

The app is a separate repository and nothing here waits on it. On the server,
`/auth/qr` is an ordinary signed-in page, so the app's WebView, signed in with
its own known-device cookie, can approve with no new API. For the app to open
the QR's link itself rather than the phone's browser:

- the app declares an `autoVerify` intent filter for `https://<host>/auth/qr`
  and loads the URL, fragment included, in its WebView;
- muster's `/.well-known/assetlinks.json` then needs the
  `delegate_permission/common.handle_all_urls` relation beside
  `get_login_creds`. It is deliberately absent today
  ([android.md](android.md), "the app handles no links"), and adding it is a
  one-line change in `internal/api/assetlinks.go` to make together with the
  app's intent filter.
