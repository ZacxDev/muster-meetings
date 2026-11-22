// Live captions (phase 3c): the plugin's REAL nemotron engine against the
// fake Nemotron server (internal/stttest, which speaks muster's
// images/stt-nemotron protocol exactly), started by
// `scripts/e2e-harness -stt=nemotron`. The fake LiveKit acts as Egress for
// the owner's microphone (Egress's own PCM to the ingest); the engine sends
// it to the fake server in 80 ms frames; the server's partials grow a word per
// 560 ms of audio and its final is the whole line (its closing mark late, as
// the next final's `lead`). In the transcript panel a
// partial caption appears (in progress, no line yet) and is then REPLACED by
// the final line. At a phone's size and a laptop's.
import { test, expect, type Browser } from '@playwright/test';
import { spawn, type ChildProcess } from 'node:child_process';
import { createInterface } from 'node:readline';
import path from 'node:path';

let harness: ChildProcess;
let parent = '';
let pluginID = 0;

test.beforeAll(async () => {
  harness = spawn('go', ['run', './scripts/e2e-harness', '-stt=nemotron'], {
    cwd: path.resolve(__dirname, '..'),
    stdio: ['pipe', 'pipe', 'inherit'],
  });
  const line = await new Promise<string>((resolve, reject) => {
    const rl = createInterface({ input: harness.stdout! });
    rl.once('line', resolve);
    harness.once('exit', (code) => reject(new Error(`the harness exited ${code} before it started`)));
  });
  ({ parent, plugin_id: pluginID } = JSON.parse(line));
});

test.afterAll(() => {
  harness?.stdin?.end();
});

// The fake Nemotron server's first line (scripts/e2e-harness: captionLines).
const line = 'Captions arrive word by word as Ada speaks.';

type RTC = { Claims: { sub: string; video?: { room?: string } } };

async function ownerRoom(browser: Browser, before: number): Promise<string> {
  const ctx = await browser.newContext();
  let room = '';
  await expect.poll(async () => {
    const all = ((await (await ctx.request.get(`${parent}/livekit-joins`)).json()) as RTC[] | null) ?? [];
    const mine = all.slice(before).filter((j) => j.Claims.sub === 'owner');
    room = mine.length ? mine[mine.length - 1].Claims.video?.room ?? '' : '';
    return room;
  }, { timeout: 15_000 }).toMatch(/^meeting-/);
  await ctx.close();
  return room;
}

async function joinCount(browser: Browser): Promise<number> {
  const ctx = await browser.newContext();
  const n = (((await (await ctx.request.get(`${parent}/livekit-joins`)).json()) as RTC[] | null) ?? []).length;
  await ctx.close();
  return n;
}

for (const size of [{ width: 390, height: 844 }, { width: 1280, height: 800 }]) {
  test(`a partial caption appears and is replaced by the final line (${size.width}x${size.height})`, async ({ browser }) => {
    const ctx = await browser.newContext({ viewport: size });
    const owner = await ctx.newPage();
    const before = await joinCount(browser);
    await owner.goto(`${parent}/apps/${pluginID}`);
    const app = owner.frameLocator('#app');
    await app.locator('#new-meeting').click();
    await expect(app.locator('#btn-join')).toBeEnabled({ timeout: 15_000 });
    await app.locator('#btn-join').click();
    await expect(app.locator('#call-view')).toBeVisible();
    const room = await ownerRoom(browser, before);

    const list = app.locator('#transcript');
    const partial = list.locator('li.line.partial');
    const finals = list.locator('li.line:not(.partial)');
    await expect(app.locator('#panels')).toBeHidden();

    const pub = await browser.newContext();
    const r = await pub.request.get(`${parent}/publish?room=${encodeURIComponent(room)}&identity=owner`);
    expect(r.status(), await r.text()).toBe(200);
    await pub.close();

    // A caption in progress: the owner's name, the first words of the line
    // and the in-progress mark, and NO stored line yet.
    await expect(partial).toHaveCount(1, { timeout: 15_000 });
    await expect(app.locator('#panels')).toBeVisible();
    await expect(partial.locator('.who')).toHaveText('Ada L.');
    const caption = (await partial.locator('.text').textContent()) ?? '';
    expect(caption.endsWith('…'), caption).toBe(true);
    expect(line.startsWith(caption.slice(0, -1)), caption).toBe(true);
    expect(caption.slice(0, -1).length).toBeLessThan(line.length);
    await expect(finals).toHaveCount(0);
    // It grows while the speaker goes on.
    await expect.poll(async () => ((await partial.locator('.text').textContent()) ?? '').length, { timeout: 10_000 })
      .toBeGreaterThan(caption.length);

    // The final replaces it: one line, its words, and no caption left. Its
    // closing "." comes late, as the real server sends it (the next final's
    // `lead`, with the next utterance), and the line is amended in place
    // (late-marks.spec.ts).
    await expect(finals).toHaveCount(1, { timeout: 15_000 });
    await expect(finals.first().locator('.text')).toHaveText(line, { timeout: 15_000 });
    await expect(finals.first().locator('.who')).toHaveText('Ada L.');
    await expect(list.locator('li.line.partial', { hasText: 'Captions' })).toHaveCount(0);
    // The panel fits the screen: no sideways scroll.
    const frame = owner.frames().find((f) => f !== owner.mainFrame())!;
    const overflow = await frame.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
    expect(overflow).toBeLessThanOrEqual(0);

    await app.locator('#btn-leave').click();
    await app.locator('#btn-end').click();
    await expect(app.locator('#list-view')).toBeVisible();
    await ctx.close();
  });
}
