// Use Meetings from Claude Code: in the owner's settings, Create token shows
// the ready `claude mcp add …` command ONCE, the list shows the token by its
// dates only, and Revoke turns the token's /mcp door off at once. The token
// is used the way Claude Code uses it (a JSON-RPC POST to the plugin's /mcp
// with the bearer), from the test, not the page.
import { test, expect } from '@playwright/test';
import { startHarness, type Harness } from './helpers';

let h: Harness;
test.beforeAll(async () => { h = await startHarness(); });
test.afterAll(() => { h?.proc.stdin?.end(); });
test.use({ viewport: { width: 1280, height: 800 } });

async function toolsList(url: string, token: string): Promise<number> {
  const r = await fetch(url, {
    method: 'POST',
    headers: { 'Authorization': `Bearer ${token}`, 'Content-Type': 'application/json', 'Accept': 'application/json, text/event-stream' },
    body: JSON.stringify({ jsonrpc: '2.0', id: 1, method: 'tools/list' }),
  });
  return r.status;
}

test('Create token shows the claude mcp add command once; Revoke makes /mcp refuse it', async ({ page }) => {
  await page.goto(`${h.parent}/apps/${h.pluginID}`);
  const app = page.frameLocator('#app');
  await expect(app.locator('#mcp-settings')).toBeVisible();
  await expect(app.locator('#mcp-tokens .mcp-token')).toHaveCount(0);
  await expect(app.locator('#mcp-out')).toBeHidden();

  await app.locator('#mcp-create').click();
  await expect(app.locator('#mcp-out')).toBeVisible();
  const cmd = await app.locator('#mcp-command').inputValue();
  const m = cmd.match(/^claude mcp add --transport http meetings (\S+\/mcp) --header "Authorization: Bearer (mmc_[A-Za-z0-9_-]{43})"$/);
  expect(m, cmd).not.toBeNull();
  const [, url, token] = m!;
  expect(url).toBe(`${h.plugin}/mcp`);
  expect(await toolsList(url, token)).toBe(200);

  // The list names the token by its dates, never the token itself.
  const row = app.locator('#mcp-tokens .mcp-token');
  await expect(row).toHaveCount(1);
  await expect(row).toContainText('Created');
  expect(await app.locator('#mcp-tokens').innerText()).not.toContain(token.slice(4));

  await row.locator('.mcp-revoke').click();
  await expect.poll(() => toolsList(url, token), { message: 'the revoked token still opens /mcp' }).toBe(401);
  await expect(row).toHaveCount(0);
});
