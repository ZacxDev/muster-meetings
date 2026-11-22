// muster's UI kit (docs/ui-kit.md), served to the pages from the PLUGIN'S
// origin (/kit/v1/, internal/server kit.go): the plugin fetches it from muster
// (the harness's parent, serving muster's own render of the kit), so the
// install's theme reaches the pages on their next load, while no page ever
// makes the browser contact muster. When muster fails or hangs, the plugin
// serves the kit bundled with it, quickly, and the call works the same.
import { test, expect, type Page, type Response } from '@playwright/test';
import { startHarness, useFakeLiveKit, ownerInCall, kitMode, lk, appFrame, type Harness } from './helpers';

let h: Harness;
test.beforeAll(async () => { h = await startHarness(); });
test.afterAll(() => { h?.proc.stdin?.end(); });
test.afterEach(async () => { await kitMode(h, 'theme=brass'); });
test.use({ viewport: { width: 1280, height: 800 } });

// watch records every request the browser makes to muster's origin, and the
// plugin's kit responses.
function watch(page: Page) {
  const toMuster: string[] = [];
  const kit: Response[] = [];
  page.on('request', (r) => { if (r.url().startsWith(h.parent) && !/\/apps\/\d+/.test(r.url())) toMuster.push(r.url()); });
  page.on('response', (r) => { if (r.url().includes('/kit/v1/')) kit.push(r); });
  return { toMuster, kit };
}

test("the kit comes through the plugin with the install's theme, and the browser never asks muster", async ({ page }) => {
  await useFakeLiveKit(page.context());
  const w = watch(page);
  await ownerInCall(page, h);
  const f = appFrame(page);
  expect(w.toMuster, 'requests from the page to muster').toEqual([]);
  expect(w.kit.map((r) => `${new URL(r.url()).origin === h.plugin} ${new URL(r.url()).pathname} ${r.headers()['x-kit-source']}`).sort())
    .toEqual([`true /kit/v1/kit.css muster`, `true /kit/v1/kit.js muster`]);
  expect(await f.evaluate(() => (window as any).MusterKit?.version)).toBe('1.0.0');
  const accent = () => f.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue('--mu-accent').trim());
  expect(await accent()).toBe('228 181 86'); // brass, the default preset
  // The admin picks another preset: the next load draws it, no rebuild.
  await kitMode(h, 'theme=harbor');
  await useFakeLiveKit(page.context());
  const app2 = await ownerInCall(page, h);
  const f2 = appFrame(page);
  const harbor = await f2.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue('--mu-accent').trim());
  expect(harbor).not.toBe('228 181 86');
  await lk(f2, 'addRemote', 'guest-a', 'Grace (guest)');
  await lk(f2, 'speakers', ['guest-a']);
  const ring = app2.locator('.tile[data-identity="guest-a"]');
  await expect.poll(() => ring.evaluate((el) => getComputedStyle(el).boxShadow)).toContain(`rgb(${harbor.split(/\s+/).join(', ')})`);
  expect(w.toMuster, 'requests from the page to muster').toEqual([]);
});

test("the guest page never contacts muster, and its CSP allows script, style and images from 'self' only", async ({ page, browser }) => {
  await useFakeLiveKit(page.context());
  const app = await ownerInCall(page, h);
  const link = await app.locator('#call-url').inputValue();
  const gctx = await browser.newContext();
  const guest = await gctx.newPage();
  const w = watch(guest);
  const resp = (await guest.goto(link))!;
  const csp = resp.headers()['content-security-policy'] ?? '';
  const dir = (name: string) => csp.split(';').map((d) => d.trim()).find((d) => d.startsWith(name + ' '));
  for (const d of ['script-src', 'style-src', 'img-src']) expect(dir(d)).toBe(`${d} 'self'`);
  expect(csp).not.toContain(h.parent);
  await expect(guest.locator('#ask')).toBeVisible();
  expect(await guest.evaluate(() => (window as any).MusterKit?.version)).toBe('1.0.0');
  expect(w.toMuster).toEqual([]);
  await gctx.close();
});

for (const missing of ['css,js', 'css', 'js']) {
  test(`muster answering 404 for ${missing}: the plugin serves its bundled kit and the call works`, async ({ page, browser }) => {
    await kitMode(h, `missing=${missing}`);
    await useFakeLiveKit(page.context());
    const w = watch(page);
    const app = await ownerInCall(page, h);
    const f = appFrame(page);
    const src = Object.fromEntries(w.kit.map((r) => [new URL(r.url()).pathname, r.headers()['x-kit-source']]));
    // A failed file: the bundled kit, or the last copy from muster (an
    // earlier test fetched one; kit_internal_test pins which is which).
    const failed = /^(bundled|muster-stale)$/;
    expect(src['/kit/v1/kit.css'], 'the kit.css source').toMatch(missing.includes('css') ? failed : /^muster$/);
    expect(src['/kit/v1/kit.js'], 'the kit.js source').toMatch(missing.includes('js') ? failed : /^muster$/);
    expect(await f.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue('--mu-bg').trim())).not.toBe('');
    expect(await f.evaluate(() => (window as any).MusterKit?.version), 'kit.js is installed').toBe('1.0.0');
    // The call works: a guest's mute shows; Mute mutes; icons and tooltips are drawn.
    const mic = app.locator('#btn-mic');
    expect((await mic.boundingBox())!.width).toBeGreaterThanOrEqual(44);
    expect((await mic.locator('svg.mu-icon').boundingBox())!.width).toBeGreaterThan(10);
    expect(await mic.locator('svg.mu-icon use').evaluate((u) => !!document.querySelector(u.getAttribute('href') || '#none'))).toBe(true);
    await lk(f, 'addRemote', 'guest-a', 'Grace (guest)');
    await lk(f, 'mute', 'guest-a', 'microphone', true);
    await expect(app.locator('.tile[data-identity="guest-a"] [data-ind="mic"]')).toBeVisible();
    await mic.click();
    await expect(mic).toHaveAttribute('aria-pressed', 'true');
    await mic.hover();
    await expect(app.locator('.mu-tooltip')).toHaveText('Mute (M)');
    const g = await browser.newPage();
    await g.goto(await app.locator('#call-url').inputValue());
    await expect(g.locator('#ask')).toBeVisible();
    expect(await g.evaluate(() => (window as any).MusterKit?.version)).toBe('1.0.0');
    await g.close();
  });
}

test('muster hanging: the page is not held on it (the plugin gives up after its 2 s fetch timeout and serves the bundled kit)', async ({ browser }) => {
  await kitMode(h, 'slow=15000');
  const ctx = await browser.newContext();
  const page = await ctx.newPage();
  // A fresh owner call takes the kit through the plugin; time the guest page,
  // a first load in its own context.
  await useFakeLiveKit(ctx);
  const app = await ownerInCall(page, h); // itself waits on the kit: ~2 s, not 15
  const link = await app.locator('#call-url').inputValue();
  const g = await browser.newContext();
  const guest = await g.newPage();
  const w = watch(guest);
  const start = Date.now();
  await guest.goto(link);
  await expect(guest.locator('#ask')).toBeVisible();
  const took = Date.now() - start;
  expect(took, `the guest page took ${took} ms with muster hanging 15 s`).toBeLessThan(6000);
  const css = w.kit.find((r) => new URL(r.url()).pathname === '/kit/v1/kit.css')!;
  expect(css.headers()['x-kit-source']).toMatch(/^(bundled|muster-stale)$/);
  await g.close();
  await ctx.close();
});
