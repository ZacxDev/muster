import { Page } from '@playwright/test';

// openDispatchSheet opens the free-form dispatch sheet (the Model, Repository,
// task picker and ad-hoc task form, GET /ui/agents/new) on the page shown, the
// way the Agents tab's "+" FAB did: reveal the sheet (cgModalOpen, which first
// clears a previous open's body), then load its body.
//
// ⚠ THE FAB IS GONE (operator ask: "hide the agent replies FAB and deploy agent
// FAB on agent list page"), and NOTHING ELSE IN THE UI OPENS THIS FORM: a task
// card's Dispatch button opens the TASK-SCOPED sheet (/ui/agents/new?note=…).
// The specs that dispatch an agent to test what follows (a kind, a terminal, a
// composer, a layout) open it here instead of through a control a user can no
// longer reach. The sheet's markup and its POST are unchanged.
export async function openDispatchSheet(page: Page): Promise<void> {
  await page.waitForFunction(() => typeof (window as any).htmx?.ajax === 'function');
  await page.evaluate(() => {
    const w = window as any;
    if (w.cgModalOpen) w.cgModalOpen('agent-modal');
    else document.getElementById('agent-modal')!.classList.remove('hidden');
    w.htmx.ajax('GET', '/ui/agents/new', { target: '#agent-modal-body', swap: 'innerHTML' });
  });
}
