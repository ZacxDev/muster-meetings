// The recording (design: phase 5), with the harness's fake S3 store and the
// fake LiveKit as the room composite Egress (scripts/e2e-harness -s3=fake):
// the badge every participant sees, its "Recording failed" while the egress
// is down, the owner's archive playing the video beside the transcript with
// a line click seeking it, and the share link's page playing it with no
// muster sign-in. The fake composite "uploads" a 20 s VP9 MP4
// (internal/frames/testdata); its file begins when the egress is accepted.
import { test, expect, type Browser, type FrameLocator, type Page } from '@playwright/test';
import { startHarness, useFakeLiveKit, ownerInCall, sizes, type Harness } from './helpers';

let h: Harness;
test.beforeAll(async () => { h = await startHarness(['-s3=fake']); });
test.afterAll(() => { h?.proc.stdin?.end(); });

async function get(browser: Browser, path: string): Promise<string> {
  const ctx = await browser.newContext();
  const r = await ctx.request.get(`${h.parent}${path}`);
  const body = await r.text();
  await ctx.close();
  expect(r.status(), `${path}: ${body}`).toBe(200);
  return body;
}

// roomOf is the room of the meeting made last (the tests run one at a time).
async function roomOf(browser: Browser): Promise<string> {
  const room = await get(browser, '/latest-room');
  expect(room).toMatch(/^meeting-[A-Za-z0-9_-]{16}$/);
  return room;
}

// guestIn knocks as Grace on link and the owner admits her; the guest's page
// is in the call (the fake LiveKit client).
async function guestIn(browser: Browser, app: FrameLocator, link: string, viewport: { width: number; height: number }): Promise<Page> {
  const ctx = await browser.newContext({ viewport });
  await useFakeLiveKit(ctx);
  const guest = await ctx.newPage();
  await guest.goto(link);
  await guest.locator('#name').fill('Grace');
  await expect(guest.locator('#ask')).toBeEnabled({ timeout: 15_000 });
  await guest.locator('#ask').click();
  await expect(app.locator('#lobby-count')).toHaveText('1');
  await app.locator('#btn-people').click();
  await app.locator('.lobby-row.waiting .admit').click();
  await expect(guest.locator('#guest')).toHaveAttribute('data-state', 'admitted');
  await expect(guest.locator('#call-view')).toBeVisible();
  return guest;
}

const on = 'Recording · Transcribing · AI';
const failed = 'Recording failed · Transcribing · AI';

for (const size of sizes) {
  test(`at ${size.width}x${size.height} the owner and a guest see the recording badge, "Recording failed" while the egress is down, and Recording again`, async ({ browser }) => {
    const viewport = { width: size.width, height: size.height };
    const owner = await browser.newPage({ viewport });
    await useFakeLiveKit(owner.context());
    const app = await ownerInCall(owner, h);
    const link = await app.locator('#join-url').inputValue();
    const guest = await guestIn(browser, app, link, viewport);
    const badges = [app.locator('#badge'), guest.locator('#badge')];
    for (const b of badges) {
      await expect(b).toHaveText(on, { timeout: 15_000 });
      await expect(b).toBeInViewport({ ratio: 1 });
    }
    // The egress dies and its restarts are refused: everyone is told.
    const room = await roomOf(browser);
    await get(browser, '/composite-refuse?n=1000');
    await get(browser, `/composite-kill?room=${encodeURIComponent(room)}`);
    for (const b of badges) {
      await expect(b).toHaveText(failed, { timeout: 15_000 });
      await expect(b).toBeInViewport({ ratio: 1 });
    }
    // Egress takes starts again: the next retry records, and the badge says so.
    await get(browser, '/composite-refuse?n=0');
    for (const b of badges) await expect(b).toHaveText(on, { timeout: 45_000 });
    await guest.context().close();
    await owner.close();
  });
}

