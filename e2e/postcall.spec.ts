// The post-call notes, in a browser at a phone's 390x844 and a desktop's
// 1280x800: the live action-items panel (owner and an admitted guest alike),
// the owner's address confirmed by its mailed link, and the drafts reviewed,
// edited, previewed and sent. The harness runs the plugin with -notes=fake
// (the fake OpenRouter and the fake SMTP server) and -contacts=fake (the
// guest card, whose address the notes go to). No model and no mail server
// outside this machine is reached.
import { test, expect, type Browser, type FrameLocator, type Page } from '@playwright/test';
import { startHarness, sizes, type Harness } from './helpers';

type RTC = { Claims: { sub: string; video?: { room?: string } } };
type Mail = { To: string; Subject: string; Text: string };

let h: Harness;
test.beforeAll(async () => { h = await startHarness(['-contacts=fake', '-notes=fake']); });
test.afterAll(() => { h?.proc.stdin?.end(); });

async function joins(): Promise<RTC[]> { return (await fetch(`${h.parent}/livekit-joins`)).json(); }
async function mails(): Promise<Mail[]> { return (await fetch(`${h.parent}/mail-received`)).json(); }

// call opens a meeting as the owner and admits name from a context of the
// page's size; it answers the owner's frame and the guest's page.
async function call(page: Page, browser: Browser, name: string, size: { width: number; height: number }): Promise<{ app: FrameLocator; guest: Page; room: string }> {
  await page.goto(`${h.parent}/apps/${h.pluginID}`);
  const app = page.frameLocator('#app');
  await app.locator('#new-meeting').click();
  await expect(app.locator('#prejoin, #call-view').filter({ visible: true })).toHaveCount(1, { timeout: 15_000 });
  if (await app.locator('#prejoin').isVisible()) {
    await expect(app.locator('#btn-join')).toBeEnabled({ timeout: 15_000 });
    await app.locator('#btn-join').click();
  }
  await expect(app.locator('#call-view')).toBeVisible();
  await app.locator('#call-copy').waitFor();
  const link = await app.locator('#call-url').inputValue();
  const ctx = await browser.newContext({ viewport: size });
  const guest = await ctx.newPage();
  await guest.goto(link);
  await guest.locator('#name').fill(name);
  await expect(guest.locator('#ask')).toBeEnabled({ timeout: 15_000 });
  await guest.locator('#ask').click();
  await expect(guest.locator('#guest')).toHaveAttribute('data-state', 'waiting');
  if (!(await app.locator('.lobby-row').first().isVisible())) await app.locator('#btn-people').click();
  await app.locator('.lobby-row.waiting').filter({ hasText: name }).locator('.admit').click();
  await expect(guest.locator('#guest')).toHaveAttribute('data-state', 'admitted');
  await expect(guest.locator('#badge')).not.toHaveText('', { timeout: 15_000 });
  await expect.poll(async () => (await joins()).filter((j) => j.Claims.sub === 'owner').length, { timeout: 15_000 }).toBeGreaterThan(0);
  const js = (await joins()).filter((j) => j.Claims.sub === 'owner');
  return { app, guest, room: js[js.length - 1].Claims.video!.room! };
}

async function speak(room: string) {
  const r = await fetch(`${h.parent}/publish?room=${encodeURIComponent(room)}&identity=owner`);
  expect(r.status, await r.text()).toBe(200);
}

async function endMeeting(app: FrameLocator) {
  await app.locator('#btn-leave').click();
  await app.locator('#btn-end').click();
  await expect(app.locator('#list-view')).toBeVisible();
}

// confirm sets and confirms the owner's address from the meetings list.
async function confirm(page: Page, app: FrameLocator, browser: Browser, addr: string) {
  await expect(app.locator('#notes-settings')).toBeVisible();
  await app.locator('#notes-addr').fill(addr);
  await app.locator('#notes-addr-send').click();
  await expect(app.locator('#notes-addr-state')).toHaveText(`We sent a link to ${addr}. Open it to confirm.`);
  const link = await (await fetch(`${h.parent}/confirm-link`)).text();
  expect(link).toMatch(/^\/e\/[A-Za-z0-9_-]{43}$/);
  const tab = await page.context().newPage();
  await tab.goto(h.plugin + link);
  await expect(tab.locator('#message')).toContainText(`Send your meeting notes from and to ${addr}?`);
  await tab.locator('#confirm-yes').click();
  await expect(tab.locator('#message')).toContainText(`Confirmed. Meetings will send your meeting notes from and to ${addr}.`);
  await tab.close();
}

