import { test, expect } from '../lib/fixtures';
import { hookToken } from '../lib/env';
import { mkchat } from '../lib/mirrorhost';

// A CHIEF REPLY THAT NAMES ANOTHER AGENT OR A TASK LINKS TO IT
// (internal/ui/mentions.go), on the shared server, phone-sized. Operator ask:
// when the chief mentions another session or agent, render it as a link to
// that session's view.
//
// The chief is an agent of this spec's own (display name "chief", e2e/mkchat),
// and so is the agent its reply names; both are removed afterwards. The task
// is made through the API and dismissed afterwards. Its reply also names an
// agent that does NOT exist and puts the real one in a code span: neither may
// become a link — a mention is linked only when it resolves, and never inside
// code.

const stamp = Date.now().toString(36);
const chief = 'e2e-mention-chief-' + stamp;
const target = 'e2e-mention-target-' + stamp;
const ghost = 'e2e-mention-ghost-' + stamp;

test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

test.beforeAll(() => {
  mkchat(chief, null);
  mkchat(target, null);
  mkchat(target, [{ role: 'user', content: 'hello' }, { role: 'assistant', content: 'On it.' }]);
  mkchat(chief, [{ role: 'user', content: 'who has the login fix?' }], { MUSTER_E2E_DISPLAY_NAME: 'chief' });
});

test.afterAll(() => {
  mkchat(chief, null);
  mkchat(target, null);
});

test('a chief reply links the agent and task it names, and clicking the agent opens its page', async ({ page, api, request }) => {
  const task = await api.createTask('e2e mention task ' + stamp, 'synthetic task body');
  try {
    mkchat(chief, [{ role: 'assistant', content: `Handed it to ${target}, see #${task.id}. ${ghost} is not real, and \`${target}\` is code.` }], {
      MUSTER_E2E_APPEND: '1',
    });
    await page.goto('/agents/' + chief);
    const bubble = page.locator('[data-bubble="assistant"]').last();
    await expect(bubble).toContainText('Handed it to');

    // Exactly the two that resolve are links: the agent and the task.
    const links = bubble.locator('a[data-mention]');
    await expect(links).toHaveCount(2);
    const agentLink = bubble.locator('a[data-mention="agent"]');
    await expect(agentLink).toHaveText(target);
    await expect(agentLink).toHaveAttribute('href', '/agents/' + target);
    await expect(agentLink).not.toHaveAttribute('target', /.*/);
    const taskLink = bubble.locator('a[data-mention="task"]');
    await expect(taskLink).toHaveText('#' + task.id);
    await expect(taskLink).toHaveAttribute('href', '/tasks/' + task.id);
    // The unresolved name and the code span stay text.
    await expect(bubble).toContainText(ghost);
    await expect(bubble.locator('code')).toHaveText(target);

    // Clicking the agent opens ITS page, in this tab.
    await agentLink.click();
    await expect(page).toHaveURL(new RegExp(`/agents/${target}\\?session=\\d+$`));
    await expect(page.locator('[data-bubble="assistant"]').last()).toContainText('On it.');
  } finally {
    await request.delete('/api/tasks/' + task.id, { headers: { Authorization: `Bearer ${hookToken}` } });
  }
});
