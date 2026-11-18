import { expect, Page } from '@playwright/test';

// turnSuggestionsOn turns suggested replies ON for the signed-in user of page's
// context, as Account → Suggested replies does (POST /account/suggest on=1).
//
// Suggested replies are OFF for a user who never chose (suggest.DefaultDisabled,
// docs/suggested-replies.md), so a spec that expects chips opts its user in
// first. It POSTs rather than clicking the Account switch because a server
// with no OpenRouter key (these specs' fake model needs none) shows no switch
// there — the registry says the feature needs the key — while the toggle route
// itself still saves the choice the engine reads. Same-origin, as the form is.
export async function turnSuggestionsOn(page: Page, origin: string): Promise<void> {
  const r = await page.request.post(origin + '/account/suggest', { form: { on: '1' }, headers: { Origin: origin }, maxRedirects: 0 });
  expect(r.status(), `POST /account/suggest on=1: ${await r.text()}`).toBe(204);
}
