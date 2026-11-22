// The tile indicators against a REAL livekit-server (scripts/e2e-harness
// -livekit=real): the owner and an admitted guest in one room, each with
// Chromium's fake camera and microphone. The guest mutes and turns the
// camera off with their own buttons; the owner's tile for them shows both
// within 1 s, and unmuting restores it; a real screen share switches the
// owner to the presentation view, and the share's own track ending (the
// browser's Stop sharing) ends it. This is the check that LiveKit
// really sends what the fake client (fixtures/fake-livekit-client.js) plays.
// Without a livekit-server the test skips, unless MEETINGS_REQUIRE_LIVEKIT=1
// (CI), when it fails.
import { test, expect } from '@playwright/test';
import { startHarness, type Harness } from './helpers';

let h: Harness;
test.beforeAll(async () => { h = await startHarness(['-livekit=real']); });
test.afterAll(() => { h?.proc.stdin?.end(); });

test.use({
  launchOptions: { args: ['--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream', '--allow-loopback-in-peer-connection', '--autoplay-policy=no-user-gesture-required'] },
  viewport: { width: 1280, height: 800 },
});

test("a guest's mute and camera-off reach the owner's tile within 1 s, through a real LiveKit", async ({ page, browser }) => {
  if (!h.realLiveKit) {
    if (process.env.MEETINGS_REQUIRE_LIVEKIT === '1') throw new Error('MEETINGS_REQUIRE_LIVEKIT=1 but the harness found no livekit-server');
    test.skip(true, 'no livekit-server (set MEETINGS_LIVEKIT_SERVER)');
  }
  await page.goto(`${h.parent}/apps/${h.pluginID}`);
  const app = page.frameLocator('#app');
  await app.locator('#new-meeting').click();
  // The link is filled in when New meeting's answer comes: wait for it.
  await expect(app.locator('#join-url')).not.toHaveValue('');
  const link = await app.locator('#join-url').inputValue();
  await expect(app.locator('#btn-join')).toBeEnabled({ timeout: 15_000 });
  await app.locator('#btn-join').click();
  await expect(app.locator('#call-view')).toHaveAttribute('data-conn', 'connected', { timeout: 30_000 });

  const gctx = await browser.newContext({ viewport: { width: 390, height: 844 } });
  const guest = await gctx.newPage();
  await guest.goto(link);
  await guest.locator('#name').fill('Grace');
  await expect(guest.locator('#ask')).toBeEnabled({ timeout: 15_000 });
  await guest.locator('#ask').click();
  await app.locator('#btn-people').click();
  await app.locator('.lobby-row.waiting .admit').click();
  await expect(guest.locator('#guest')).toHaveAttribute('data-state', 'admitted');

  // The guest's tile on the owner's page, live: their camera on, mic on.
  const t = app.locator('.tile[data-source="camera"]:not(.local):not(.leaving)');
  await expect(t).toHaveCount(1, { timeout: 30_000 });
  await expect(t.locator('.mu-tile__label')).toHaveText('Grace (guest)');
  await expect(t).toHaveAttribute('data-mic', 'on', { timeout: 15_000 });
  await expect(t).toHaveAttribute('data-cam', 'on', { timeout: 15_000 });
  await expect(t).toHaveAttribute('data-video', 'on', { timeout: 15_000 });
  // The guest's own controls say so too.
  await expect(guest.locator('#btn-mic')).toHaveAttribute('aria-pressed', 'false');

  // Mute and camera off, from the guest's control bar.
  await guest.locator('#btn-mic').click();
  await expect(guest.locator('#btn-mic')).toHaveAttribute('aria-pressed', 'true');
  await expect(t.locator('[data-ind="mic"]')).toBeVisible({ timeout: 1000 });
  await guest.locator('#btn-cam').click();
  await expect(guest.locator('#btn-cam')).toHaveAttribute('aria-pressed', 'true');
  await expect(t.locator('[data-ind="cam"]')).toBeVisible({ timeout: 1000 });
  await expect(t).toHaveAttribute('data-video', 'off', { timeout: 1000 });
  await expect(t.locator('.mu-avatar')).toBeVisible();

  // Unmute restores.
  await guest.locator('#btn-mic').click();
  await expect(t.locator('[data-ind="mic"]')).toBeHidden({ timeout: 1000 });
  await guest.locator('#btn-cam').click();
  await expect(t.locator('[data-ind="cam"]')).toBeHidden({ timeout: 1000 });
  await expect(t).toHaveAttribute('data-video', 'on', { timeout: 1000 });

  // LiveKit measures the connection and the tile shows a signal level.
  await expect(t).not.toHaveAttribute('data-quality', 'unknown', { timeout: 20_000 });
  await expect(t.locator('[data-ind="quality"]')).toHaveAttribute('aria-label', /^Connection (excellent|good|poor|lost)$/);

  // A real screen share (Chromium's fake capture): the owner's view switches
  // to presentation, and stopping the shared track itself, as the browser's
  // own "Stop sharing" does, ends it cleanly.
  await guest.setViewportSize({ width: 1280, height: 800 });
  await expect(guest.locator('#btn-share')).toBeVisible();
  await guest.locator('#btn-share').click();
  await expect(guest.locator('#btn-share')).toHaveAttribute('aria-pressed', 'true', { timeout: 15_000 });
  await expect(app.locator('#call-view')).toHaveAttribute('data-layout', 'presentation', { timeout: 15_000 });
  await expect(app.locator('#stage > .tile[data-source="screen_share"]')).toHaveCount(1);
  // The browser's "Stop sharing" ends the captured track and fires its
  // `ended`; do exactly that to the track getDisplayMedia gave (the one
  // with a displaySurface), and nothing else.
  const stopped = await guest.evaluate(() => {
    let n = 0;
    for (const v of document.querySelectorAll('video')) {
      const s = (v as HTMLVideoElement).srcObject as MediaStream | null;
      s?.getVideoTracks().forEach((t) => {
        if ((t.getSettings() as { displaySurface?: string }).displaySurface && t.readyState === 'live') { n++; t.stop(); t.dispatchEvent(new Event('ended')); }
      });
    }
    return n;
  });
  expect(stopped).toBe(1);
  await expect(guest.locator('#btn-share')).toHaveAttribute('aria-pressed', 'false', { timeout: 15_000 });
  await expect(app.locator('#call-view')).toHaveAttribute('data-layout', 'grid', { timeout: 15_000 });
  await expect(app.locator('.tile[data-source="screen_share"]:not(.leaving)')).toHaveCount(0);
  await expect(guest.locator('#presenting')).toBeHidden();
  await gctx.close();
});
