// Visual goldens of the meeting UI at a phone's size and a laptop's: the
// pre-join, the grid, the presentation view, the lobby and the transcript
// panel, drawn with muster's UI kit in its default theme (the harness serves
// muster's own render of it). The goldens are e2e/goldens/ui-visual.spec.ts/.
//
// 🔴 Pixels depend on the fonts and the rasteriser, so these run ONLY in the
// pinned Playwright image (mcr.microsoft.com/playwright:v1.64.1-noble): the
// `visual` project in playwright.config.ts, CI's `visual` job, and
// scripts/update-goldens.sh to remake them. Moving pictures (the camera
// preview, remote video, the level meter) and the random parts (the link,
// the transcript's clock) are hidden in place; the rest is compared.
import { test, expect, type FrameLocator, type Page } from '@playwright/test';
import { startHarness, useFakeLiveKit, ownerPrejoin, lk, appFrame, sizes, type Harness } from './helpers';

let h: Harness;
// A plugin that records (-s3=fake), as the goldens were drawn: the badge
// reads "Recording · …".
test.beforeAll(async () => { h = await startHarness(['-s3=fake']); });
test.afterAll(() => { h?.proc.stdin?.end(); });

// settle waits until nothing in the app frame is still animating, and moves
// the pointer away (no tooltip).
async function settle(page: Page) {
  await page.mouse.move(0, 0);
  const f = appFrame(page);
  await expect.poll(() => f.evaluate(() => document.getAnimations().filter((a) => a.playState === 'running' && a.effect?.getComputedTiming().iterations !== Infinity).length)).toBe(0);
}

// still hides what moves or is random, in place, so the tile's label,
// indicators and ring around it still show: the video pictures (the tile's
// own background shows), the level meter's bar, the link's text and the
// transcript's clock. (Through the CSSOM: the frame's CSP refuses an
// injected stylesheet, and screenshot's own `style` does not reach a
// cross-origin frame.)
async function still(page: Page) {
  await appFrame(page).evaluate(() => {
    document.querySelectorAll<HTMLElement>('video, .meter__fill').forEach((el) => { el.style.visibility = 'hidden'; });
    document.querySelectorAll<HTMLElement>('#join-url, .transcript .when').forEach((el) => { el.style.color = 'transparent'; });
  });
}

async function shot(page: Page, _app: FrameLocator, name: string) {
  await settle(page);
  await still(page);
  await expect(page.locator('#app')).toHaveScreenshot(name);
}

for (const size of sizes) {
  test.describe(`${size.name}`, () => {
    test.use({ viewport: { width: size.width, height: size.height } });

    test('pre-join, grid, presentation, lobby and transcript', async ({ page, browser }) => {
      await useFakeLiveKit(page.context());
      const app = await ownerPrejoin(page, h);
      await shot(page, app, `prejoin-${size.name}.png`);

      await app.locator('#btn-join').click();
      await expect(app.locator('#call-view')).toHaveAttribute('data-conn', 'connected');
      const f = appFrame(page);
      const me = await f.evaluate(() => (window as any).__lk.room().localParticipant.identity);
      await lk(f, 'quality', me, 'excellent');
      await lk(f, 'addRemote', 'guest-a', 'Grace Hopper (guest)', { quality: 'good' });
      await lk(f, 'addRemote', 'guest-b', 'Linus (guest)', { mic: false, cam: false, quality: 'poor' });
      await lk(f, 'speakers', ['guest-a']);
      await shot(page, app, `grid-${size.name}.png`);

      await lk(f, 'share', 'guest-a', true);
      await expect(app.locator('#call-view')).toHaveAttribute('data-layout', 'presentation');
      await shot(page, app, `presentation-${size.name}.png`);
      await lk(f, 'share', 'guest-a', false);
      await expect(app.locator('#call-view')).toHaveAttribute('data-layout', 'grid');

      // The lobby: one guest waiting, one in the call.
      await app.locator('#btn-people').click();
      const link = await app.locator('#join-url').inputValue();
      const guests = await browser.newContext();
      for (const name of ['Ada Byron', 'Alan T.']) {
        const g = await guests.newPage();
        await g.goto(link);
        await g.locator('#name').fill(name);
        await expect(g.locator('#ask')).toBeEnabled({ timeout: 15_000 });
        await g.locator('#ask').click();
        await expect(g.locator('#guest')).toHaveAttribute('data-state', 'waiting');
      }
      await expect(app.locator('.lobby-row.waiting')).toHaveCount(2);
      await app.locator('.lobby-row.waiting', { hasText: 'Alan T.' }).locator('.admit').click();
      await expect(app.locator('.lobby-row.waiting')).toHaveCount(1);
      await expect(app.locator('.lobby-row.admitted')).toHaveCount(1);
      await shot(page, app, `lobby-${size.name}.png`);
      await guests.close();

      // The transcript panel: the fake LiveKit's Egress streams the owner's
      // microphone to the fake engine; two lines arrive.
      await app.locator('#btn-transcript').click();
      const room = await f.evaluate(() => (window as any).__lk.room().name);
      const r = await fetch(`${h.parent}/publish?room=${encodeURIComponent(room)}&identity=owner`);
      expect(r.status, await r.text()).toBe(200);
      await expect(app.locator('#transcript li.line:not(.partial)')).toHaveCount(2, { timeout: 30_000 });
      await shot(page, app, `transcript-${size.name}.png`);
    });
  });
}
