import type { Page } from '@playwright/test';

// The Features page's top layer, as a person reads it (operator rule: no
// variable, Compose profile, flag or command in muster's top layer). The Go
// scan is internal/plainscan's Setup; this is its browser half, over what the
// browser actually shows.

// setupJargon is a variable's shape, an assignment, the .env file, a docs
// path, Docker Compose, Kubernetes and "misconfigured".
export const setupJargon = /\b[A-Z][A-Z0-9]*_[A-Z0-9_]+\b|=|\.env\b|docs\/|\.md\b|\b(docker|compose|kubernetes|misconfigured)\b/i;
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

