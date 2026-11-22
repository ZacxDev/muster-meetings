// The owner's device check happens ONCE: the first New meeting goes through
// the pre-join; Join remembers the choice (static/prefs.js, one localStorage
// key on the plugin's origin) and every later New meeting goes straight into
// the call with the link on screen. A remembered device that is gone, or a
// permission refused since, joins anyway with what works and says so. Storage
// that is blocked means the check again. A guest ALWAYS gets the check. And
// an unanswered camera prompt never holds Join (or Ask to join) for good.
// The fake muster and the fake LiveKit client, as ui-call.spec.ts.
import { test, expect, type Page } from '@playwright/test';
import { startHarness, useFakeLiveKit, ownerPrejoin, ownerInCall, lk, appFrame, type Harness } from './helpers';

let h: Harness;
test.beforeAll(async () => { h = await startHarness(); });
test.afterAll(() => { h?.proc.stdin?.end(); });
test.use({ viewport: { width: 1280, height: 800 } });

const KEY = 'meetings.devices.v1';

async function stored(page: Page) {
  return appFrame(page).evaluate((k) => { const o: Record<string, string> = {}; for (let i = 0; i < localStorage.length; i++) { const n = localStorage.key(i)!; o[n] = localStorage.getItem(n)!; } return { all: o, mine: o[k] }; }, KEY);
}

async function newMeeting(page: Page) {
  const app = page.frameLocator('#app');
  await app.locator('#new-meeting').click();
  await expect(app.locator('#prejoin, #call-view').filter({ visible: true })).toHaveCount(1, { timeout: 15_000 });
  return app;
}

test('the first New meeting shows the check; Join remembers it; the next goes straight in with the link on screen', async ({ page }) => {
  await useFakeLiveKit(page.context());
  const app = await ownerPrejoin(page, h);
  await expect(app.locator('#prejoin')).toHaveAttribute('data-check', 'ready');
  // Microphone off at the check: that is remembered too.
  await app.locator('#pre-mic').click();
  await app.locator('#btn-join').click();
  await expect(app.locator('#call-view')).toHaveAttribute('data-conn', 'connected');
  let f = appFrame(page);
  expect(await f.evaluate(() => (window as any).__lk.calls)).toEqual(['camera=true']);
  // Storage holds that one key, and only the five device fields.
  const s1 = await stored(page);
  expect(Object.keys(s1.all), 'the finished check was remembered, under one key').toEqual([KEY]);
  const v = JSON.parse(s1.mine);
  expect(Object.keys(v).sort()).toEqual(['cam', 'camId', 'mic', 'micId', 'v']);
  expect(v).toMatchObject({ v: 1, mic: false, cam: true });
  expect(v.camId).not.toBe('');

  // Back to the list; New meeting again: no check, in the call, the link shown.
  await app.locator('#btn-leave').click();
  await app.locator('#btn-back').click();
  await expect(app.locator('#list-view')).toBeVisible();
  await f.evaluate(() => { (window as any).__lk.calls.length = 0; }); // the same page: count this call's only
  await newMeeting(page);
  await expect(app.locator('#prejoin')).toBeHidden();
  await expect(app.locator('#call-view')).toHaveAttribute('data-conn', 'connected');
  await expect(app.locator('#invite')).toBeVisible();
  await expect(app.locator('#call-url')).toHaveValue(/^http:\/\/127\.0\.0\.1:\d+\/j\/[A-Za-z0-9_-]{22}$/);
  f = appFrame(page);
  expect(await f.evaluate(() => (window as any).__lk.calls)).toEqual(['camera=true']); // mic stays off, as chosen
  expect(await f.evaluate(() => (window as any).__lk.room().options.videoCaptureDefaults?.deviceId)).toBe(v.camId);
  // The invite steps aside when someone joins.
  await lk(f, 'addRemote', 'guest-a', 'Grace (guest)');
  await expect(app.locator('#invite')).toBeHidden();

  // The devices stay reachable in the call: the settings popover's pickers.
  await app.locator('#btn-settings').click();
  await expect(app.locator('#settings-pop')).toBeVisible();
  await expect(app.locator('#set-cam option')).not.toHaveCount(0);
  await expect(app.locator('#set-mic option')).not.toHaveCount(0);
  const micId = await app.locator('#set-mic').evaluate((s: HTMLSelectElement) => { s.dispatchEvent(new Event('change')); return s.value; });
  await expect.poll(() => f.evaluate(() => (window as any).__lk.calls.join(' '))).toContain(`switch audioinput=${micId}`);
  await expect.poll(async () => JSON.parse((await stored(page)).mine).micId).toBe(micId);
});

