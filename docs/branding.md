# Instance branding

An admin can give a muster server its own logo and display name. muster then
draws them in place of its own mark:

- in the page header and the sidebar, on every signed-in page;
- on the sign-in page;
- as the favicon and the touch icon, on every page muster serves (the
  sign-in, invite, reset and claim pages included);
- as the installed app's icons (the web app manifest);
- as the icon of every Web Push notification;

(the last three are the logo's icons, or an [app icon](#an-app-icon-of-its-own)'s
when one is set);
- in [`GET /api/instance`](#the-instance-document), which the apps read to show
  which server is which.

There is no environment variable for it. Branding is set in the UI and kept in
the database, so a server without a database has none.

With nothing set, every surface is exactly what it was before branding existed:
the `muster` wordmark and muster's own icons.

## Setting it

Open **Account** as an admin. The **Instance branding** section has:

- **Display name**: optional, up to 64 characters. Surrounding spaces are
  trimmed. Control characters and text-direction overrides are refused.
  Save an empty name to clear it.
- **Logo**: a PNG, JPEG or WebP file, up to 1 MiB, and at most 4096 pixels on
  each side and 4,194,304 pixels (2048 × 2048) in all, so a 4096 × 1024
  banner fits. **Save branding** uploads it.
- **App icon**: optional, the same kinds of file under the same limits. It
  replaces the icons made from the logo ([below](#an-app-icon-of-its-own)).
  Use a square image, at least 512 × 512 pixels.
- **The logo already shows the name (hide the name text next to it)**: tick
  it when the logo is a wordmark ([below](#a-logo-that-spells-the-name)).
  Unticked by default.
- **Reset logo to the muster mark** removes the logo and the app icon, and
  keeps the name and the checkbox.
- **Remove app icon**, shown while one is set, removes the app icon: the icons
  are made from the logo again.

The section shows the current logo and the app icon. A member's
Account page has no such section, and the routes refuse a member.

In password mode the operator (user 1) is the admin. In GoTrue mode it is any
user with the admin role ([auth.md](auth.md)).

## A logo that spells the name

Many logos are wordmarks: they already spell the server's name. Drawn with the
name beside them, the header says it twice. Tick **The logo already shows the
name** and, while a logo is set, the header, the sidebar and the sign-in page
draw the logo alone. The logo's `alt` text is then the name, so a screen
reader still announces it. (With the name drawn beside the logo, the logo's
`alt` is empty, so the name is not read twice.)

This is a setting, not a guess from the image. It changes nothing else:

- the display name is kept, and `GET /api/instance` still returns it, so the
  apps' server switcher still shows it;
- with no logo set the box does nothing, and the name is drawn as text;
- resetting the logo keeps the setting, so it applies again to the next logo.

## An app icon of its own

The favicon, the touch icon, the installed app's icons and the Web Push icon
are square. muster makes them from the logo by scaling it to fit and centring
it on a transparent square. A wide wordmark becomes a thin strip that is hard
to see at 192 pixels, and pale lettering may vanish on a light background.

Upload an **App icon** to use a different image for these icons. It goes
through the same checks and the same re-encoding as the logo
([below](#what-happens-to-an-upload)), and both icons are drawn from it, again
scaled to fit and centred, with nothing cropped. A square image fills the
square. Nothing else of it is kept.

- An app icon needs a logo. Upload one first, or both in the same save. An
  app icon on a server with no logo is refused (`app_icon_needs_logo`).
- Replacing the logo keeps the app icon.
- **Remove app icon** makes the icons from the logo again. **Reset logo to the
  muster mark** removes both.
- The logo's [ETag](#the-instance-document) covers the icons muster serves,
  so setting or removing an app icon changes it. Every link to the icons
  changes with it: the pages, the manifest, Web Push and the instance
  document.

## What happens to an upload

The upload (a logo or an app icon) is never stored or served as sent. muster:

1. **Checks the size**: more than 1 MiB is refused.
2. **Checks the format by its bytes**: the file must start with a PNG, JPEG or
   WebP signature. The filename and the Content-Type the browser sends are not
   read at all. SVG is refused (it is markup and can carry script), as are GIF,
   HTML and everything else.
3. **Reads only the header first**: an image wider or taller than 4096 pixels,
   or with more than 4,194,304 pixels (2048 × 2048), is refused **before** any pixel is
   decoded. A small file can declare a huge image (a decompression bomb), and
   this is what keeps it from being decoded.
4. **Decodes it** with that format's own decoder. A truncated or damaged file
   is refused.
5. **Draws the pixels onto a fresh canvas and writes new PNGs**:
   - `logo.png`: the logo, scaled down to fit 1024 × 1024 if it is larger
     (never scaled up);
   - `icon-192.png` and `icon-512.png`: square icons, the logo scaled to fit
     and centred on a transparent background, with nothing cropped.

An app icon's step 5 writes only `icon-192.png` and `icon-512.png`, drawn
from the upload (scaled down to fit 1024 × 1024 first, as a logo is).

Nothing of the original file survives except its pixels: no EXIF, no text
chunks, no colour profile, and no bytes appended after the image (a file that
is both a PNG and an HTML page loses the HTML).

One upload is processed at a time. Processing the largest permitted image
peaks at about 120 MB of memory (measured: a 2048 × 2048 progressive CMYK
JPEG; other formats about 116 MB). The pixel cap is what bounds it: memory
grows with the pixel count, not the file size, and at the old cap of 16
million pixels a progressive CMYK JPEG of a few hundred KB took 560-580 MB.

| refusal | code | status |
|---|---|---|
| the file is empty | `empty` | 400 |
| the file is over 1 MiB | `too_large` | 413 |
| the request body is over 2 MiB (plus the form's framing) | `body_too_large` | 413 |
| SVG | `svg_refused` | 415 |
| not a PNG, JPEG or WebP | `unsupported_type` | 415 |
| truncated or damaged | `corrupt` | 400 |
| a side over 4096 pixels | `too_wide` | 400 |
| over 4,194,304 pixels | `too_many_pixels` | 400 |
| the name is not valid UTF-8 | `name_invalid` | 400 |
| the name is over 64 characters | `name_too_long` | 400 |
| the name has a control or text-direction character | `name_control` | 400 |
| `logo_shows_name` is not `true` or `false` | `logo_shows_name_invalid` | 400 |
| an app icon, and no logo is set or sent | `app_icon_needs_logo` | 400 |
| nothing was sent: no logo, no app icon, no name, no `logo_shows_name` | `empty` | 400 |

The file refusals above apply to the app icon too, with the same codes.

## The routes

| route | who | what it does |
|---|---|---|
| `GET /api/instance` | anyone | The [instance document](#the-instance-document). `Cache-Control: public, max-age=300`. |
| `GET /brand/logo.png`, `/brand/icon-192.png`, `/brand/icon-512.png` | anyone | The images, cached by URL (below). 404 when no logo is set. |
| `PUT /api/instance/branding` | an admin's browser session | `multipart/form-data`, each part optional: `logo` (a file), `app_icon` (a file), `name` (empty clears it) and `logo_shows_name` (`true` or `false`). At least one must be sent. A part left out leaves that setting as it is. `logo_shows_name` may be sent more than once, and the last value counts: the Account page sends a hidden `false` before the checkbox, so an unticked box (which a browser does not send) still saves `false`. Answers with the instance document. |
| `DELETE /api/instance/branding/logo` | an admin's browser session | Removes the logo and the app icon, and keeps the name. Answers with the instance document. |
| `DELETE /api/instance/branding/app-icon` | an admin's browser session | Removes the app icon: the icons are made from the logo again. Answers with the instance document. |

The writes take an admin's browser session from muster's own pages (the
`Origin` must be muster's), and no machine credential: no personal API token,
hook token or agent token. A member is answered 403, and no session 401.

Each image is served with:

- `Content-Type: image/png` and `X-Content-Type-Options: nosniff`;
- `Content-Security-Policy: default-src 'none'`;
- an `ETag`. A request whose `If-None-Match` names it gets 304;
- a `Cache-Control` chosen by the URL:
  - `?v=<the current etag>`: `public, max-age=2592000, immutable`. That URL
    names those exact bytes, so it may be cached for 30 days;
  - no `v`, or a `v` that is not the current etag: `no-cache`. The current
    image is still served (a stale `v` is not redirected), but a browser or a
    CDN must revalidate it before reusing it, and the `ETag` answers that with
    a 304;
  - a 404 (no logo set): `no-cache`.

Every link muster publishes to the images carries `?v=<etag>`: its pages, the
web app manifest, Web Push notification icons and the
[instance document](#the-instance-document). A new logo is a new URL, so no
cache shows the old one.

Resetting or replacing a logo does **not** purge copies already cached under
its old `?v=` URL: a browser or CDN that fetched it may keep serving that URL
for up to 30 days. Nothing muster publishes links it any more, but the bytes
stay reachable by anyone who has the old URL. To withdraw a logo at once,
also purge `/brand/*` at your CDN.

Without the `no-cache` on an unversioned URL, a CDN in front of muster (most
cache `.png` by default) keeps serving the previous logo for as long as the
old lifetime allowed — even after the logo is reset and the origin answers
404.

### The instance document

`GET /api/instance` needs no credential. It works on the sign-in page and for
an app that has not signed in yet:

```json
{
  "name": "Team Example",
  "logo": {
    "url": "/brand/logo.png?v=3f2a9c1d0b7e4a65",
    "icon192": "/brand/icon-192.png?v=3f2a9c1d0b7e4a65",
    "icon512": "/brand/icon-512.png?v=3f2a9c1d0b7e4a65",
    "etag": "3f2a9c1d0b7e4a65"
  }
}
```

- `name` is `null` when no name is set.
- `logo` is `null` when no logo is set.
- The paths are on the same server, and each carries `?v=<etag>` exactly as
  muster's pages link it. Fetch them as given, query string included: that is
  the URL a cache may keep, and it always answers with the images `etag`
  names. A path with the query dropped still answers with the current image,
  but `no-cache`.
- `etag` is the first 16 hex digits of a sha256 over the three images, and it
  changes whenever any of them does (an app icon set or removed included). A
  client that caches the images should compare it.
- `icon192` and `icon512` are the [app icon](#an-app-icon-of-its-own)'s when
  one is set.

These field names are a contract with the Android app's server switcher.

The document does not carry the
[logo-shows-the-name](#a-logo-that-spells-the-name) setting. The switcher lists
servers by name, and the name stays the switcher's label whether or not the
logo spells it.

## Notifications

Every Web Push payload carries `icon`: the logo's 192 px icon (versioned by its
ETag) when a logo is set, and muster's own icon otherwise. With
`MUSTER_PUBLIC_URL` set the URL is absolute; without it, it is a path on the
server, which the service worker resolves against its own origin.

The Android app's push payload (v2) carries no icon: the app reads the logo
from [`GET /api/instance`](#the-instance-document) itself, when it starts, when
a server is added, and every 15 minutes.

## Behind an edge that signs people in

`GET /api/instance` and `/brand/` hold nothing per-user: only what an admin
chose to publish. A deployment that relies on muster's own sign-in serves them
to anyone, which the sign-in page needs.

Behind a single sign-on proxy, the proxy's own sign-in comes first, so they are
reachable only after it unless you let them through. Let them through if the
apps should show the server's name and icon before signing in. Keep
`/manifest.webmanifest` behind the proxy either way
([auth.md](auth.md#sign-in-origins-for-a-single-sign-on-proxy)).

## Where it is kept

One row in `instance_branding` (migrations `0105` and `0114`,
[schema.md](schema.md)): the name, the logo and the icons made from it, the
app icon's two icons, the ETag, the logo-shows-the-name setting, when it last
changed and which admin changed it. The table belongs to the server, not to a user: it has no `owner_id`.

Row-level security is on and forced, with one policy that admits only the
system scope. muster reads the row as the system (the sign-in page has no user)
and writes it as the system only after checking that the caller is an admin.
A query run with any user's principal, an admin's included, sees no row and
writes none.

A server reads the row at most every 30 seconds, and at once after its own
write. A second replica shows a change within 30 seconds.

## Rolling back

To roll back only `0114` (the app icon and the logo-shows-the-name setting),
roll the image back, then run the migration header's statements
**immediately**. Do not run them first: the current image reads these columns
and fails without them. Until they run, the older image serves the logo's own
icons under the app icon's ETag, so a cache keyed by that `?v=` URL can keep
the wrong bytes. Its **Reset logo** also fails with a 500, because clearing the
logo breaks `instance_branding_app_icon_needs_logo` while an app icon is still
stored.

An older image never reads these columns: it draws the name beside the logo
and serves the logo's own icons. The first statement puts back the ETag of the
logo's icons, so it matches what that image serves:

```sql
UPDATE public.instance_branding SET logo_etag = substr(encode(sha256(logo_png || icon192_png || icon512_png), 'hex'), 1, 16) WHERE appicon192_png IS NOT NULL;
ALTER TABLE public.instance_branding DROP CONSTRAINT IF EXISTS instance_branding_app_icon_needs_logo;
ALTER TABLE public.instance_branding DROP CONSTRAINT IF EXISTS instance_branding_app_icon_whole;
ALTER TABLE public.instance_branding DROP COLUMN IF EXISTS appicon512_png;
ALTER TABLE public.instance_branding DROP COLUMN IF EXISTS appicon192_png;
ALTER TABLE public.instance_branding DROP COLUMN IF EXISTS logo_shows_name;
DELETE FROM schema_migrations WHERE version = 114;
```

To go back past `0105`, roll the image back first: an image older than
`0105` never reads the table and draws muster's own mark.
To remove all the data (after rolling back `0114`):

```sql
DROP TABLE IF EXISTS public.instance_branding;
DELETE FROM schema_migrations WHERE version = 105;
```

The same statements are in the migration's header, and a test runs them.
