import { Page } from '@playwright/test';

// THE DOCS IMAGES CALL EVERY SESSION BY ITS WORK, NEVER BY ITS CODENAME.
//
// A session with a conversation (a Claude Code window: a link to its Rendered
// view, /mirror/view) is headed by its NAME (docs/focus.md, "A session's
// name"), which docs.spec.ts gives it the way a focus refresh does; without
// one muster falls back to the window's codename (`otter:1`). A window with no
// conversation (a shell, a Codex window) has nothing to name, so it must carry
// a work-style host label (`api-logs:1`), never its raw tmux session
// (`kestrel:1`). Before this check the README's images showed only codenames.
//
// What is read (collectSessionTitles, in the page): every sidebar row and
// switcher entry (muster draws each one's name in data-switcher-name and its
// codename in data-switcher-code) and the page header's title
// ([data-mirror-name], which carries data-session-name=<source> only when the
// name is a human one, internal/ui/session_name.go).
//
// The same rule the demo-fleet media outside this repository is held to
// (docs/testing.md), so every image the README shows holds one rule.

export type Title = { where: string; name: string; code: string; session?: string; source?: string; nameable: boolean };

// A bare codename title: muster's fallback for a window, its host label (or
// tmux session) and window index, qualified by host when it repeats
// (`otter:1`, `otter:1 · laptop`; docs/mirroring.md).
export const CODENAME_TITLE = /^[\w.-]+:\d+(?: · .+)?$/;

// Passed to page.evaluate, so it must stay self-contained.
export function collectSessionTitles(): Title[] {
  const onView = location.pathname === '/mirror/view';
  const sessionOf = (url: string) => new URLSearchParams(url.split('?')[1] || '').get('session') || '';
  const out: Title[] = [];
  for (const e of document.querySelectorAll('[data-switcher-name]')) {
    const link = e.getAttribute('href') || e.getAttribute('data-href') || '';
    out.push({
      where: e.closest('#sidebar-agents-list') ? 'sidebar' : 'switcher',
      name: (e.getAttribute('data-switcher-name') || '').trim(),
      code: (e.getAttribute('data-switcher-code') || '').trim(),
      session: link ? sessionOf(link) : location.pathname.startsWith('/mirror/') ? sessionOf(location.search) : '',
      nameable: link ? link.startsWith('/mirror/view?') : onView,
    });
  }
  for (const e of document.querySelectorAll('header [data-mirror-name]')) {
    out.push({ where: 'header', name: (e.textContent || '').trim(), code: '', source: e.getAttribute('data-session-name') || '', nameable: onView });
  }
  return out;
}

// The titles that call a session by its codename. A nameable one: by the
// words, or by STATE (the name IS the entry's own codename, or a header with
// no human-name source). One muster cannot name: titled by its raw tmux
// session instead of a host label.
export function bareCodenames(titles: Title[]): Title[] {
  return titles.filter((t) =>
    t.nameable
      ? CODENAME_TITLE.test(t.name) ||
        (t.code !== '' && (t.name === t.code || t.name.startsWith(t.code + ' · '))) ||
        (t.where === 'header' && (!t.source || t.source === 'fallback'))
      : !!t.session && (t.name === t.session || t.name.startsWith(t.session + ':')),
  );
}

// The check's controls: each FLAGGED one must be caught alone, each PASSED one
// let through. Run once (namesControls) before the first shot, so a check that
// cannot see its own control fails the file instead of passing every image.
const FLAGGED: Title[] = [
  { where: 'sidebar', name: 'zz9:1', code: '', nameable: true }, // the words alone
  { where: 'switcher', name: 'Zz nine', code: 'Zz nine', nameable: true }, // the name IS the codename
  { where: 'sidebar', name: 'zz9 · far', code: 'zz9', nameable: true }, // the codename, qualified
  { where: 'header', name: 'Zz the nine', code: '', source: '', nameable: true }, // a header with no name source
  { where: 'sidebar', name: 'zz9:1', code: 'zz9:1', session: 'zz9', nameable: false }, // a shell titled by its raw session
];
const PASSED: Title[] = [
  { where: 'sidebar', name: 'zz-logs:1', code: 'zz-logs:1', session: 'zz9', nameable: false }, // a shell with a host label
  { where: 'header', name: 'Fix the zz9 test', code: '', source: 'auto', nameable: true },
];
export function namesControls() {
  for (const c of FLAGGED) if (bareCodenames([c]).length !== 1) throw new Error(`name check cannot see its own control: ${JSON.stringify(c)}`);
  for (const c of PASSED) if (bareCodenames([c]).length) throw new Error(`name check flags a title it must pass: ${JSON.stringify(c)}`);
}

// namedSessions fails the shot when any title on the page is a bare codename,
// and returns how many nameable titles it read (a caller can require > 0, so a
// selector that matches nothing cannot pass every image).
export async function namedSessions(page: Page, where: string): Promise<number> {
  const titles = await page.evaluate(collectSessionTitles);
  const bare = bareCodenames(titles);
  if (bare.length) {
    throw new Error(`${where}: ${bare.length} session(s) titled by codename, not by their work: ${bare.map((t) => `${t.where} "${t.name}"`).join(', ')}`);
  }
  return titles.filter((t) => t.nameable).length;
}