for (const size of sizes) {
  test.describe(`at ${size.width}x${size.height}`, () => {
    test.use({ viewport: { width: size.width, height: size.height } });

    test(`the live action items reach the owner and the guest, icon-led, in the panel (${size.name})`, async ({ page, browser }) => {
      const { app, guest, room } = await call(page, browser, `Iris ${size.name}`, size);
      // The owner speaks (the fake engine's first line is "I'll send the deck
      // by Friday."); the button appears with the first items.
      await speak(room);
      for (const [who, f] of [['owner', app], ['guest', guest]] as const) {
        await expect(f.locator('#btn-items'), who).toBeVisible({ timeout: 30_000 });
        await f.locator('#btn-items').click();
        const item = f.locator('#items-list .item');
        await expect(item, who).toHaveCount(1);
        await expect(item.locator('.item__text')).toHaveText('Send the deck by Friday');
        await expect(item.locator('.item__who')).toHaveText('Ada L.');
        await expect(item.locator('.item__icon svg.mu-icon')).toHaveCount(1);
        await expect(f.locator('#items-status')).toBeHidden();
        await expect(f.locator('#items-panel')).toBeInViewport();
      }
      await guest.context().close();
    });

    test(`the owner confirms the address, reviews and edits the drafts, previews, and sends (${size.name})`, async ({ page, browser }) => {
      const name = `Juno ${size.name}`;
      const addr = `juno.${size.name}@example.test`;
      const owner = `ada.${size.name}@example.test`;
      const { app, guest, room } = await call(page, browser, name, size);
      await speak(room);
      await expect(app.locator('#btn-items')).toBeVisible({ timeout: 30_000 });
      await endMeeting(app);
      // The guest's card says the address also gets the notes.
      await expect(guest.locator('#card-consent')).toHaveText(
        'Your name and email go to Ada L., who will see that you were in this meeting, and you get the meeting notes when Ada L. sends them. Nothing else is shared.');
      await guest.locator('#card-email').fill(addr);
      await guest.locator('#card-send').click();
      await expect(guest.locator('#card-done')).toHaveText('Sent to Ada L.');
      await guest.context().close();

      await confirm(page, app, browser, owner);
      const before = (await mails()).length;
      await app.locator('.meeting').first().locator('.transcript-row').click();
      await expect(app.locator('#analysis')).toBeVisible();
      await expect(app.locator('#archive-items .item .item__text')).toHaveText(['Send the deck by Friday'], { timeout: 30_000 });
      await expect(app.locator('#summary-points li').first()).toContainText('Ada L. said:');
      const cards = app.locator('#draft-list .draft');
      await expect(cards).toHaveCount(2);
      await expect(cards.nth(0).locator('.draft__who')).toHaveText('Ada L. (you)');
      await expect(cards.nth(1).locator('.draft__who')).toHaveText(name);
      await expect(cards.nth(1).locator('.draft__addr')).toHaveText(addr);
      // Juno takes the deck: their preview shows it as theirs, the owner's
      // as everyone else's.
      const who = cards.nth(1).locator('input[data-field=assignee]');
      await who.fill(name);
      await who.blur();
      await cards.nth(1).locator('.preview-btn').click();
      await expect(cards.nth(1).locator('.preview__yours li')).toHaveText(['Send the deck by Friday']);
      await expect(cards.nth(1).locator('.preview__others')).toHaveCount(0);
      await expect(cards.nth(1).locator('.preview__subject')).toHaveText(/^Notes: the meeting on /);
      await expect(app.locator('#drafts-send')).toBeEnabled();
      await app.locator('#drafts-send').click();
      await expect.poll(async () => (await mails()).length, { timeout: 15_000 }).toBe(before + 2);
      const got = (await mails()).slice(before);
      const toGuest = got.find((m) => m.To === addr)!;
      const toOwner = got.find((m) => m.To === owner)!;
      expect(toGuest.Text).toContain('YOUR ACTION ITEMS\r\n  * Send the deck by Friday\r\n');
      expect(toOwner.Text).toContain('YOUR ACTION ITEMS\r\n  * Send the deck by Friday\r\n'); // the owner's own draft was not edited
      await expect(cards.nth(1).locator('.mu-chip')).toHaveText('Sent', { timeout: 15_000 });
      await expect(app.locator('#drafts-send')).toBeHidden();
    });
  });
}
