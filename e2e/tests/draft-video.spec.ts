import { execFileSync } from 'node:child_process';
import crypto from 'node:crypto';
import fs from 'node:fs';
import path from 'node:path';
import { test, expect } from '../lib/fixtures';
import { signIn } from '../lib/signin';
import { draftVideoPort, draftVideoS3Port, dsn, e2eRoot, repoRoot, versionA } from '../lib/env';
import { startServer, Running } from '../lib/server';
import { FakeS3, startFakeS3 } from '../lib/fakes3';

// A STEP-UP CARD'S VIDEO (internal/api host_stepup_card.go stepUpMediumHTML;
// operator: "the embedded video player is showing broken videos"), in a real
// browser against a real muster-server whose upload store is a fake S3
// holding a REAL MP4 — served through GET /plugins/drafts/{id}/media/{n}, the
// route the card draws, never a route stub — at a phone (390×844, touch) and
// a desktop (1440×900):
//
//   - the media route answers a Range request 206 with the video's type and
//     the inert-file headers, and the card's page plays it: readyState ≥ 1, no
//     MediaError, the film's own size (invariant guards: the base serves and
//     plays it too — measured, the same bytes and headers at both);
//   - 🔴 the Asks feed's compact preview draws the video as a THUMBNAIL — its
//     first frame under a play mark, no native controls — where the base drew
//     <video controls> in an 80px box, whose control bar could not fit and
//     looked broken (RED at the base: `controls` on the feed's video);
//   - 🔴 the full-size player draws the whole film (object-fit: contain);
//     the base cropped a portrait film to its middle (object-cover). RED at
//     the base.
//
// The film: MUSTER_E2E_VIDEO when set (a local file), else the committed
// e2e/testdata/portrait-film.mp4 — H.264 High, yuv420p, 810×1440, moov first,
// the shape of the film the operator's card carried.

const origin = `http://localhost:${draftVideoPort}`;
const stamp = 'video' + Date.now().toString(36);
const ACCESS = 'e2e-draftvideo-s3-access-not-real';
const film = process.env.MUSTER_E2E_VIDEO || path.join(e2eRoot, 'testdata', 'portrait-film.mp4');
let server: Running | undefined;
let s3: FakeS3 | undefined;
let card: { draft: number; ask: number };
let bytes: Buffer;

const fix = (...args: string[]) =>
  execFileSync('go', ['run', './e2e/asksfiltfix', ...args], { cwd: repoRoot, env: { ...process.env, DATABASE_URL: dsn } }).toString().trim();

test.use({ storageState: { cookies: [], origins: [] } });

test.beforeAll(async () => {
  test.setTimeout(180_000);
  s3 = await startFakeS3(draftVideoS3Port, ACCESS, 'muster-e2e');
  server = await startServer(versionA, draftVideoPort, {
    MUSTER_PLUGINS: '1',
    MUSTER_FOCUS_INTERVAL: '0',
    MUSTER_PUBLIC_URL: origin,
    MUSTER_S3_ENDPOINT: `http://127.0.0.1:${draftVideoS3Port}`,
    MUSTER_S3_BUCKET: 'muster-e2e',
    MUSTER_S3_ACCESS_KEY: ACCESS,
    MUSTER_S3_SECRET_KEY: 'e2e-draftvideo-s3-secret-not-real',
    MUSTER_UPLOAD_SIGNING_KEY: 'e2e-draftvideo-signing-key-not-a-real-one-0123456789',
  });
  bytes = fs.readFileSync(film);
  const key = 'uploads/plugin-drafts/e2e/' + stamp;
  s3.objects.set(key, bytes);
  card = JSON.parse(fix('video', '1', stamp, key, crypto.createHash('sha256').update(bytes).digest('hex'), String(bytes.length)));
});

test.afterAll(async () => {
  try {
    fix('clear', '1', stamp);
  } finally {
    await server?.stop();
    await s3?.close();
  }
});

