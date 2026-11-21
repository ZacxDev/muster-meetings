// The /enter frame gate in a real browser (design: "/enter refuses unless
// Sec-Fetch-Dest: iframe"; "a real-browser case loads /enter top-level and in
// muster's frame: top-level is refused, framed works"). The plugin runs
// against the fake muster (scripts/e2e-harness); a muster-like parent on
// another origin frames /enter as muster's /apps/<id> page does.
import { test, expect, type APIRequestContext } from '@playwright/test';
import { spawn, type ChildProcess } from 'node:child_process';
import { createInterface } from 'node:readline';
import path from 'node:path';

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

async function redeems(request: APIRequestContext): Promise<number> {
  return Number(await (await request.get(`${parent}/redeems`)).text());
}

test('a top-level /enter is refused with no redeem, and the same code works in muster\'s frame', async ({ page, request }) => {
  const code = await (await request.get(`${parent}/mint`)).text();
  const before = await redeems(request);

  // Top level: the browser sends Sec-Fetch-Dest: document.
  const resp = await page.goto(`${plugin}/enter?muster_code=${encodeURIComponent(code)}&muster_plugin=${pluginID}`);
  expect(resp?.status()).toBe(403);
  await expect(page.locator('#message')).toHaveText('Open Meetings from muster.');
  await expect(page.locator('#new-meeting')).toHaveCount(0);
  expect(await redeems(request)).toBe(before);

  // The same code, framed by the muster-like parent: Sec-Fetch-Dest: iframe.
  await page.goto(`${parent}/apps/${pluginID}?code=${encodeURIComponent(code)}`);
  const app = page.frameLocator('#app');
  await expect(app.locator('#new-meeting')).toBeVisible();
  expect(await redeems(request)).toBe(before + 1);

  // And the app works inside the frame: New meeting shows its link.
  await app.locator('#new-meeting').click();
  await expect(app.locator('#join-url')).toHaveValue(/^http:\/\/127\.0\.0\.1:\d+\/j\/[A-Za-z0-9_-]{22}$/);
});
