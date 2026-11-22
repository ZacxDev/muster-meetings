// Late closing marks: the model behind muster's stt-nemotron emits a
// sentence's closing mark (. ? ! ,) only when the NEXT speech begins, so the
// server sends a line's final at once WITHOUT it and the mark arrives as the
// next final's `lead`. The plugin then amends the line it has already stored
// and shown. Here the plugin's REAL nemotron engine runs against the fake
// Nemotron server (internal/stttest), which sends marks exactly so, started
// by `scripts/e2e-harness -stt=nemotron` with a script whose first line is a
// question. The owner and an admitted guest each have the panel open: both
// see line 1 first without its "?", then the SAME element (no reload, no new
// line) ends with "?" once line 2's final comes.
import { test, expect, type Browser, type FrameLocator, type Locator, type ElementHandle } from '@playwright/test';
import { spawn, type ChildProcess } from 'node:child_process';
import { createInterface } from 'node:readline';
import path from 'node:path';

const question = 'Can everyone hear me clearly?';
const next = 'Alice will send the budget by Friday';

let harness: ChildProcess;
let parent = '';
let pluginID = 0;

test.beforeAll(async () => {
  harness = spawn('go', ['run', './scripts/e2e-harness', '-stt=nemotron', `-nemotron-script=${question}|${next}`], {
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

type RTC = { Claims: { sub: string; video?: { room?: string } } };

async function joins(browser: Browser): Promise<RTC[]> {
  const ctx = await browser.newContext();
  const out = (((await (await ctx.request.get(`${parent}/livekit-joins`)).json()) as RTC[] | null) ?? []);
  await ctx.close();
  return out;
}

test('a line already shown gets its late "?" in place on every open panel, without a reload', async ({ browser }) => {
  const owner = await browser.newPage();
  await owner.goto(`${parent}/apps/${pluginID}`);
  const app: FrameLocator = owner.frameLocator('#app');
  await app.locator('#new-meeting').click();
  const link = await app.locator('#join-url').inputValue();
  await expect(app.locator('#btn-join')).toBeEnabled({ timeout: 15_000 });
  await app.locator('#btn-join').click();
  await expect(app.locator('#call-view')).toBeVisible();

  const guestCtx = await browser.newContext();
  const guest = await guestCtx.newPage();
  await guest.goto(link);
  await guest.locator('#name').fill('Grace');
  await expect(guest.locator('#ask')).toBeEnabled({ timeout: 15_000 });
  await guest.locator('#ask').click();
  await app.locator('#btn-people').click();
  await app.locator('.lobby-row.waiting .admit').click();
  await expect(guest.locator('#guest')).toHaveAttribute('data-state', 'admitted');
  await expect.poll(async () => (await joins(browser)).filter((j) => j.Claims.sub === 'owner').length, { timeout: 15_000 }).toBeGreaterThan(0);
  const room = (await joins(browser)).find((j) => j.Claims.sub === 'owner')!.Claims.video!.room!;

  // Count page loads: an amend must arrive on the open page, not by a reload.
  let loads = 0;
  owner.on('framenavigated', () => loads++);
  guest.on('framenavigated', () => loads++);

  const pub = await browser.newContext();
  const r = await pub.request.get(`${parent}/publish?room=${encodeURIComponent(room)}&identity=owner`);
  expect(r.status(), await r.text()).toBe(200);
  await pub.close();

  const panels: [string, Locator][] = [['owner', app.locator('#transcript')], ['guest', guest.locator('#transcript')]];
  const firsts: ElementHandle[] = [];
  for (const [who, list] of panels) {
    // Line 1 as its final stored it: the words, and not yet the mark.
    const first = list.locator('li.line:not(.partial)').first();
    await expect(first.locator('.text'), who).toHaveText(question.slice(0, -1), { timeout: 20_000 });
    firsts.push((await first.elementHandle())!);
  }
  for (const [i, [who, list]] of panels.entries()) {
    // Line 2's final carries the "?": line 1 ends with it, in place.
    const finals = list.locator('li.line:not(.partial)');
    await expect(finals.first().locator('.text'), who).toHaveText(question, { timeout: 20_000 });
    await expect(finals.nth(1).locator('.text'), who).toHaveText(next);
    await expect(finals, who).toHaveCount(2);
    // The very element that was shown before: amended, not redrawn.
    expect(await firsts[i].evaluate((el) => el.isConnected && el.querySelector('.text')?.textContent), who).toBe(question);
  }
  expect(loads).toBe(0);

  await guestCtx.close();
  await owner.close();
});