test.describe('the archive and the share link', () => {
  test.use({ viewport: { width: 1280, height: 800 } });

  // recordedMeeting holds a meeting with a guest, the guest's two lines said
  // while it was recorded, then ends it and opens its archive.
  async function recordedMeeting(browser: Browser, owner: Page): Promise<FrameLocator> {
    await useFakeLiveKit(owner.context());
    const app = await ownerInCall(owner, h);
    const link = await app.locator('#join-url').inputValue();
    const guest = await guestIn(browser, app, link, { width: 1280, height: 800 });
    await expect(app.locator('#badge')).toHaveText(on, { timeout: 15_000 });
    const room = await roomOf(browser);
    // The recording's file has begun (the fake composite reports it at once).
    await expect.poll(async () => {
      const ctx = await browser.newContext();
      const r = await ctx.request.get(`${h.parent}/composite-start?room=${encodeURIComponent(room)}`);
      await ctx.close();
      return r.status();
    }, { timeout: 15_000 }).toBe(200);
    await get(browser, `/publish?room=${encodeURIComponent(room)}&identity=owner`);
    await expect.poll(async () => app.locator('#transcript li.line:not(.partial)').count(), { timeout: 30_000 }).toBe(2);
    await guest.context().close();
    await app.locator('#btn-leave').click();
    await app.locator('#btn-end').click();
    await expect(app.locator('#list-view')).toBeVisible();
    await app.locator('.meeting').first().locator('.transcript-row').click();
    await expect(app.locator('#archive-view')).toBeVisible();
    return app;
  }

  test('the archive plays the recording beside the transcript, and clicking a line seeks the video to it', async ({ browser }) => {
    const owner = await browser.newPage();
    const app = await recordedMeeting(browser, owner);
    const video = app.locator('#archive-video');
    await expect(app.locator('#archive-media')).toBeVisible({ timeout: 15_000 });
    await expect(video).toHaveAttribute('src', /^\/media\/[A-Za-z0-9_-]{16}\/1\?exp=\d+&sig=[A-Za-z0-9_-]+$/);
    // Side by side at 1280 wide: the video's column, then the lines'.
    const vb = (await video.boundingBox())!;
    const lb = (await app.locator('#archive-lines').boundingBox())!;
    expect(vb.x + vb.width).toBeLessThanOrEqual(lb.x);
    // The video really plays: Chromium read its index and knows its length
    // (the fixture is 20 s).
    await expect.poll(() => video.evaluate((v: HTMLVideoElement) => v.readyState), { timeout: 15_000 }).toBeGreaterThanOrEqual(1);
    expect(await video.evaluate((v: HTMLVideoElement) => Math.round(v.duration))).toBe(20);

    const lines = app.locator('#archive-lines li.line.seekable');
    await expect(lines).toHaveCount(2);
    const offsets = await lines.evaluateAll((els) => els.map((el) => Number(el.getAttribute('data-offset-ms'))));
    // Said in order, after the file began, and inside it.
    expect(offsets[0]).toBeGreaterThan(0);
    expect(offsets[1]).toBeGreaterThan(offsets[0]);
    expect(offsets[1]).toBeLessThan(20_000);
    for (const i of [1, 0]) {
      await lines.nth(i).click();
      await expect.poll(() => video.evaluate((v: HTMLVideoElement) => v.currentTime), { timeout: 10_000 }).toBeCloseTo(offsets[i] / 1000, 2);
      await expect(lines.nth(i)).toHaveClass(/playing/);
    }
    // The keyboard does the same.
    await lines.nth(1).focus();
    await owner.keyboard.press('Enter');
    await expect.poll(() => video.evaluate((v: HTMLVideoElement) => v.currentTime), { timeout: 10_000 }).toBeCloseTo(offsets[1] / 1000, 2);
    expect(await video.evaluate((v: HTMLVideoElement) => v.error)).toBeNull();
    await owner.close();
  });

  test('a share link plays the recording with no muster sign-in, and Stop sharing turns it off', async ({ browser }) => {
    const owner = await browser.newPage();
    const app = await recordedMeeting(browser, owner);
    await expect(app.locator('#archive-media')).toBeVisible({ timeout: 15_000 });
    await app.locator('#share-rec').click();
    const shape = new RegExp(`^${h.plugin.replace(/\./g, '\\.')}/r/[A-Za-z0-9_-]{43}$`);
    await expect(app.locator('#share-url')).toHaveValue(shape);
    const url = await app.locator('#share-url').inputValue();
    await expect(app.locator('#share-until')).not.toHaveText('');

    // A stranger's browser: no cookie, no muster.
    const ctx = await browser.newContext();
    const viewer = await ctx.newPage();
    const media: { status: number; range: string }[] = [];
    viewer.on('response', (r) => {
      if (r.url() === url + '/1') media.push({ status: r.status(), range: r.headers()['content-range'] || '' });
    });
    await viewer.goto(url);
    const v = viewer.locator('video.share-video');
    await expect(v).toHaveCount(1);
    await expect.poll(() => v.evaluate((x: HTMLVideoElement) => x.readyState), { timeout: 15_000 }).toBeGreaterThanOrEqual(1);
    expect(await v.evaluate((x: HTMLVideoElement) => Math.round(x.duration))).toBe(20);
    // The video is read in byte ranges from the link's own media route (the
    // store is never named to the browser).
    expect(media.some((m) => m.status === 206 && /^bytes \d+-\d+\/\d+$/.test(m.range)), JSON.stringify(media)).toBe(true);
    await v.evaluate((x: HTMLVideoElement) => { x.currentTime = 12; });
    await expect.poll(() => v.evaluate((x: HTMLVideoElement) => x.currentTime)).toBeCloseTo(12, 1);

    await app.locator('#unshare-rec').click();
    await expect(app.locator('#share-out')).toBeHidden();
    const gone = await viewer.goto(url);
    expect(gone!.status()).toBe(404);
    await expect(viewer.locator('#message')).toHaveText('This recording is not available. The link may have expired or been turned off.');
    await ctx.close();
    await owner.close();
  });
});
