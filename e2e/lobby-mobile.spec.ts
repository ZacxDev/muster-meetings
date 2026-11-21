// A guest knocks and the owner admits them, both on a 390×844 phone screen
// (design: "Mobile layout"; "at 390×844 the call, the controls and the lobby
// row are all on screen without scrolling"). The plugin runs against the
// fake muster and the fake LiveKit (scripts/e2e-harness): the fake LiveKit
// carries no media, but it records, verified, every token a browser connects
// with, so "admit gives the guest a token" is asserted at LiveKit's door, not
// inferred from the page.
import { test, expect, type Browser } from '@playwright/test';
import { spawn, type ChildProcess } from 'node:child_process';
import { createInterface } from 'node:readline';
import path from 'node:path';

let harness: ChildProcess;
let plugin = '';
let parent = '';
let pluginID = 0;

const phone = { width: 390, height: 844 };
test.use({ viewport: phone });

test.beforeAll(async () => {
  harness = spawn('go', ['run', './scripts/e2e-harness'], {
    cwd: path.resolve(__dirname, '..'),
    stdio: ['pipe', 'pipe', 'inherit'],
  });
  const line = await new Promise<string>((resolve, reject) => {
    const rl = createInterface({ input: harness.stdout! });
    rl.once('line', resolve);
    harness.once('exit', (code) => reject(new Error(`the harness exited ${code} before it started`)));
  });
  ({ plugin, parent, plugin_id: pluginID } = JSON.parse(line));
});

test.afterAll(() => {
  harness?.stdin?.end();
});

type RTC = { Path: string; Valid: boolean; Claims: { sub: string; name: string; video?: Record<string, unknown> } };

async function joins(browser: Browser): Promise<RTC[]> {
  const ctx = await browser.newContext();
  const r = await ctx.request.get(`${parent}/livekit-joins`);
  const out = (await r.json()) as RTC[];
  await ctx.close();
  return out;
}

