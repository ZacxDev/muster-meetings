// The meeting's agent (an experiment): a Claude Code session of the owner's
// joins a live meeting through Meetings' MCP (the test plays it: JSON-RPC
// POSTs to the plugin's /mcp with the owner's token, as Claude Code sends
// them). The owner presses Ask agent with a question; the agent's wait
// answers the transcript so far and the question; its answer shows, live,
// in the owner's Agent panel and in an admitted guest's, under its name,
// and only the owner's copy holds the question.
import { test, expect } from '@playwright/test';
import { startHarness, ownerInCall, type Harness } from './helpers';

let h: Harness;
test.beforeAll(async () => { h = await startHarness(); });
test.afterAll(() => { h?.proc.stdin?.end(); });
test.use({ viewport: { width: 1280, height: 800 } });

// The harness's fake engine script (scripts/e2e-harness: script).
const first = 'Ada opens the meeting.';

async function tool(url: string, token: string, name: string, args: Record<string, unknown>): Promise<any> {
  const r = await fetch(url, {
    method: 'POST',
    headers: { 'Authorization': `Bearer ${token}`, 'Content-Type': 'application/json', 'Accept': 'application/json, text/event-stream' },
    body: JSON.stringify({ jsonrpc: '2.0', id: 1, method: 'tools/call', params: { name, arguments: args } }),
  });
  expect(r.status).toBe(200);
  const b = await r.json();
  const text = (b.result?.content || []).map((c: any) => c.text).join('\n');
  if (b.result?.isError) throw new Error(`${name}: ${text}`);
  return JSON.parse(text);
}

test('Ask agent: the agent gets the transcript and the question, and the owner and a guest see its answer', async ({ page, browser }) => {
  // The owner's MCP token, from settings (shown once, in the command).
  await page.goto(`${h.parent}/apps/${h.pluginID}`);
  const list = page.frameLocator('#app');
  await list.locator('#mcp-create').click();
  const cmd = await list.locator('#mcp-command').inputValue();
  const m = cmd.match(/meetings (\S+\/mcp) --header "Authorization: Bearer (mmc_[A-Za-z0-9_-]{43})"$/);
  expect(m, cmd).not.toBeNull();
  const [, url, token] = m!;

  const app = await ownerInCall(page, h);
  await expect(app.locator('#call-url')).toHaveValue(/\/j\/[A-Za-z0-9_-]{22}$/, { timeout: 15_000 });
  const link = await app.locator('#call-url').inputValue();
  const { meetings } = await tool(url, token, 'meetings_list', {});
  const meeting = meetings.find((x: any) => x.state === 'open');
  expect(meeting, JSON.stringify(meetings)).toBeTruthy();

  // A guest is admitted.
  const guestCtx = await browser.newContext();
  const guest = await guestCtx.newPage();
  await guest.goto(link);
  await guest.locator('#name').fill('Grace');
  await expect(guest.locator('#ask')).toBeEnabled({ timeout: 15_000 });
  await guest.locator('#ask').click();
  await app.locator('#btn-people').click();
  await app.locator('.lobby-row.waiting .admit').click();
  await expect(guest.locator('#guest')).toHaveAttribute('data-state', 'admitted');

  // The owner speaks (the fake Egress and engine): two lines.
  const pub = await guestCtx.request.get(`${h.parent}/publish?room=${encodeURIComponent('meeting-' + meeting.id)}&identity=owner`);
  expect(pub.status(), await pub.text()).toBe(200);
  await expect.poll(() => app.locator('#transcript li.line:not(.partial)').count(), { timeout: 30_000 }).toBe(2);

  // No agent yet: no Agent button for either.
  await expect(app.locator('#btn-agent')).toBeHidden();
  await expect(guest.locator('#btn-agent')).toBeHidden();

  // The agent joins: both see it by name.
  const { agent_key: key } = await tool(url, token, 'meeting_agent_join', { id: meeting.id, name: 'Widget builder' });
  await expect(app.locator('#btn-agent')).toBeVisible({ timeout: 15_000 });
  await expect(guest.locator('#btn-agent')).toBeVisible({ timeout: 15_000 });
  await app.locator('#btn-agent').click();
  await expect(app.locator('#agent-who')).toContainText('Widget builder (AI agent) is in this meeting');
  await guest.locator('#btn-agent').click();
  await expect(guest.locator('#agent-who')).toContainText('Widget builder (AI agent) is in this meeting');
  // A guest's panel has no Ask agent.
  await expect(guest.locator('#agent-ask')).toHaveCount(0);

  // Ask agent, with a question: the button waits for the answer.
  const question = 'What did Ada open?';
  await app.locator('#agent-question').fill(question);
  await app.locator('#agent-ask').click();
  await expect(app.locator('#agent-ask')).toBeDisabled();
  const ask = await tool(url, token, 'meeting_agent_wait', { id: meeting.id, agent_key: key });
  expect(ask.status).toBe('ask');
  expect(ask.question, 'the question typed in Ask agent').toBe(question);
  expect(ask.first).toBe(true);
  expect(ask.lines.map((l: any) => l.text)).toContain(first);
  expect(ask.lines).toHaveLength(2);

  // An answer that looks like markup is shown as text.
  const answer = 'Ada opened the meeting. <b>Not bold</b> & done.';
  await tool(url, token, 'meeting_agent_say', { id: meeting.id, agent_key: key, ask_id: ask.ask_id, text: answer });

  // The owner's panel: the answer, its name, and the question.
  const ownerReply = app.locator('#agent-replies .agent-reply');
  await expect(ownerReply).toHaveCount(1, { timeout: 15_000 });
  await expect(ownerReply.locator('.who')).toHaveText('Widget builder');
  await expect(ownerReply.locator('.agent-reply__text')).toHaveText(answer);
  await expect(ownerReply.locator('.agent-reply__question')).toHaveText('You asked: ' + question);
  await expect(app.locator('#agent-ask')).toBeEnabled();

  // The guest's: the same answer, without the question.
  const guestReply = guest.locator('#agent-replies .agent-reply');
  await expect(guestReply).toHaveCount(1, { timeout: 15_000 });
  await expect(guestReply.locator('.agent-reply__text')).toHaveText(answer);
  await expect(guestReply.locator('b'), 'the answer was parsed as markup').toHaveCount(0);
  await expect(ownerReply.locator('b'), 'the answer was parsed as markup').toHaveCount(0);
  await expect(guestReply.locator('.who')).toHaveText('Widget builder');
  await expect(guestReply.locator('.agent-reply__question')).toHaveCount(0);
  expect(await guest.locator('#agent-panel').innerText()).not.toContain(question);

  // Remove agent: the agent's next wait says it is over.
  await app.locator('#agent-remove').click();
  await expect(app.locator('#agent-who')).toHaveText('No agent is in this meeting.');
  const after = await tool(url, token, 'meeting_agent_wait', { id: meeting.id, agent_key: key });
  expect(after.status).toBe('ended');
  await guestCtx.close();
});