test('a remembered camera that is gone, and a permission refused since: in anyway, with what works, and told', async ({ page }) => {
  await page.context().addInitScript((k) => {
    try { localStorage.setItem(k, JSON.stringify({ v: 1, mic: true, cam: true, micId: 'a-microphone-unplugged-long-ago', camId: 'a-camera-unplugged-long-ago' })); } catch (e) { /* the parent's origin */ }
    (window as any).__lkPreset = { refuse: { camera: 'NotAllowedError' } };
  }, KEY);
  await useFakeLiveKit(page.context());
  await page.goto(`${h.parent}/apps/${h.pluginID}`);
  const app = await newMeeting(page);
  await expect(app.locator('#prejoin')).toBeHidden();
  await expect(app.locator('#call-view')).toHaveAttribute('data-conn', 'connected');
  const f = appFrame(page);
  // The stale ids were dropped (the browser's default is used) …
  expect(await f.evaluate(() => (window as any).__lk.room().options.videoCaptureDefaults)).toBeUndefined();
  expect(await f.evaluate(() => (window as any).__lk.room().options.audioCaptureDefaults)).toBeUndefined();
  // … the microphone works, the refused camera is said, and the call goes on.
  await expect.poll(() => f.evaluate(() => (window as any).__lk.calls)).toEqual(expect.arrayContaining(['microphone=true', 'camera=true']));
  await expect(app.locator('#btn-mic')).toHaveAttribute('aria-pressed', 'false');
  await expect(app.locator('#btn-cam')).toHaveAttribute('aria-pressed', 'true');
  await expect(app.locator('.mu-toast')).toContainText('camera or microphone could not start');
  await expect(app.locator('#call-status')).toContainText('Your camera or microphone is off');
});

test('storage that is blocked means the check, every time', async ({ page }) => {
  await page.context().addInitScript(() => {
    Object.defineProperty(window, 'localStorage', { get() { throw new DOMException('blocked', 'SecurityError'); } });
  });
  await useFakeLiveKit(page.context());
  const app = await ownerPrejoin(page, h);
  await app.locator('#btn-join').click();
  await expect(app.locator('#call-view')).toHaveAttribute('data-conn', 'connected');
  await app.locator('#btn-leave').click();
  await app.locator('#btn-back').click();
  await newMeeting(page);
  await expect(app.locator('#prejoin')).toBeVisible();
  await expect(app.locator('#btn-join')).toBeEnabled({ timeout: 15_000 });
});

test('a guest always gets the check, even with a remembered choice on their browser', async ({ page, browser }) => {
  await useFakeLiveKit(page.context());
  const app = await ownerPrejoin(page, h);
  const link = await app.locator('#join-url').inputValue();
  const g = await browser.newContext();
  await g.addInitScript((k) => {
    localStorage.setItem(k, JSON.stringify({ v: 1, mic: false, cam: false, micId: '', camId: '' }));
  }, KEY);
  const guest = await g.newPage();
  await guest.goto(link);
  expect(await guest.evaluate((k) => localStorage.getItem(k), KEY)).not.toBeNull(); // the control: it is there
  await expect(guest.locator('#join-view')).toHaveAttribute('data-check', 'ready', { timeout: 15_000 });
  await expect(guest.locator('#pre-tile')).toHaveAttribute('data-video', 'on');
  await g.close();
});

