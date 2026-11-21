// The meeting UI (the UI/UX pass): the pre-join, the tiles' indicators, the
// active speaker's ring, the layouts and screen share, the connection
// quality, the keyboard shortcuts, reduced motion, accessible names and axe.
// The plugin runs against the fake muster (scripts/e2e-harness, which also
// serves muster's UI kit) and the FAKE LiveKit client
// (fixtures/fake-livekit-client.js): the spec plays the room's other people.
// The indicators against a real LiveKit are ui-real-livekit.spec.ts.
import { test, expect, type Frame, type FrameLocator, type Page } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { startHarness, useFakeLiveKit, ownerInCall, ownerPrejoin, lk, appFrame, sizes, type Harness } from './helpers';

let h: Harness;
test.beforeAll(async () => { h = await startHarness(); });
test.afterAll(() => { h?.proc.stdin?.end(); });

const tileOf = (app: FrameLocator, id: string, source = 'camera') => app.locator(`.tile[data-identity="${id}"][data-source="${source}"]:not(.leaving)`);

async function setup(page: Page): Promise<{ app: FrameLocator; f: Frame }> {
  await useFakeLiveKit(page.context());
  const app = await ownerInCall(page, h);
  return { app, f: appFrame(page) };
}

for (const size of sizes) {
  test.describe(`at ${size.width}x${size.height}`, () => {
    test.use({ viewport: { width: size.width, height: size.height } });

    test('the pre-join holds Join until the device check is done, then shows the preview, the level and the devices', async ({ page }) => {
      await useFakeLiveKit(page.context());
      // Hold getUserMedia until the spec lets it go, to see the "checking" state.
      await page.context().addInitScript(() => {
        const md = navigator.mediaDevices;
        if (!md) return;
        const real = md.getUserMedia.bind(md);
        let release: () => void = () => {};
        const gate = new Promise<void>((r) => { release = r; });
        (window as any).__releaseMedia = () => release();
        md.getUserMedia = (c: MediaStreamConstraints) => gate.then(() => real(c));
      });
      await page.goto(`${h.parent}/apps/${h.pluginID}`);
      const app = page.frameLocator('#app');
      await app.locator('#new-meeting').click();
      await expect(app.locator('#prejoin')).toHaveAttribute('data-check', 'checking');
      await expect(app.locator('#btn-join')).toBeDisabled();
      await expect(app.locator('#pre-checking')).toBeVisible();
      // The link is there to share before anything else.
      await expect(app.locator('#join-url')).toHaveValue(/\/j\/[A-Za-z0-9_-]{22}$/);
      await appFrame(page).evaluate(() => (window as any).__releaseMedia());
      await expect(app.locator('#prejoin')).toHaveAttribute('data-check', 'ready');
      await expect(app.locator('#btn-join')).toBeEnabled();
      await expect(app.locator('#pre-tile')).toHaveAttribute('data-video', 'on');
      // The fake microphone's tone moves the meter.
      await expect.poll(async () => Number(await app.locator('#mic-level').getAttribute('aria-valuenow')), { timeout: 10_000 }).toBeGreaterThan(0);
      // Each picker lists the fake device.
      await expect(app.locator('#pick-cam option')).not.toHaveCount(0);
      await expect(app.locator('#pick-mic option')).not.toHaveCount(0);
      // The toggles: muted is pressed, and the meter drops to 0; camera off shows the avatar.
      await app.locator('#pre-mic').click();
      await expect(app.locator('#pre-mic')).toHaveAttribute('aria-pressed', 'true');
      await expect(app.locator('#mic-level')).toHaveAttribute('aria-valuenow', '0');
      await app.locator('#pre-cam').click();
      await expect(app.locator('#pre-cam')).toHaveAttribute('aria-pressed', 'true');
      await expect(app.locator('#pre-tile')).toHaveAttribute('data-video', 'off');
      await expect(app.locator('#pre-avatar')).toBeVisible();
      // Join publishes what the pre-join chose: neither.
      await app.locator('#btn-join').click();
      await expect(app.locator('#call-view')).toHaveAttribute('data-conn', 'connected');
      const f = appFrame(page);
      expect(await f.evaluate(() => (window as any).__lk.calls)).toEqual([]);
      await expect(app.locator('#btn-mic')).toHaveAttribute('aria-pressed', 'true');
      await expect(app.locator('#btn-cam')).toHaveAttribute('aria-pressed', 'true');
      const me = app.locator('.tile.local[data-source="camera"]');
      await expect(me).toHaveAttribute('data-mic', 'off');
      await expect(me).toHaveAttribute('data-cam', 'off');
    });

    test("a remote person's mute and camera-off show on their tile within 1 s, and unmute restores it", async ({ page }) => {
      const { app, f } = await setup(page);
      await lk(f, 'addRemote', 'guest-a', 'Grace (guest)', { quality: 'good' });
      const t = tileOf(app, 'guest-a');
      await expect(t).toBeVisible();
      await expect(t.locator('.mu-tile__label')).toHaveText('Grace (guest)');
      await expect(t.locator('[data-ind="mic"]')).toBeHidden();
      await expect(t.locator('[data-ind="cam"]')).toBeHidden();
      await expect(t).toHaveAttribute('data-video', 'on');

      await lk(f, 'mute', 'guest-a', 'microphone', true);
      await lk(f, 'mute', 'guest-a', 'camera', true);
      await expect(t.locator('[data-ind="mic"]')).toBeVisible({ timeout: 1000 });
      await expect(t.locator('[data-ind="mic"]')).toHaveAttribute('aria-label', 'Microphone off', { timeout: 1000 });
      await expect(t.locator('[data-ind="cam"]')).toBeVisible({ timeout: 1000 });
      await expect(t.locator('[data-ind="cam"]')).toHaveAttribute('aria-label', 'Camera off', { timeout: 1000 });
      // Camera off: the initials, not a frozen picture.
      await expect(t).toHaveAttribute('data-video', 'off', { timeout: 1000 });
      await expect(t.locator('.mu-avatar')).toHaveText('G');
      await expect(t.locator('.mu-avatar')).toBeVisible();

      await lk(f, 'mute', 'guest-a', 'microphone', false);
      await lk(f, 'mute', 'guest-a', 'camera', false);
      await expect(t.locator('[data-ind="mic"]')).toBeHidden({ timeout: 1000 });
      await expect(t.locator('[data-ind="cam"]')).toBeHidden({ timeout: 1000 });
      await expect(t).toHaveAttribute('data-video', 'on', { timeout: 1000 });
      await expect(t.locator('.mu-avatar')).toBeHidden();
    });

    test('a name that looks like markup is text on the tile', async ({ page }) => {
      const { app, f } = await setup(page);
      await lk(f, 'addRemote', 'guest-m', '<img src=x onerror=alert(1)> (guest)', { cam: false });
      const t = tileOf(app, 'guest-m');
      await expect(t.locator('.mu-tile__label')).toHaveText('<img src=x onerror=alert(1)> (guest)');
      await expect(t.locator('img')).toHaveCount(0);
    });

    test('the active speaker ring follows whoever is speaking', async ({ page }) => {
      const { app, f } = await setup(page);
      await lk(f, 'addRemote', 'guest-a', 'Grace (guest)');
      await lk(f, 'addRemote', 'guest-b', 'Linus (guest)');
      const a = tileOf(app, 'guest-a'), b = tileOf(app, 'guest-b');
      await expect(app.locator('.tile.mu-tile--speaking')).toHaveCount(0);
      await lk(f, 'speakers', ['guest-a']);
      await expect(a).toHaveClass(/mu-tile--speaking/);
      await expect(b).not.toHaveClass(/mu-tile--speaking/);
      // The ring is drawn in the theme's accent.
      const accent = await a.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue('--mu-accent').trim().split(/\s+/).join(', '));
      await expect.poll(() => a.evaluate((el) => getComputedStyle(el).boxShadow)).toContain(`rgb(${accent})`);
      await lk(f, 'speakers', ['guest-b']);
      await expect(b).toHaveClass(/mu-tile--speaking/);
      await expect(a).not.toHaveClass(/mu-tile--speaking/);
      await lk(f, 'speakers', []);
      await expect(app.locator('.tile.mu-tile--speaking')).toHaveCount(0);
    });

    test('the connection indicator on each tile follows LiveKit’s connection quality', async ({ page }) => {
      const { app, f } = await setup(page);
      await lk(f, 'addRemote', 'guest-a', 'Grace (guest)');
      const q = tileOf(app, 'guest-a').locator('[data-ind="quality"]');
      await expect(q).toBeHidden(); // unknown: nothing measured yet
      for (const [quality, icon, label] of [['excellent', 'signal-4', 'Connection excellent'], ['poor', 'signal-1', 'Connection poor'],
        ['lost', 'signal-0', 'Connection lost'], ['good', 'signal-3', 'Connection good']]) {
        await lk(f, 'quality', 'guest-a', quality);
        await expect(q).toBeVisible();
        await expect(q).toHaveAttribute('aria-label', label);
        await expect(q).toHaveAttribute('data-icon', icon);
        await expect(q.locator('use')).toHaveAttribute('href', new RegExp(`#mu-${icon}$`));
      }
      // Your own tile too.
      const me = await f.evaluate(() => (window as any).__lk.room().localParticipant.identity);
      await lk(f, 'quality', me, 'poor');
      await expect(app.locator('.tile.local[data-source="camera"] [data-ind="quality"]')).toHaveAttribute('aria-label', 'Connection poor');
    });

    test('a shared screen switches to the presentation view by itself, and back to the grid when it stops; the toggle overrides', async ({ page }) => {
      const { app, f } = await setup(page);
      const view = app.locator('#call-view');
      await lk(f, 'addRemote', 'guest-a', 'Grace (guest)');
      await expect(view).toHaveAttribute('data-layout', 'grid');
      await lk(f, 'share', 'guest-a', true);
      await expect(view).toHaveAttribute('data-layout', 'presentation');
      // The screen is the main stage; the people are in the filmstrip.
      await expect(app.locator('#stage > .tile[data-source="screen_share"]')).toHaveCount(1);
      await expect(app.locator('#strip > .tile[data-source="camera"]')).toHaveCount(2);
      await expect(app.locator('#stage > .tile[data-source="screen_share"] .mu-tile__label')).toHaveText('Grace (guest) · screen');
      await expect(app.locator('#btn-layout')).toHaveAttribute('aria-pressed', 'true');
      // The manual toggle: back to the grid while the share goes on, and again.
      await app.locator('#btn-layout').click();
      await expect(view).toHaveAttribute('data-layout', 'grid');
      await expect(app.locator('#btn-layout')).toHaveAttribute('aria-pressed', 'false');
      await app.locator('#btn-layout').click();
      await expect(view).toHaveAttribute('data-layout', 'presentation');
      await lk(f, 'share', 'guest-a', false);
      await expect(view).toHaveAttribute('data-layout', 'grid');
      await expect(app.locator('.tile[data-source="screen_share"]:not(.leaving)')).toHaveCount(0);
    });

    test('your own share: Share starts it, you are presenting, and the browser’s own Stop ends it cleanly', async ({ page }) => {
      const { app, f } = await setup(page);
      const share = app.locator('#btn-share');
      await expect(share).toBeVisible();
      await share.click();
      await expect(share).toHaveAttribute('aria-pressed', 'true');
      await expect(app.locator('#presenting')).toBeVisible();
      await expect(app.locator('#call-view')).toHaveAttribute('data-layout', 'presentation');
      const mine = app.locator('.tile.local[data-source="screen_share"]:not(.leaving)');
      await expect(mine.locator('.presenting')).toContainText('You are presenting');
      // The browser's "Stop sharing" bar: the track ends, LiveKit unpublishes.
      await lk(f, 'endLocalShare');
      await expect(share).toHaveAttribute('aria-pressed', 'false');
      await expect(app.locator('#presenting')).toBeHidden();
      await expect(app.locator('.tile[data-source="screen_share"]:not(.leaving)'), "the browser's Stop removed the share's tile").toHaveCount(0);
      await expect(app.locator('#call-view')).toHaveAttribute('data-layout', 'grid');
      // Stop from the tile's own button too.
      await share.click();
      await mine.locator('.stop-share').click();
      await expect(share).toHaveAttribute('aria-pressed', 'false');
      // Closing the browser's picker is not an error.
      await f.evaluate(() => { (window as any).__lk.refuse.screen_share = 'NotAllowedError'; });
      await share.click();
      await expect(share).toHaveAttribute('aria-pressed', 'false');
      await expect(app.locator('#call-status')).toHaveText('');
    });

    test('the keyboard shortcuts: M mutes, V turns the camera off, S shares, ? opens the sheet; never while typing', async ({ page }) => {
      const { app, f } = await setup(page);
      await app.locator('#stage').click({ position: { x: 5, y: 5 } });
      const press = (k: string) => page.keyboard.press(k);
      await press('m');
      await expect(app.locator('#btn-mic')).toHaveAttribute('aria-pressed', 'true');
      await expect(app.locator('.tile.local[data-source="camera"]')).toHaveAttribute('data-mic', 'off');
      await press('m');
      await expect(app.locator('#btn-mic')).toHaveAttribute('aria-pressed', 'false');
      await press('v');
      await expect(app.locator('#btn-cam')).toHaveAttribute('aria-pressed', 'true');
      await press('s');
      await expect(app.locator('#btn-share')).toHaveAttribute('aria-pressed', 'true');
      await press('s');
      await expect(app.locator('#btn-share')).toHaveAttribute('aria-pressed', 'false');
      await press('Shift+?');
      await expect(app.locator('#shortcuts')).toBeVisible();
      await expect(app.locator('#shortcuts')).toContainText('Mute or unmute');
      await press('Escape');
      await expect(app.locator('#shortcuts')).toBeHidden();
      // Typing in a field is typing.
      const calls = await f.evaluate(() => (window as any).__lk.calls.length);
      await app.locator('#consent-btn').click();
      await app.locator('#sw-record').focus();
      await press('m');
      expect(await f.evaluate(() => (window as any).__lk.calls.length)).toBe(calls);
      // Every shortcut is named on its button.
      await expect(app.locator('#btn-mic')).toHaveAttribute('aria-keyshortcuts', 'M');
      await expect(app.locator('#btn-cam')).toHaveAttribute('aria-keyshortcuts', 'V');
      await expect(app.locator('#btn-share')).toHaveAttribute('aria-keyshortcuts', 'S');
    });

    test('every button has an accessible name, and every icon button a tooltip', async ({ page }) => {
      const { app, f } = await setup(page);
      await lk(f, 'addRemote', 'guest-a', 'Grace (guest)');
      await app.locator('#btn-people').click();
      await app.locator('#btn-transcript').click();
      // Enumerate every button in the call, pre-join and list views, hidden or
      // not: each needs a name from its label or its text.
      const unnamed = await f.evaluate(() => {
        const out: string[] = [];
        let n = 0;
        document.querySelectorAll('button, [role="button"], select, input:not([type="hidden"])').forEach((el) => {
          n++;
          const e = el as HTMLElement;
          const name = (e.getAttribute('aria-label') || e.getAttribute('aria-labelledby') || (e.closest('label')?.textContent ?? '') || e.textContent || '').trim();
          if (!name) out.push(e.outerHTML.slice(0, 120));
        });
        return { n, out };
      });
      expect(unnamed.n).toBeGreaterThan(20); // the scan saw the page's controls
      expect(unnamed.out).toEqual([]);
      // Every icon button carries an aria-label (the kit's tooltip text).
      const iconBtns = app.locator('.mu-icon-btn');
      expect(await iconBtns.count()).toBeGreaterThan(10);
      for (const b of await iconBtns.all()) {
        expect(await b.getAttribute('aria-label'), await b.evaluate((e) => e.outerHTML.slice(0, 120))).toBeTruthy();
      }
      // The kit draws the label as a tooltip on hover.
      await app.locator('#btn-cam').hover();
      await expect(app.locator('.mu-tooltip')).toHaveText('Turn camera off (V)');
      await expect(app.locator('#btn-cam')).toHaveAccessibleName('Turn camera off');
    });

    test('axe finds no serious or critical violation on the pre-join, the call, the lobby and the past meetings', async ({ page, browser }) => {
      await useFakeLiveKit(page.context());
      const app = await ownerPrejoin(page, h);
      const f = appFrame(page);
      const scan = async (what: string) => {
        // Colour contrast is measured on what is settled, not mid-fade.
        await expect.poll(() => f.evaluate(() => document.getAnimations().filter((x) => x.playState === 'running' && x.effect?.getComputedTiming().iterations !== Infinity).length)).toBe(0);
        // The plugin's document, inside muster's frame (the frame element
        // itself is muster's page, not this plugin's).
        const r = await new AxeBuilder({ page }).include(['#app', 'html']).analyze();
        const bad = r.violations.filter((v) => v.impact === 'serious' || v.impact === 'critical');
        expect(bad.map((v) => `${v.id}: ${v.nodes.map((n) => n.target.join(' ') + ' ' + n.failureSummary).join(', ')}`), what).toEqual([]);
        expect(r.passes.length, `${what}: axe checked nothing`).toBeGreaterThan(5);
      };
      await scan('pre-join');
      await app.locator('#btn-join').click();
      await expect(app.locator('#call-view')).toHaveAttribute('data-conn', 'connected');
      await lk(f, 'addRemote', 'guest-a', 'Grace (guest)', { mic: false, quality: 'good' });
      await scan('call');
      // The lobby: a guest knocks; the People panel lists them.
      const link = await app.locator('#join-url').inputValue();
      const gctx = await browser.newContext();
      const guest = await gctx.newPage();
      await guest.goto(link);
      await guest.locator('#name').fill('Grace');
      await expect(guest.locator('#ask')).toBeEnabled({ timeout: 15_000 });
      await guest.locator('#ask').click();
      await app.locator('#btn-people').click();
      await expect(app.locator('.lobby-row.waiting')).toBeVisible();
      await scan('lobby');
      // The guest's own pages: the pre-join (waiting) as well.
      const gr = await new AxeBuilder({ page: guest }).analyze();
      expect(gr.violations.filter((v) => v.impact === 'serious' || v.impact === 'critical').map((v) => v.id), 'guest pre-join').toEqual([]);
      await gctx.close();
      await app.locator('#btn-leave').click();
      const listed = page.waitForResponse((r) => /\/api\/meetings$/.test(r.url()));
      await app.locator('#btn-back').click();
      await listed;
      await expect(app.locator('#list-view')).toBeVisible();
      await expect(app.locator('.meeting').first()).toBeVisible();
      await scan('past meetings');
    });
  });
}

