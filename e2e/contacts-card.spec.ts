// The contacts card (muster's docs/design/contacts.md, "The guest card"):
// after the owner ends the meeting, or the guest leaves, an admitted guest
// (never the owner) sees an optional card: their name prefilled from the
// knock, an email, the consent sentence, Send and Skip; once per guest per
// meeting; one success message whatever contacts does with it. With no
// contacts URL there is no card at all.
//
// The harness runs the plugin with -contacts=fake (the fake contacts API,
// held to the contract by internal/contacts's contract test) for the first
// group, and with no contacts for the last. Every card test runs at a
// phone's 390x844 and a desktop's 1280x800.
import { test, expect, type Browser, type Page, type FrameLocator } from '@playwright/test';
import { startHarness, useFakeLiveKit, ownerPrejoin, sizes, type Harness } from './helpers';

// The consent, as the design words it, whole (a reword must fail here).
const consent = 'Your name and email go to Ada L., who will see that you were in this meeting. Nothing else is shared.';
const title = 'Stay in touch with Ada L.?';
const sent = 'Sent to Ada L.';

type Received = { Key: string; Name: string; Email: string; Owner: string; Meeting: string; Title: string; TypedName: string };

async function received(h: Harness): Promise<{ received: Received[]; submission_posts: number }> {
  const r = await fetch(`${h.parent}/contacts-received`);
  expect(r.status).toBe(200);
  return r.json();
}

async function contactsMode(h: Harness, q: string) {
  const r = await fetch(`${h.parent}/contacts-mode?${q}`);
  expect(r.status, await r.text()).toBe(200);
}

// meeting opens the owner's call and returns its link.
async function meeting(page: Page, h: Harness): Promise<{ app: FrameLocator; link: string }> {
  await useFakeLiveKit(page.context());
  const app = await ownerPrejoin(page, h);
  const link = await app.locator('#join-url').inputValue();
  await app.locator('#btn-join').click();
  await expect(app.locator('#call-view')).toHaveAttribute('data-conn', 'connected', { timeout: 15_000 });
  return { app, link };
}

// guestIn knocks as name from a fresh context of the given size and is
// admitted by the owner.
async function guestIn(browser: Browser, app: FrameLocator, link: string, name: string, viewport: { width: number; height: number }, reducedMotion: 'reduce' | 'no-preference' = 'no-preference') {
  const ctx = await browser.newContext({ viewport, reducedMotion });
  await useFakeLiveKit(ctx);
  const guest = await ctx.newPage();
  await guest.goto(link);
  await guest.locator('#name').fill(name);
  await expect(guest.locator('#ask')).toBeEnabled({ timeout: 15_000 });
  await guest.locator('#ask').click();
  await expect(guest.locator('#guest')).toHaveAttribute('data-state', 'waiting');
  if (!(await app.locator('.lobby-row').first().isVisible())) await app.locator('#btn-people').click();
  const row = app.locator('.lobby-row.waiting').filter({ hasText: name });
  await row.locator('.admit').click();
  await expect(guest.locator('#guest')).toHaveAttribute('data-state', 'admitted');
  // The stream is open (its badge drawn), so the plugin has asked contacts.
  await expect(guest.locator('#badge')).not.toHaveText('', { timeout: 15_000 });
  return { ctx, guest };
}

async function endMeeting(app: FrameLocator) {
  await app.locator('#btn-leave').click();
  await app.locator('#btn-end').click();
  await expect(app.locator('#list-view')).toBeVisible();
}

const norm = (s: string | null) => (s || '').replace(/\s+/g, ' ').trim();

