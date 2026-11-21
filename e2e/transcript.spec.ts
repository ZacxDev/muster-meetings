// The live transcript, seen by the owner and an admitted guest alike
// (design: decision 6, "the same live transcript and action-item panels as
// the owner"), and the archive after the meeting. The plugin runs against
// the fake muster and the fake LiveKit (scripts/e2e-harness), which also
// acts as LiveKit's track Egress: when a microphone is published it streams
// Egress's own PCM (1920-byte messages, S16LE stereo 48 kHz) to the plugin's
// ingest over a websocket whose URL token is its only credential. The
// plugin's FAKE engine (the real endpointer, a scripted line per utterance)
// turns that into lines. No real Egress and no real STT model run here.
import { test, expect, type Browser, type FrameLocator, type Locator } from '@playwright/test';
import { spawn, type ChildProcess } from 'node:child_process';
import { createInterface } from 'node:readline';
import path from 'node:path';
import fs from 'node:fs';

let harness: ChildProcess;
let plugin = '';
let parent = '';
let pluginID = 0;

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

type RTC = { Path: string; Valid: boolean; Claims: { sub: string; name: string; video?: { room?: string } } };

async function joins(browser: Browser): Promise<RTC[]> {
  const ctx = await browser.newContext();
  const r = await ctx.request.get(`${parent}/livekit-joins`);
  const out = (await r.json()) as RTC[];
  await ctx.close();
  return out;
}

// The harness's fake engine script (scripts/e2e-harness: script).
const first = 'Ada opens the meeting.';
const markup = '<b>Not bold</b> & <i>not italic</i>';

// linesOf is a transcript list's lines as "who: text", partials left out.
async function linesOf(list: Locator): Promise<string[]> {
  return list.locator('li.line:not(.partial)').evaluateAll((els) =>
    els.map((el) => `${el.querySelector('.who')?.textContent}: ${el.querySelector('.text')?.textContent}`));
}

test('the owner and a guest see the same transcript lines from the fake engine, as text, and the archive keeps them', async ({ browser }) => {
  const owner = await browser.newPage();
  await owner.goto(`${parent}/apps/${pluginID}`);
  const app: FrameLocator = owner.frameLocator('#app');
  await app.locator('#new-meeting').click();
  await expect(app.locator('#call-view')).toBeVisible();
  const link = await app.locator('#call-url').inputValue();

  const guestCtx = await browser.newContext();
  const guest = await guestCtx.newPage();
  await guest.goto(link);
  await guest.locator('#name').fill('Grace');
  await guest.locator('#ask').click();
  await app.locator('.lobby-row.waiting .admit').click();
  await expect(guest.locator('#guest')).toHaveAttribute('data-state', 'admitted');

  // The guest's identity and the room, from the token LiveKit was shown.
  await expect.poll(async () => (await joins(browser)).filter((j) => j.Claims.sub?.startsWith('guest-')).length, { timeout: 15_000 }).toBeGreaterThan(0);
  const gj = (await joins(browser)).find((j) => j.Claims.sub?.startsWith('guest-'))!;
  const room = gj.Claims.video!.room!;
  expect(room).toMatch(/^meeting-/);

  // No line before anyone speaks; the panels are hidden.
  await expect(app.locator('#panels')).toBeHidden();
  await expect(guest.locator('#panels')).toBeHidden();

  // The owner, then the guest, publish a microphone; each egress streams
  // two utterances.
  const ctx = await browser.newContext();
  for (const identity of ['owner', gj.Claims.sub]) {
    const r = await ctx.request.get(`${parent}/publish?room=${encodeURIComponent(room)}&identity=${encodeURIComponent(identity)}`);
    expect(r.status(), await r.text()).toBe(200);
  }
  await ctx.close();

  const want = [`Ada L.: ${first}`, `Ada L.: ${markup}`, `Grace (guest): ${first}`, `Grace (guest): ${markup}`].sort();
  const ownerList = app.locator('#transcript');
  const guestList = guest.locator('#transcript');
  await expect.poll(async () => (await linesOf(ownerList)).length, { timeout: 30_000 }).toBe(4);
  await expect.poll(async () => (await linesOf(guestList)).length, { timeout: 30_000 }).toBe(4);
  const ownerLines = await linesOf(ownerList);
  const guestLines = await linesOf(guestList);
  // The same lines on both sides, in the same order.
  expect(guestLines).toEqual(ownerLines);
  expect([...ownerLines].sort()).toEqual(want);
  // The line that looks like markup is text: no element came of it.
  await expect(ownerList.locator('b, i')).toHaveCount(0);
  await expect(guestList.locator('b, i')).toHaveCount(0);
  await expect(app.locator('#panels')).toBeVisible();
  await expect(guest.locator('#panels')).toBeVisible();

  // The owner ends the meeting; the guest is told; the archive keeps the
  // lines, and the text download is the same transcript.
  await app.locator('#btn-end').click();
  await expect(guest.locator('#message')).toHaveText('This meeting has ended.');
  await expect(app.locator('#list-view')).toBeVisible();
  await app.locator('.meeting').first().locator('.transcript-row').click();
  await expect(app.locator('#archive-view')).toBeVisible();
  await expect.poll(async () => (await linesOf(app.locator('#archive-lines'))).length).toBe(4);
  expect([...(await linesOf(app.locator('#archive-lines')))].sort()).toEqual(want);
  await expect(app.locator('#archive-lines').locator('b, i')).toHaveCount(0);
  const [dl] = await Promise.all([owner.waitForEvent('download'), app.locator('#dl-txt').click()]);
  const text = fs.readFileSync(await dl.path(), 'utf8');
  // The text export fences each speaker's name, a guest's mark outside it.
  for (const l of [`«Ada L.»: ${first}`, `«Ada L.»: ${markup}`, `«Grace» (guest): ${first}`, `«Grace» (guest): ${markup}`]) {
    expect(text).toContain(`] ${l}\n`);
  }
  await guestCtx.close();
  await owner.close();
});