for (const vp of [
  { name: 'phone 390×844', use: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } },
  { name: 'desktop 1440×900', use: { viewport: { width: 1440, height: 900 } } },
]) {
  test.describe(vp.name, () => {
    test.use(vp.use);

    test('the card plays a real MP4 from its media route', async ({ page }) => {
      await signIn(page, origin);
      // The route itself: a Range request is answered 206, typed, inert.
      const r = await page.request.get(`${origin}/plugins/drafts/${card.draft}/media/0`, { headers: { Range: 'bytes=0-99' } });
      expect(r.status()).toBe(206);
      expect(r.headers()['content-type']).toBe('video/mp4');
      expect(r.headers()['content-range']).toBe(`bytes 0-99/${bytes.length}`);
      expect(r.headers()['accept-ranges']).toBe('bytes');
      expect(r.headers()['x-content-type-options']).toBe('nosniff');
      expect((await r.body()).equals(bytes.subarray(0, 100)), 'the bytes are the film\'s').toBe(true);

      await page.goto(`${origin}/attention/asks/${card.ask}`);
      const video = page.locator('[data-stepup-card] video[data-stepup-video]');
      await expect(video).toBeVisible();
      await expect.poll(() => video.evaluate((v: HTMLVideoElement) => v.readyState), { message: 'the film loaded its metadata' }).toBeGreaterThanOrEqual(1);
      const st = await video.evaluate((v: HTMLVideoElement) => ({ err: v.error ? v.error.code : 0, w: v.videoWidth, h: v.videoHeight, controls: v.controls, fit: getComputedStyle(v).objectFit, inline: v.playsInline }));
      expect(st.err, 'no MediaError').toBe(0);
      expect([st.w, st.h], 'the film\'s own size').toEqual([810, 1440]);
      expect(st.controls, 'the full-size player has its controls').toBe(true);
      expect(st.inline).toBe(true);
      expect(st.fit, 'the whole film is drawn, never cropped').toBe('contain');
      const box = await video.boundingBox();
      expect(box!.height, 'a player, not a sliver').toBeGreaterThan(200);
    });

    test('the Asks feed draws the video as a thumbnail with a play mark, not a cramped player', async ({ page }) => {
      await signIn(page, origin);
      await page.goto(`${origin}/attention`);
      const cardEl = page.locator(`#ask-${card.ask}`);
      await expect(cardEl).toBeVisible();
      const thumb = cardEl.locator('[data-stepup-video-thumb]');
      await expect(thumb).toBeVisible();
      const video = thumb.locator('video[data-stepup-video]');
      await expect.poll(() => video.evaluate((v: HTMLVideoElement) => v.readyState)).toBeGreaterThanOrEqual(1);
      const st = await video.evaluate((v: HTMLVideoElement) => ({ err: v.error ? v.error.code : 0, controls: v.controls, hasAttr: v.hasAttribute('controls'), muted: v.muted }));
      expect(st.err).toBe(0);
      expect(st.controls || st.hasAttr, 'a thumbnail carries no native controls').toBe(false);
      expect(st.muted).toBe(true);
      // The play mark sits over the frame.
      const tb = await thumb.boundingBox();
      const mb = await thumb.locator('svg').boundingBox();
      expect(mb!.x).toBeGreaterThanOrEqual(tb!.x - 1);
      expect(mb!.x + mb!.width).toBeLessThanOrEqual(tb!.x + tb!.width + 1);
      // A tap on it opens the card's page, where it plays.
      if (vp.use.hasTouch) await thumb.tap();
      else await thumb.click();
      await expect(page).toHaveURL(`${origin}/attention/asks/${card.ask}`);
      await expect(page.locator('[data-stepup-card] video[data-stepup-video]')).toBeVisible();
      // The card's page opens at its top.
      await page.waitForTimeout(500);
      expect(await page.evaluate(() => Math.round(window.scrollY)), 'the card page opened scrolled').toBe(0);
    });
  });
}