test.describe('with contacts configured', () => {
  let h: Harness;
  test.beforeAll(async () => { h = await startHarness(['-contacts=fake']); });
  test.afterAll(() => { h?.proc.stdin?.end(); });
  test.beforeEach(async () => { await contactsMode(h, 'mode=up'); });

  for (const size of sizes) {
    test.describe(`at ${size.width}x${size.height}`, () => {
      test.use({ viewport: { width: size.width, height: size.height } });

      test(`End: the guest's card is prefilled and Sent reaches contacts once; the owner never sees one (${size.name})`, async ({ page, browser }) => {
        const { app, link } = await meeting(page, h);
        const name = `Wren ${size.name}`;
        const { ctx, guest } = await guestIn(browser, app, link, name, size);
        const before = (await received(h)).received.length;
        await endMeeting(app);
        await expect(guest.locator('#message')).toHaveText('This meeting has ended.');
        const card = guest.locator('#card');
        await expect(card).toBeVisible();
        await expect(card.locator('#card-title')).toHaveText(title);
        expect(norm(await card.locator('#card-consent').textContent())).toBe(consent);
        await expect(card.locator('#card-name')).toHaveValue(name);
        await expect(card.locator('#card-email')).toHaveValue('');
        await expect(card.locator('#card-email')).toHaveAttribute('type', 'email');
        await expect(card.locator('#card-send')).toHaveAccessibleName('Send');
        await expect(card.locator('#card-skip')).toHaveAccessibleName('Skip');
        // Icon-led: the kit's icons are drawn in the title and on Send.
        await expect(card.locator('.card-ask__head svg.mu-icon')).toHaveCount(1);
        await expect(card.locator('#card-send svg.mu-icon')).toHaveCount(1);
        // The whole card is on screen, Send and Skip included, with no scroll.
        await expect(card.locator('#card-send')).toBeInViewport({ ratio: 1 });
        await expect(card.locator('#card-skip')).toBeInViewport({ ratio: 1 });
        // 🔴 Never the owner: their frame has no card at all.
        await expect(app.locator('#card')).toHaveCount(0);
        // An invalid email is refused in place, keeping what was typed.
        await card.locator('#card-email').fill('wren at example');
        await card.locator('#card-send').click();
        await expect(card.locator('#card-email')).toHaveAttribute('aria-invalid', 'true');
        await expect(card.locator('#card-email-error')).toBeVisible();
        await expect(card.locator('#card-name')).toHaveValue(name);
        const email = `wren.${size.name}@example.test`;
        await card.locator('#card-email').fill(email);
        await card.locator('#card-send').click();
        await expect(guest.locator('#card-done')).toHaveText(sent);
        await expect.poll(async () => (await received(h)).received.length, { timeout: 15_000 }).toBe(before + 1);
        const got = (await received(h)).received.at(-1)!;
        expect(got).toMatchObject({ Name: name, Email: email, TypedName: name });
        expect(got.Key).toMatch(/^card:[A-Za-z0-9_-]{43}$/);
        // 🔴 Once only: the ended link shows no card again.
        await guest.reload();
        await expect(guest.locator('#message')).toHaveText('This meeting has ended.');
        await expect(guest.locator('#card')).toHaveCount(0);
        await ctx.close();
      });

      test(`Leave: the card follows the guest's own Leave; Skip sends nothing and it never comes back (${size.name})`, async ({ page, browser }) => {
        const { app, link } = await meeting(page, h);
        const { ctx, guest } = await guestIn(browser, app, link, `Ash ${size.name}`, size);
        const posts = (await received(h)).submission_posts;
        await guest.locator('#btn-leave').click();
        await expect(guest.locator('#message')).toHaveText('You left the meeting.');
        const card = guest.locator('#card');
        await expect(card).toBeVisible();
        await expect(card.locator('#card-name')).toHaveValue(`Ash ${size.name}`);
        await card.locator('#card-skip').click();
        await expect(card).toHaveCount(0);
        // The owner ends the meeting afterwards: no second card.
        await endMeeting(app);
        await guest.waitForTimeout(1500);
        await expect(guest.locator('#card')).toHaveCount(0);
        // 🔴 Skip sent nothing: contacts was never posted to.
        expect((await received(h)).submission_posts).toBe(posts);
        await ctx.close();
      });
    });
  }

  test('the answer is the same when contacts is down, and the card arrives once it is back (390x844)', async ({ page, browser }) => {
    const size = sizes[0];
    await page.setViewportSize(size);
    const { app, link } = await meeting(page, h);
    const { ctx, guest } = await guestIn(browser, app, link, 'Rowan D.', size);
    const before = (await received(h)).received.length;
    await endMeeting(app);
    await expect(guest.locator('#card')).toBeVisible();
    await contactsMode(h, 'mode=down');
    await guest.locator('#card-email').fill('rowan.d@example.test');
    const t0 = Date.now();
    await guest.locator('#card-send').click();
    await expect(guest.locator('#card-done')).toHaveText(sent);
    expect(Date.now() - t0, 'Send waited on contacts').toBeLessThan(2500);
    expect((await received(h)).received.length).toBe(before);
    await contactsMode(h, 'mode=up');
    // The outbox retries on its backoff (5 s first).
    await expect.poll(async () => (await received(h)).received.length, { timeout: 30_000 }).toBe(before + 1);
    expect((await received(h)).received.at(-1)!.Email).toBe('rowan.d@example.test');
    await ctx.close();
  });

  test.describe('reduced motion', () => {
    test.use({ viewport: { width: 390, height: 844 }, reducedMotion: 'reduce' });
    test('the card neither rises nor fades under reduced motion, and does without it', async ({ page, browser }) => {
      const { app, link } = await meeting(page, h);
      const { ctx, guest } = await guestIn(browser, app, link, 'Quill R.', { width: 390, height: 844 }, 'reduce');
      await endMeeting(app);
      const card = guest.locator('#card');
      await expect(card).toBeVisible();
      const timing = () => card.evaluate((el) => { const cs = getComputedStyle(el); return `${cs.animationDuration} ${cs.transitionDuration}`; });
      expect(await timing()).toBe('0s 0s, 0s');
      await guest.emulateMedia({ reducedMotion: 'no-preference' });
      expect(await timing()).not.toBe('0s 0s, 0s');
      await ctx.close();
    });
  });
});

