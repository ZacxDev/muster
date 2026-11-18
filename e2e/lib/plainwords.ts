import type { Page } from '@playwright/test';

// The Features page's top layer, as a person reads it (operator rule: no
// variable, Compose profile, flag or command in muster's top layer). The Go
// scan is internal/plainscan's Setup; this is its browser half, over what the
// browser actually shows.

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
// pageWords is the page's visible text and the tooltips, accessible names
// and placeholders of its elements, never an ⓘ sheet's.
export const pageWords = (page: Page) =>
  page.locator('[data-features-page]').evaluate((root) => {
    const out = [(root as HTMLElement).innerText];
    root.querySelectorAll('[title], [aria-label], [placeholder]').forEach((el) => {
      if (el.closest('[data-info-sheet]')) return;
      for (const a of ['title', 'aria-label', 'placeholder']) {
        const v = el.getAttribute(a);
        if (v) out.push(v);
      }
    });
    return out.join('\n');
  });