test('an unanswered camera prompt does not hold Join, or Ask to join, for good', async ({ page, browser }) => {
  const never = () => {
    if (navigator.mediaDevices) navigator.mediaDevices.getUserMedia = () => new Promise(() => {});
  };
  await page.context().addInitScript(never);
  await useFakeLiveKit(page.context());
  await page.goto(`${h.parent}/apps/${h.pluginID}`);
  const app = page.frameLocator('#app');
  await app.locator('#new-meeting').click();
  await expect(app.locator('#prejoin')).toHaveAttribute('data-check', 'checking');
  await expect(app.locator('#btn-join')).toBeDisabled();
  await expect(app.locator('#prejoin')).toHaveAttribute('data-check', 'pending', { timeout: 10_000 });
  await expect(app.locator('#btn-join')).toBeEnabled();
  await expect(app.locator('#pre-status')).toContainText('You can join without them');
  await app.locator('#btn-join').click();
  await expect(app.locator('#call-view')).toHaveAttribute('data-conn', 'connected');
  expect(await appFrame(page).evaluate(() => (window as any).__lk.calls)).toEqual([]);
  // Not remembered: an unanswered check is asked again.
  expect(await appFrame(page).evaluate((k) => localStorage.getItem(k), KEY)).toBeNull();

  const link = await app.locator('#join-url').inputValue();
  const g = await browser.newContext();
  await g.addInitScript(never);
  const guest = await g.newPage();
  await guest.goto(link);
  await guest.locator('#name').fill('Grace');
  await expect(guest.locator('#ask')).toBeDisabled();
  await expect(guest.locator('#ask')).toBeEnabled({ timeout: 10_000 });
  await g.close();
});

test('a guest: the consent notice sits above the preview, and nothing leaves the page for LiveKit before the owner admits them', async ({ page, browser }) => {
  await useFakeLiveKit(page.context());
  const app = await ownerInCall(page, h);
  const link = await app.locator('#call-url').inputValue();
  const g = await browser.newContext();
  await useFakeLiveKit(g);
  const guest = await g.newPage();
  const elsewhere: string[] = [];
  guest.on('request', (r) => { if (new URL(r.url()).origin !== h.plugin) elsewhere.push(r.url()); });
  guest.on('websocket', (ws) => elsewhere.push(ws.url()));
  await guest.goto(link);
  await expect(guest.locator('#join-view')).toHaveAttribute('data-check', 'ready', { timeout: 15_000 });
  // The notice first, the camera preview after it.
  const notice = (await guest.locator('#notice').boundingBox())!;
  const preview = (await guest.locator('#pre-tile').boundingBox())!;
  expect(notice.y + notice.height, 'the notice ends above the preview').toBeLessThanOrEqual(preview.y);
  await expect(guest.locator('#notice')).toContainText('recorded');
  // Knock and wait: the preview is local only; no room, no publish.
  await guest.locator('#name').fill('Grace');
  await guest.locator('#ask').click();
  await expect(guest.locator('#guest')).toHaveAttribute('data-state', 'waiting');
  await guest.waitForTimeout(500);
  expect(await guest.evaluate(() => (window as any).__lk.rooms.length), 'LiveKit rooms made before admission').toBe(0);
  expect(await guest.evaluate(() => (window as any).__lk.calls), 'tracks published before admission').toEqual([]);
  expect(elsewhere, 'requests from the guest page to anywhere but the plugin').toEqual([]);
  // Admitted: now, and only now, the room and the tracks.
  await app.locator('#btn-people').click();
  await app.locator('.lobby-row.waiting .admit').click();
  await expect(guest.locator('#guest')).toHaveAttribute('data-state', 'admitted');
  await expect.poll(() => guest.evaluate(() => (window as any).__lk.rooms.length)).toBe(1);
  await expect.poll(() => guest.evaluate(() => (window as any).__lk.calls)).toEqual(expect.arrayContaining(['microphone=true', 'camera=true']));
  await g.close();
});