test('at 390×844 a guest knocks, the lobby row shows their name as text, and Admit gives them a guest token', async ({ browser }) => {
  // The owner: muster's page framing the app, on a phone.
  const owner = await browser.newPage({ viewport: phone });
  await owner.goto(`${parent}/apps/${pluginID}`);
  const app = owner.frameLocator('#app');
  await app.locator('#new-meeting').click();
  // New meeting opens the pre-join, with the link to share on it.
  await expect(app.locator('#prejoin')).toBeVisible();
  const link = await app.locator('#join-url').inputValue();
  expect(link).toMatch(/^http:\/\/127\.0\.0\.1:\d+\/j\/[A-Za-z0-9_-]{22}$/);
  await expect(app.locator('#btn-join')).toBeEnabled({ timeout: 15_000 });
  await app.locator('#btn-join').click();
  await expect(app.locator('#call-view')).toBeVisible();

  // The guest: a fresh context, no cookies, a phone.
  const guestCtx = await browser.newContext({ viewport: phone });
  const guest = await guestCtx.newPage();
  await guest.goto(link);
  // The notice comes before the name field, and names what is on.
  const notice = guest.locator('#notice');
  await expect(notice).toContainText('This meeting is recorded and transcribed.');
  await expect(notice).toContainText('sent to an AI service (OpenRouter)');
  const noticeBox = (await notice.boundingBox())!;
  const nameBox = (await guest.locator('#name').boundingBox())!;
  expect(noticeBox.y + noticeBox.height).toBeLessThanOrEqual(nameBox.y);
  await expect(guest.locator('#whose')).toHaveText('Ada L.’s meeting');

  // A name that looks like markup is just a name.
  const typed = '<b>Grace</b>';
  await guest.locator('#name').fill(typed);
  await expect(guest.locator('#ask')).toBeEnabled({ timeout: 15_000 }); // the device check
  await guest.locator('#ask').click();
  await expect(guest.locator('#guest')).toHaveAttribute('data-state', 'waiting');
  await expect(guest.locator('#guest-status')).toHaveText('Waiting for Ada L. to let you in.');

  // The knock is a count on People and a toast naming them, as text.
  await expect(app.locator('#lobby-count')).toHaveText('1');
  await expect(app.locator('#btn-people')).toHaveAccessibleName('People, 1 waiting');
  await expect(app.locator('.mu-toast')).toContainText(`${typed} wants to join`);
  await expect(app.locator('.mu-toast b')).toHaveCount(0);
  // People opens the lobby: the row is on screen without scrolling, with the
  // name as text (no <b> element), "(guest)" after it.
  await app.locator('#btn-people').click();
  const row = app.locator('.lobby-row.waiting');
  await expect(row).toBeVisible();
  await expect(row.locator('.who')).toHaveText(`${typed} (guest)`);
  await expect(row.locator('b')).toHaveCount(0);
  await expect(row).toBeInViewport({ ratio: 1 });
  await expect(row.locator('.admit')).toBeInViewport({ ratio: 1 });
  await expect(row.locator('.admit')).toHaveAccessibleName(`Admit ${typed}`);
  await expect(app.locator('#btn-leave')).toBeInViewport({ ratio: 1 });
  await expect(app.locator('#badge')).toHaveText('Recording · Transcribing · AI');
  const before = (await joins(browser)).filter((j) => j.Claims.sub?.startsWith('guest-'));
  expect(before).toHaveLength(0); // no guest token before the admit

  await row.locator('.admit').click();

  // The guest is in: their page connects to LiveKit with a valid GUEST token.
  await expect(guest.locator('#guest')).toHaveAttribute('data-state', 'admitted');
  await expect(guest.locator('#call-view')).toBeVisible();
  await expect.poll(async () => (await joins(browser)).filter((j) => j.Claims.sub?.startsWith('guest-')).length, { timeout: 15_000 }).toBeGreaterThan(0);
  const tokens = (await joins(browser)).filter((j) => j.Claims.sub?.startsWith('guest-'));
  for (const j of tokens) {
    expect(j.Valid).toBe(true);
    expect(j.Claims.name).toBe(`${typed} (guest)`);
    expect(j.Claims.video).toMatchObject({ roomJoin: true, roomAdmin: false, canPublishData: false, hidden: false, recorder: false });
  }
  // The guest's controls fit the phone screen, and so does the badge.
  await expect(guest.locator('#btn-leave')).toBeInViewport({ ratio: 1 });
  await expect(guest.locator('#badge')).toHaveText('Recording · Transcribing · AI');
  // The owner's row moved from waiting to in the call.
  await expect(app.locator('.lobby-row.admitted .who')).toHaveText(`${typed} (guest)`);

  // The fake LiveKit carries no media, so the guest's connection fails:
  // "Can't connect media" with Retry (design: "Failure modes"). Retry gets a
  // fresh guest token from /events and connects again, with no new knock.
  await expect(guest.locator('#btn-retry')).toBeVisible({ timeout: 30_000 });
  await expect(guest.locator('#call-status')).toContainText("Can't connect media");
  const beforeRetry = (await joins(browser)).filter((j) => j.Claims.sub?.startsWith('guest-')).length;
  await guest.locator('#btn-retry').click();
  await expect.poll(async () => (await joins(browser)).filter((j) => j.Claims.sub?.startsWith('guest-')).length, { timeout: 15_000 }).toBeGreaterThan(beforeRetry);
  const after = (await joins(browser)).filter((j) => j.Claims.sub?.startsWith('guest-'));
  expect(new Set(after.map((j) => j.Claims.sub)).size).toBe(1); // the same identity: no new knock
  await expect(guest.locator('#guest')).toHaveAttribute('data-state', 'admitted');
  await expect(app.locator('.lobby-row')).toHaveCount(1);
  await expect(app.locator('.lobby-row.waiting')).toHaveCount(0);
  await guestCtx.close();
  await owner.close();
});