test.describe('reduced motion', () => {
  test.use({ viewport: { width: 1280, height: 800 }, reducedMotion: 'reduce' });
  test('nothing animates: every transition and animation in the call is 0 s, and a layout change runs no animation', async ({ page }) => {
    const { app, f } = await setup(page);
    await lk(f, 'addRemote', 'guest-a', 'Grace (guest)');
    await lk(f, 'speakers', ['guest-a']);
    await app.locator('#btn-transcript').click();
    // A layout change (FLIP) and a share: no Web Animation is started.
    await lk(f, 'share', 'guest-a', true);
    await expect(app.locator('#call-view')).toHaveAttribute('data-layout', 'presentation');
    const moving = await f.evaluate(() => document.getAnimations().filter((a) => {
      const t = a.effect?.getComputedTiming();
      return t && Number(t.duration) > 0 && a.playState === 'running';
    }).length);
    expect(moving, 'Web Animations running under reduced motion').toBe(0);
    const offenders = await f.evaluate(() => {
      const out: string[] = [];
      let n = 0;
      document.querySelectorAll('#call-view, #call-view *').forEach((el) => {
        const cs = getComputedStyle(el);
        n++;
        const longest = (v: string) => Math.max(...v.split(',').map((s) => parseFloat(s) || 0));
        if (longest(cs.transitionDuration) > 0 || longest(cs.animationDuration) > 0) {
          out.push(`${(el as HTMLElement).id || el.className}: transition ${cs.transitionDuration}, animation ${cs.animationDuration}`);
        }
      });
      return { n, out };
    });
    expect(offenders.n).toBeGreaterThan(30);
    expect(offenders.out).toEqual([]);
    // The control: without the preference, the same page does animate.
    await page.emulateMedia({ reducedMotion: 'no-preference' });
    const animated = await f.evaluate(() => getComputedStyle(document.querySelector('.tile')!).transitionDuration);
    expect(parseFloat(animated)).toBeGreaterThan(0);
  });
});

test.describe('a phone with no screen sharing', () => {
  test.use({ viewport: { width: 390, height: 844 } });
  test('the Share button is not drawn, and the controls sit at the bottom, all on screen', async ({ page }) => {
    await page.context().addInitScript(() => {
      if (navigator.mediaDevices) (navigator.mediaDevices as any).getDisplayMedia = undefined;
    });
    const { app } = await setup(page);
    await expect(app.locator('#btn-share')).toBeHidden();
    await page.keyboard.press('s'); // no share, no error
    await expect(app.locator('#call-status')).toHaveText('');
    for (const id of ['#btn-mic', '#btn-cam', '#btn-layout', '#btn-transcript', '#btn-people', '#btn-leave']) {
      const b = app.locator(id);
      await expect(b).toBeInViewport({ ratio: 1 });
      const box = (await b.boundingBox())!;
      // Thumb reach: the lowest fifth of the screen, and a 44 px target.
      expect(box.y, id).toBeGreaterThan(844 * 0.8);
      expect(box.width, id).toBeGreaterThanOrEqual(44);
      expect(box.height, id).toBeGreaterThanOrEqual(44);
    }
  });
});
