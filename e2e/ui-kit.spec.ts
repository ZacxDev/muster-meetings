// muster's UI kit (docs/ui-kit.md): the pages load kit.css and kit.js LIVE
// from the configured muster origin (the harness's parent, serving muster's
// own render of the kit), the install's theme reaches the plugin on its next
// load, and when the kit cannot be loaded the built-in fallback keeps the
// call working. The guest page (top level, not framed) the same.
import { test, expect, type Page } from '@playwright/test';
import { startHarness, useFakeLiveKit, ownerInCall, kitMode, lk, appFrame, type Harness } from './helpers';

let h: Harness;
test.beforeAll(async () => { h = await startHarness(); });
test.afterAll(() => { h?.proc.stdin?.end(); });
test.afterEach(async () => { await kitMode(h, 'theme=brass'); });
test.use({ viewport: { width: 1280, height: 800 } });

// kitRequests records where the plugin's pages asked for the kit.
function kitRequests(page: Page): string[] {
  const seen: string[] = [];
  page.on('request', (r) => { if (/\/ui-kit\/|kit-fallback/.test(r.url())) seen.push(r.url()); });
  return seen;
}

test("the kit comes from the configured muster origin, and its theme is the plugin's", async ({ page }) => {
  await useFakeLiveKit(page.context());
  const seen = kitRequests(page);
  const app = await ownerInCall(page, h);
  const f = appFrame(page);
  expect(seen.sort()).toEqual([`${h.parent}/ui-kit/v1/kit.css`, `${h.parent}/ui-kit/v1/kit.js`]);
  expect(await f.evaluate(() => document.documentElement.getAttribute('data-kit'))).toBe('live');
  expect(await f.evaluate(() => (window as any).MusterKit?.version)).toBe('1.0.0');
  // The default preset's accent on a primary-filled thing (the speaking ring).
  const accent = () => f.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue('--mu-accent').trim());
  expect(await accent()).toBe('228 181 86');
  // The admin picks another preset: the next load draws it, no rebuild.
  await kitMode(h, 'theme=harbor');
  await page.reload();
  await useFakeLiveKit(page.context());
  const app2 = await ownerInCall(page, h);
  const f2 = appFrame(page);
  const harbor = await f2.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue('--mu-accent').trim());
  expect(harbor).not.toBe('228 181 86');
  await lk(f2, 'addRemote', 'guest-a', 'Grace (guest)');
  await lk(f2, 'speakers', ['guest-a']);
  const ring = app2.locator('.tile[data-identity="guest-a"]');
  await expect.poll(() => ring.evaluate((el) => getComputedStyle(el).boxShadow)).toContain(`rgb(${harbor.split(/\s+/).join(', ')})`);
  void app;
});

test('the page CSP lets the kit in from muster and nothing else', async ({ page }) => {
  const resp = await page.request.get(`${h.parent}/apps/${h.pluginID}`); // muster's page: fine
  expect(resp.status()).toBe(200);
  await useFakeLiveKit(page.context());
  let csp = '';
  page.on('response', (r) => { if (r.url().includes('/enter?')) csp = r.headers()['content-security-policy'] ?? ''; });
  await ownerInCall(page, h);
  const dir = (name: string) => csp.split(';').map((d) => d.trim()).find((d) => d.startsWith(name + ' '));
  for (const d of ['script-src', 'style-src', 'img-src']) expect(dir(d)).toBe(`${d} 'self' ${h.parent}`);
  expect(dir('connect-src')).not.toContain(h.parent);
  expect(dir('default-src')).toBe("default-src 'self'");
});

for (const missing of ['css,js', 'css', 'js']) {
  test(`with the kit's ${missing} missing (404), the fallback draws the call and it works`, async ({ page, browser }) => {
    await kitMode(h, `missing=${missing}`);
    await useFakeLiveKit(page.context());
    const seen = kitRequests(page);
    const app = await ownerInCall(page, h);
    const f = appFrame(page);
    const want = missing === 'css,js' ? 'fallback' : missing === 'css' ? 'js' : 'css';
    expect(await f.evaluate(() => document.documentElement.getAttribute('data-kit'))).toBe(want);
    expect(seen.some((u) => u.endsWith('/static/kit-fallback.css'))).toBe(missing.includes('css'));
    // The tokens are there either way, and the controls are drawn: a
    // 44 px target with a visible icon.
    expect(await f.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue('--mu-bg').trim())).not.toBe('');
    const mic = app.locator('#btn-mic');
    const box = (await mic.boundingBox())!;
    expect(box.width).toBeGreaterThanOrEqual(44);
    const icon = (await mic.locator('svg.mu-icon').boundingBox())!;
    expect(icon.width).toBeGreaterThan(10);
    // The icon draws (the sprite it points at has that symbol).
    expect(await mic.locator('svg.mu-icon use').evaluate(async (u) => {
      const href = u.getAttribute('href') || '';
      if (href.startsWith('#')) return !!document.querySelector(href);
      const [file, id] = href.split('#');
      return file === '/static/kit-fallback-icons.svg' && (await (await fetch(file)).text()).includes(`id="${id}"`);
    })).toBe(true);
    // The call works: a guest's mute shows; Mute mutes.
    await lk(f, 'addRemote', 'guest-a', 'Grace (guest)');
    await lk(f, 'mute', 'guest-a', 'microphone', true);
    await expect(app.locator('.tile[data-identity="guest-a"] [data-ind="mic"]')).toBeVisible();
    await mic.click();
    await expect(mic).toHaveAttribute('aria-pressed', 'true');
    // With no kit.js the browser's own tooltip carries the label.
    if (missing.includes('js')) await expect(mic).toHaveAttribute('title', /Mute/);
    // The guest page falls back the same way.
    const link = await app.locator('#join-url').inputValue();
    const g = await browser.newPage();
    await g.goto(link);
    expect(await g.evaluate(() => document.documentElement.getAttribute('data-kit'))).toBe(want);
    await expect(g.locator('#ask')).toBeVisible();
    await g.close();
  });
}
