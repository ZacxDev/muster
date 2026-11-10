# Instance branding

An admin can give a muster server its own logo and display name. muster then
draws them in place of its own mark:

- in the page header and the sidebar, on every signed-in page;
- on the sign-in page;
- as the favicon and the touch icon, on every page muster serves (the
  sign-in, invite, reset and claim pages included);
- as the installed app's icons (the web app manifest);
- as the icon of every Web Push notification;
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
- **Reset logo to the muster mark** removes the logo and keeps the name.

The section shows the current logo and the app icon made from it. A member's
Account page has no such section, and the routes refuse a member.

In password mode the operator (user 1) is the admin. In GoTrue mode it is any
user with the admin role ([auth.md](auth.md)).

## What happens to an upload

The upload is never stored or served as sent. muster:

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
| the request body is over 1 MiB (plus the form's framing) | `body_too_large` | 413 |
| SVG | `svg_refused` | 415 |
| not a PNG, JPEG or WebP | `unsupported_type` | 415 |
| truncated or damaged | `corrupt` | 400 |
| a side over 4096 pixels | `too_wide` | 400 |
| over 4,194,304 pixels | `too_many_pixels` | 400 |
| the name is not valid UTF-8 | `name_invalid` | 400 |
| the name is over 64 characters | `name_too_long` | 400 |
| the name has a control or text-direction character | `name_control` | 400 |
| neither a logo nor a name was sent | `empty` | 400 |

## The routes

| route | who | what it does |
|---|---|---|
| `GET /api/instance` | anyone | The [instance document](#the-instance-document). `Cache-Control: public, max-age=300`. |
| `GET /brand/logo.png`, `/brand/icon-192.png`, `/brand/icon-512.png` | anyone | The images, cached by URL (below). 404 when no logo is set. |
| `PUT /api/instance/branding` | an admin's browser session | `multipart/form-data`: `logo` (a file, optional) and `name` (optional; empty clears it). At least one must be sent. Answers with the instance document. |
| `DELETE /api/instance/branding/logo` | an admin's browser session | Removes the logo and keeps the name. Answers with the instance document. |

The two writes take an admin's browser session from muster's own pages (the
`Origin` must be muster's), and no machine credential: no personal API token,
hook token or agent token. A member is answered 403, and no session 401.

Each image is served with:

- `Content-Type: image/png` and `X-Content-Type-Options: nosniff`;
- `Content-Security-Policy: default-src 'none'`;
- an `ETag`. A request whose `If-None-Match` names it gets 304;
- a `Cache-Control` chosen by the URL:
  - `?v=<the current etag>`: `public, max-age=31536000, immutable`. That URL
    names those exact bytes, so it may be cached for a year;
  - no `v`, or a `v` that is not the current etag: `no-cache`. The current
    image is still served (a stale `v` is not redirected), but a browser or a
    CDN must revalidate it before reusing it, and the `ETag` answers that with
    a 304;
  - a 404 (no logo set): `no-cache`.

Every link muster publishes to the images carries `?v=<etag>`: its pages, the
web app manifest, Web Push notification icons and the
[instance document](#the-instance-document). A new logo is a new URL, so no
cache shows the old one.

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
  changes whenever any of them does. A client that caches the images should
  compare it.

These field names are a contract with the Android app's server switcher.

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

One row in `instance_branding` (migration `0105`, [schema.md](schema.md)): the
name, the three PNGs, the ETag, when it last changed and which admin changed
it. The table belongs to the server, not to a user: it has no `owner_id`.

Row-level security is on and forced, with one policy that admits only the
system scope. muster reads the row as the system (the sign-in page has no user)
and writes it as the system only after checking that the caller is an admin.
A query run with any user's principal, an admin's included, sees no row and
writes none.

A server reads the row at most every 30 seconds, and at once after its own
write. A second replica shows a change within 30 seconds.

## Rolling back

Roll the image back first: an older image never reads the table and draws
muster's own mark. Then, to remove the data:

```sql
DROP TABLE IF EXISTS public.instance_branding;
DELETE FROM schema_migrations WHERE version = 105;
```

The same statements are in the migration's header, and a test runs them.
