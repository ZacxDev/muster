import type { Page } from '@playwright/test';

// A screen's top layer, as a person reads it (operator rule: no variable,
// Compose profile, flag or command in muster's top layer, and no count that
// does not read as words). The Go scans are internal/plainscan's Setup and
// Counts; these are their browser halves, over what the browser actually shows.

// setupJargon is internal/plainscan's Setup, the same list: a variable's
// shape, an assignment, a placeholder, the .env file, a docs path, and the
// deployment and protocol words a non-engineer does not use (Docker Compose,
// Kubernetes, pod, API, URL, MCP, a token, a JWT, VAPID, GoTrue, S3, a
// webhook, "misconfigured"). The variable's shape is case-sensitive and the
// words are not, as in Go (which says (?i:…); JavaScript has no inline flag,
// so each letter of a word is spelled as its two cases).
const anyCase = (w: string) => w.replace(/[a-z]/gi, (c) => `[${c.toLowerCase()}${c.toUpperCase()}]`);
const words = ['docker', 'compose', 'kubernetes', 'pods?', 'API', 'URLs?', 'MCP', 'tokens?', 'JWT', 'VAPID', 'GoTrue', 'S3', 'webhooks?', 'misconfigured'];
export const setupJargon = new RegExp(
  /\b[A-Z][A-Z0-9]*_[A-Z0-9_]+\b|=|<[a-z][^>]*>|\.env\b|\bdocs\/|\.md\b/.source + '|\\b(?:' + words.map(anyCase).join('|') + ')\\b',
);
// clumsyCounts is internal/plainscan's Counts, the same pattern: a "(s)"
// plural, and a zero said out loud about the Pages screens' things ("0 live
// link", "0 responses", "0 waiting", "0 replies"). An invariant guard (see
// the Go comment), not a regression test on its own.
export const clumsyCounts = /\(s\)|(^|[^\d.,])0 (live links?|links?|responses?|repl(y|ies)|waiting|versions?)\b/;
// pageWords is the visible text of root (the Features page's, unless named)
// and the tooltips, accessible names and placeholders of its elements, never
// an ⓘ sheet's.
export const pageWords = (page: Page, root = '[data-features-page]') =>
  page.locator(root).evaluate((el) => {
    const out = [(el as HTMLElement).innerText];
    el.querySelectorAll('[title], [aria-label], [placeholder]').forEach((x) => {
      if (x.closest('[data-info-sheet]')) return;
      for (const a of ['title', 'aria-label', 'placeholder']) {
        const v = x.getAttribute(a);
        if (v) out.push(v);
      }
    });
    return out.join('\n');
  });
