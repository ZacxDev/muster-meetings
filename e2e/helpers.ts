// Shared by the UI specs (ui-*.spec.ts): the harness, the fake LiveKit
// client, and the owner's and a guest's way into a call.
import { expect, type Browser, type BrowserContext, type FrameLocator, type Page, type Frame } from '@playwright/test';
import { spawn, type ChildProcess } from 'node:child_process';
import { createInterface } from 'node:readline';
import path from 'node:path';

export type Harness = { proc: ChildProcess; plugin: string; parent: string; pluginID: number; realLiveKit: boolean };

export async function startHarness(args: string[] = []): Promise<Harness> {
  const proc = spawn('go', ['run', './scripts/e2e-harness', ...args], {
    cwd: path.resolve(__dirname, '..'),
    stdio: ['pipe', 'pipe', 'inherit'],
  });
  const line = await new Promise<string>((resolve, reject) => {
    const rl = createInterface({ input: proc.stdout! });
    rl.once('line', resolve);
    proc.once('exit', (code) => reject(new Error(`the harness exited ${code} before it started`)));
  });
  const j = JSON.parse(line);
  return { proc, plugin: j.plugin, parent: j.parent, pluginID: j.plugin_id, realLiveKit: !!j.real_livekit };
}

export const fakeClient = path.resolve(__dirname, 'fixtures', 'fake-livekit-client.js');

// useFakeLiveKit serves the fake client in place of the vendored LiveKit
// client, for every page and frame of ctx.
export async function useFakeLiveKit(ctx: BrowserContext) {
  // The page names it with its version (?v=…, internal/server assets.go).
  await ctx.route(/\/static\/vendor\/livekit-client-[^/?]*\.umd\.js(\?|$)/, (route) => route.fulfill({ path: fakeClient, contentType: 'text/javascript' }));
}

export async function kitMode(h: Harness, q: string) {
  const r = await fetch(`${h.parent}/kit-mode?${q}`);
  expect(r.status, await r.text()).toBe(200);
}

export function appFrame(page: Page): Frame {
  const f = page.frames().find((x) => x !== page.mainFrame());
  if (!f) throw new Error('no app frame');
  return f;
}

// ownerPrejoin opens muster's page, presses New meeting, and waits for the
// pre-join's device check.
export async function ownerPrejoin(page: Page, h: Harness): Promise<FrameLocator> {
  await page.goto(`${h.parent}/apps/${h.pluginID}`);
  const app = page.frameLocator('#app');
  await app.locator('#new-meeting').click();
  await expect(app.locator('#prejoin')).toBeVisible();
  await expect(app.locator('#btn-join')).toBeEnabled({ timeout: 15_000 });
  return app;
}

// ownerInCall goes into the call (the fake client: connected at once, with
// the owner's own tile): through the pre-join the first time, straight in
// once the check is remembered.
export async function ownerInCall(page: Page, h: Harness): Promise<FrameLocator> {
  await page.goto(`${h.parent}/apps/${h.pluginID}`);
  const app = page.frameLocator('#app');
  await app.locator('#new-meeting').click();
  await expect(app.locator('#prejoin, #call-view').filter({ visible: true })).toHaveCount(1, { timeout: 15_000 });
  if (await app.locator('#prejoin').isVisible()) {
    await expect(app.locator('#btn-join')).toBeEnabled({ timeout: 15_000 });
    await app.locator('#btn-join').click();
  }
  await expect(app.locator('#call-view')).toBeVisible();
  await expect(app.locator('#call-view')).toHaveAttribute('data-conn', 'connected', { timeout: 15_000 });
  await expect(app.locator('.tile.local[data-source="camera"]')).toHaveCount(1);
  return app;
}

// lk calls window.__lk in the app frame (the fake client's handle).
export async function lk(frame: Frame, fn: string, ...args: unknown[]) {
  return frame.evaluate(([fn, args]) => (window as any).__lk[fn as string](...(args as unknown[])), [fn, args] as const);
}

export async function newPhone(browser: Browser) {
  return browser.newContext({ viewport: { width: 390, height: 844 } });
}

export const sizes = [{ name: 'mobile', width: 390, height: 844 }, { name: 'desktop', width: 1280, height: 800 }];
