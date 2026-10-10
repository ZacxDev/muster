// The suite's shared test object: signed in, phone-sized, with a machine API
// helper. Every spec imports `test` and `expect` from here.
import { test as base, expect, APIRequestContext, request as pwRequest } from '@playwright/test';
import fs from 'node:fs';
import { authState, baseURL, hookToken, stateDir } from './env';

export { expect };

export interface Task {
  id: number;
  title: string;
  body: string;
  status: string;
}

export class Api {
  constructor(private readonly ctx: APIRequestContext) {}
  async tasks(): Promise<Task[]> {
    const r = await this.ctx.get('/api/tasks');
    expect(r.status(), await r.text()).toBe(200);
    return (await r.json()) as Task[];
  }
  async createTask(title: string, body = ''): Promise<Task> {
    const r = await this.ctx.post('/api/tasks', { data: { title, body } });
    expect(r.status(), await r.text()).toBeLessThan(300);
    return (await r.json()) as Task;
  }
  async setStatus(id: number, status: string): Promise<void> {
    const r = await this.ctx.patch(`/api/tasks/${id}/status`, { data: { status } });
    expect(r.status(), await r.text()).toBeLessThan(300);
  }
}

export const test = base.extend<{ api: Api; requireServer: void }>({
  storageState: authState,
  baseURL,
  api: async ({}, use) => {
    const ctx = await pwRequest.newContext({ baseURL, extraHTTPHeaders: { Authorization: `Bearer ${hookToken}` } });
    await use(new Api(ctx));
    await ctx.dispose();
  },
  // 🔴 A SUITE WITH NO DATABASE SKIPS — AND SAYS SO. global-setup writes this
  // file when MUSTER_E2E_DATABASE_URL is unset (and refuses outright when
  // MUSTER_E2E_REQUIRE_FULL=1). The verdict step caps skips, so a CI run cannot
  // go green on a suite that executed nothing.
  requireServer: [
    async ({}, use, testInfo) => {
      testInfo.skip(fs.existsSync(`${stateDir}/skip`), 'no e2e database (MUSTER_E2E_DATABASE_URL unset)');
      await use();
    },
    { auto: true },
  ],
});

// effectiveThemeColor is the colour the browser would tint the status bar
// with: the FIRST theme-color meta whose media matches.
export const effectiveThemeColorJS = `(() => {
  for (const m of document.querySelectorAll('meta[name="theme-color"]')) {
    const q = m.getAttribute('media');
    if (!q || window.matchMedia(q).matches) return m.getAttribute('content');
  }
  return null;
})()`;