test.describe('with contacts configured, not accepting this owner', () => {
  // Its own harness: the plugin caches an owner's answer for a minute, and
  // the group above warmed it for the same owner.
  let h: Harness;
  test.beforeAll(async () => { h = await startHarness(['-contacts=fake']); });
  test.afterAll(() => { h?.proc.stdin?.end(); });

  test('no card when contacts answers the owner not accepting (390x844)', async ({ page, browser }) => {
    const size = sizes[0];
    await page.setViewportSize(size);
    const { app, link } = await meeting(page, h);
    await contactsMode(h, 'accepting=0');
    const { ctx, guest } = await guestIn(browser, app, link, 'Sage N.', size);
    await endMeeting(app);
    await expect(guest.locator('#message')).toHaveText('This meeting has ended.');
    await guest.waitForTimeout(1000);
    await expect(guest.locator('#card')).toHaveCount(0);
    await ctx.close();
  });
});

test.describe('with no contacts URL', () => {
  let h: Harness;
  test.beforeAll(async () => { h = await startHarness(); });
  test.afterAll(() => { h?.proc.stdin?.end(); });
  test.use({ viewport: { width: 390, height: 844 } });

  test('no card after End or after Leave, and the card routes do not exist', async ({ page, browser }) => {
    const { app, link } = await meeting(page, h);
    const a = await guestIn(browser, app, link, 'Ivo L.', { width: 390, height: 844 });
    const b = await guestIn(browser, app, link, 'Uma K.', { width: 1280, height: 800 });
    await b.guest.locator('#btn-leave').click();
    await expect(b.guest.locator('#message')).toHaveText('You left the meeting.');
    await endMeeting(app);
    await expect(a.guest.locator('#message')).toHaveText('This meeting has ended.');
    await a.guest.waitForTimeout(1000);
    await expect(a.guest.locator('#card')).toHaveCount(0);
    await expect(b.guest.locator('#card')).toHaveCount(0);
    const r = await fetch(`${h.plugin}/card`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{"id":"x"}' });
    expect(r.status).toBe(404);
    expect(await r.text()).toBe('{"error":"not found"}\n');
    await a.ctx.close();
    await b.ctx.close();
  });
});
