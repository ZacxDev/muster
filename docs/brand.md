# The muster mark

muster's logo is a lowercase **m on three feet**. Each foot is a session: the
brass and violet discs are sessions that have answered, and the hollow ring is
the one waiting on you. The left stem and arch are brass and the rest of the
letter is off-white.

Every file here comes from one hand-drawn brand pack. Each SVG is drawn from
literal coordinates, with no embedded raster, and the wordmark is outlines, so
no font is needed to draw it. The files in this repository are **copies of the
pack's files, byte for byte**, or renders of them made by `make icons`. Do not
edit or redraw them here. To change the mark, change the pack and copy its
files over.

(This page is about muster's own mark. To give a server its own logo and name,
see [branding.md](branding.md).)

## Files

| file | used for |
|---|---|
| [`brand/lockup-horizontal.svg`](brand/lockup-horizontal.svg) | the mark and the wordmark side by side, for a dark ground: the README (dark mode) |
| [`brand/lockup-horizontal-on-light.svg`](brand/lockup-horizontal-on-light.svg) | the same for a light ground: the README (light mode) |
| [`brand/mark-on-dark.svg`](brand/mark-on-dark.svg), [`brand/mark-on-light.svg`](brand/mark-on-light.svg) | the mark alone on its ground, drawn with its clear space |
| [`brand/og-card.png`](brand/og-card.png) | the 1200 × 630 social card: upload it as the repository's social preview (Settings → Social preview) |
| [`brand/OFL.txt`](brand/OFL.txt) | the wordmark font's licence ([below](#font-and-licence)) |

The web app's copies are in `web/icon-src/` (the sources) and
`web/static/icons/` (what the server serves):

| served file | made from | used for |
|---|---|---|
| `lockup-horizontal.svg` | copied | the header, the sidebar, the sign-in page and the admin claim page |
| `favicon.svg` | copied | the browser tab: a version of the mark drawn on a 16 px grid; also the server in view's entry in the server switcher's list, when it has no branding |
| `favicon-32.png` | `favicon.svg` at 32 px | the tab, in a browser that draws no SVG icon |
| `icon-192.png`, `icon-512.png` | `icon.svg` | the manifest's `any` icons, a large favicon, and the Web Push icon (192) |
| `apple-touch-icon.png` | `icon.svg` at 180 px | the touch icon (iOS home screen) |
| `maskable-192.png`, `maskable-512.png` | `icon-maskable.svg` | the manifest's `maskable` icons: the mark inside the central 80% safe zone |
| `badge-72.png` | `mark-mono-white.svg` | the Web Push notification badge: the white mark, centred in 72 px (Android draws a badge from its alpha only) |

A server with a [logo of its own](branding.md) draws that logo instead, and
links none of muster's favicons. The two shortcut icons (`shortcut-*-96.png`)
are glyphs, not the mark.

`internal/api/brand_assets_test.go` holds every one of these files to the pack:
each is byte-identical to the pack's file or made from one by `make icons`, each
is at the size the page, manifest or service worker declares, each SVG has a
viewBox and no embedded raster, the maskable icons keep the mark inside the
safe zone, and every colour has at least 3:1 contrast with its ground.

## Usage

- On a dark ground use the plain files. On a light ground (paper, white) use the
  `-on-light` files, which use the deep variants of brass, off-white and violet.
- Never put the dark-ground mark on a light ground: the off-white disappears.
- Don't recolour the feet or reorder them. The ring is always last and always
  hollow; never fill it and never add a fourth foot.
- Don't set the wordmark in live text; use the outlined lockup.
- No effects: no gradients, shadows, outlines or bevels. Don't stretch, rotate
  or re-space it.

## Clear space and minimum size

- **Clear space:** keep one foot diameter clear on every side of the mark and
  of the lockup. That is 28% of the mark's width. `mark-on-dark.svg` and
  `mark-on-light.svg` are drawn with exactly that padding.
- **Minimum size:** the mark at least 32 px wide (below that, use
  `favicon.svg`, which is drawn for 16 px and up), and the horizontal lockup at
  least 120 px wide. The header draws the lockup 28 px tall, about 132 px wide.
  On a phone-width screen (below 640 px) the header crops it to the mark alone,
  28 px tall and 38 px wide, so the update notice has room in the top-right
  corner. The sidebar always draws the whole lockup.

## Colours

Every colour is one of muster's own palette tokens (`web/css/input.css`).

| name | hex | token | role |
|---|---|---|---|
| navy | `#110F1C` | `--mu-bg` (dark) | the ground of every dark asset |
| brass | `#E4B556` | `--mu-accent` (dark) | the left stem, the first arch and the first foot |
| off-white | `#EEEBF7` | `--mu-text` (dark) | the second "n", the ring and the wordmark on dark |
| violet | `#6E6790` | `--mu-edge` (dark) | the middle foot |
| lavender | `#C3BDD9` | `--mu-text2` (dark) | secondary text on dark (the social card's tagline) |
| paper | `#F6F4FB` | `--mu-bg` (light) | the light ground |
| brass-deep | `#835D0F` | `--mu-accent` (light) | brass on light |
| ink | `#1A1630` | `--mu-text` (light) | off-white's counterpart on light |
| violet-deep | `#7D7699` | `--mu-edge` (light) | the middle foot on light |

Contrast against the ground, at least 3:1 (WCAG's non-text minimum) for every
colour: on navy, off-white 16.11:1, brass 9.96:1, violet 3.61:1; on paper, ink
16.02:1, brass-deep 5.44:1, violet-deep 3.91:1.

## Font and licence

The wordmark is set in **Figtree Medium** and converted to outlines.
Figtree is Copyright 2022 The Figtree Project Authors
(<https://github.com/erikdkennedy/figtree>) and licensed under the SIL Open Font
License, Version 1.1. Its text is in [`brand/OFL.txt`](brand/OFL.txt). Under the
OFL, artwork made from a font's outlines is not Font Software, and no font file
ships in this repository; the licence is recorded here as the notice for the
wordmark's letterforms.
