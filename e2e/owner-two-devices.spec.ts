// The owner opens the same meeting on a second device. The owner is one
// LiveKit identity ("owner": no subject reaches a guest), so the newer join
// wins: the older page says "Opened on another device" with Use here, and
// Use here takes the call back (moving the other device out), instead of a
// silent kick loop. This needs a real LiveKit (scripts/e2e-harness
// -livekit=real): the binary in MEETINGS_LIVEKIT_SERVER or on PATH. Without
// one the test skips, unless MEETINGS_REQUIRE_LIVEKIT=1 (CI), when it fails.
import { test, expect } from '@playwright/test';
import { spawn, type ChildProcess } from 'node:child_process';
import { createInterface } from 'node:readline';
import path from 'node:path';

let harness: ChildProcess;
let parent = '';
let pluginID = 0;
let realLiveKit = false;

test.use({
  // The harness's LiveKit is on 127.0.0.1, and Chromium leaves loopback out
  // of WebRTC's candidates unless told otherwise.
  launchOptions: { args: ['--use-fake-device-for-media-stream', '--use-fake-ui-for-media-stream', '--allow-loopback-in-peer-connection'] },
  viewport: { width: 1280, height: 800 },
});

test.beforeAll(async () => {
  harness = spawn('go', ['run', './scripts/e2e-harness', '-livekit=real'], {
    cwd: path.resolve(__dirname, '..'),
    stdio: ['pipe', 'pipe', 'inherit'],
  });
  const line = await new Promise<string>((resolve, reject) => {
    const rl = createInterface({ input: harness.stdout! });
    rl.once('line', resolve);
    harness.once('exit', (code) => reject(new Error(`the harness exited ${code} before it started`)));
  });
  ({ parent, plugin_id: pluginID, real_livekit: realLiveKit } = JSON.parse(line));
});

test.afterAll(() => {
  harness?.stdin?.end();
});

// connected waits for a page's call to connect, and on failure says what the
// page said (its status line) rather than only "none".
async function connected(frame: ReturnType<import('@playwright/test').Page['frameLocator']>, who: string) {
  const view = frame.locator('#call-view');
  await expect.poll(async () => view.getAttribute('data-conn'), { timeout: 30_000 }).not.toBe('connecting');
  const state = await view.getAttribute('data-conn');
  const status = await frame.locator('#call-status').textContent();
  expect(state, `${who}: data-conn ${state}, status ${JSON.stringify(status)}`).toBe('connected');
}

test('a second owner device takes the call, and Use here takes it back', async ({ browser }) => {
  if (!realLiveKit) {
    if (process.env.MEETINGS_REQUIRE_LIVEKIT === '1') throw new Error('MEETINGS_REQUIRE_LIVEKIT=1 but the harness found no livekit-server');
    test.skip(true, 'no livekit-server (set MEETINGS_LIVEKIT_SERVER)');
  }
  const first = await browser.newPage();
  first.on('console', (m) => console.log(`[first page] ${m.type()}: ${m.text()}`));
  await first.goto(`${parent}/apps/${pluginID}`);
  const a = first.frameLocator('#app');
  await a.locator('#new-meeting').click();
  await connected(a, 'the first page');
  const rowID = await a.locator('.meeting').first().getAttribute('id');
  const meetingID = rowID!.replace(/^meeting-/, '');

  // The second device: muster's page opened with ?ref=, as a knock
  // notification's tap opens it, goes straight into the call.
  const ctx2 = await browser.newContext();
  const second = await ctx2.newPage();
  second.on('console', (m) => console.log(`[second page] ${m.type()}: ${m.text()}`));
  await second.goto(`${parent}/apps/${pluginID}?ref=${encodeURIComponent(meetingID)}`);
  const b = second.frameLocator('#app');
  await connected(b, 'the second page');

  // The older page says so, and does not rejoin by itself.
  await expect(a.locator('#call-view')).toHaveAttribute('data-conn', 'elsewhere', { timeout: 15_000 });
  await expect(a.locator('#call-status')).toHaveText('Opened on another device.');
  await expect(a.locator('#btn-use-here')).toBeVisible();
  await first.waitForTimeout(3000);
  await expect(b.locator('#call-view')).toHaveAttribute('data-conn', 'connected');
  await expect(a.locator('#call-view')).toHaveAttribute('data-conn', 'elsewhere');

  // Use here: this page takes the call; the other one is told.
  await a.locator('#btn-use-here').click();
  await connected(a, 'the first page, after Use here');
  await expect(b.locator('#call-view')).toHaveAttribute('data-conn', 'elsewhere', { timeout: 15_000 });
  await expect(b.locator('#btn-use-here')).toBeVisible();
  await ctx2.close();
  await first.close();
});
