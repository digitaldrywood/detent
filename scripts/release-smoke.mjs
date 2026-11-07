import { randomUUID } from 'node:crypto';

const rows = [], [base, org, project, credentialEnv] = process.argv.slice(2);
const token = process.env[credentialEnv], runnerToken = process.env[process.env.SMOKE_RUNNER_CREDENTIAL_ENV];
const secrets = [token, runnerToken].filter(Boolean);
const clean = value => secrets.reduce((text, secret) => text.replaceAll(secret, '[REDACTED]'), String(value));
const fail = message => { throw new Error(message); };
const enc = encodeURIComponent, run = `release-smoke-${randomUUID()}`;
let session = '', protocol = '2025-06-18', sequence = 0;
const api = `/api/v2/organizations/${enc(org)}`, root = `${api}/projects/${enc(project)}`;
async function step(name, action) {
  const start = Date.now();
  try { return await action(); }
  catch (error) { rows.push({ name, error: clean(error.message), ms: Date.now() - start }); }
}
async function request(name, path, { method = 'GET', payload, expected = 200, board = false, credential = token, headers = {} } = {}) {
  const start = Date.now(), row = { name, expected, board };
  rows.push(row);
  try {
    const response = await fetch(new URL(path, base), { method, redirect: 'manual', signal: AbortSignal.timeout(15000),
      headers: { Authorization: `Bearer ${credential}`, 'Content-Type': 'application/json', ...headers },
      ...(payload === undefined ? {} : { body: JSON.stringify(payload) }) });
    row.status = response.status;
    const body = await response.text();
    row.body = clean(body);
    if (body.length > 8 * 1024 * 1024) fail('response exceeded 8 MiB');
    if (response.status !== expected) fail(`expected HTTP ${expected}`);
    return { value: body ? JSON.parse(body) : null, response, row };
  } catch (error) { row.error = clean(error.message); throw error; }
  finally { row.ms = Date.now() - start; }
}
async function rpc(method, params, name = method, notification = false) {
  const response = await request(name, '/mcp', { method: 'POST', expected: notification ? 202 : 200,
    headers: { Accept: 'application/json, text/event-stream', 'Mcp-Protocol-Version': protocol, ...(session ? { 'Mcp-Session-Id': session } : {}) },
    payload: { jsonrpc: '2.0', ...(notification ? {} : { id: ++sequence }), method, params } });
  if (method === 'initialize') {
    session = response.response.headers.get('mcp-session-id');
    protocol = response.value?.result?.protocolVersion;
    if (!session || !protocol) fail('initialize omitted session or protocol');
  }
  if (response.value?.error) fail('JSON-RPC error');
  return response;
}
async function tool(name, args = {}, label = name) {
  const { value, row } = await rpc('tools/call', { name, arguments: { organization_id: org, project_id: project, ...args } }, label);
  const result = value?.result;
  if (!result || result.isError) fail('MCP isError or absent result');
  const content = result.structuredContent ?? JSON.parse(result.content?.find(block => block.type === 'text')?.text ?? 'null');
  if (content == null) fail('MCP omitted application result');
  row.body = clean(JSON.stringify({ ...result, structuredContent: content }));
  return content.data ?? content;
}
async function lifecycle() {
  let issue, disposableID;
  const draft = { request_id: run, title: `Disposable release smoke ${run}`, state: 'Backlog', type: 'change_request',
    description: `## Acceptance criteria\nExercise only this disposable smoke issue (${run}).\n## Must not break\nExisting work and runner dispatch.\n## How we know it worked\nComment, edit, move and cancel this issue.` };
  try {
    try { issue = await tool('file_issue', draft); }
    catch { issue = await tool('file_issue', draft, 'reconcile file_issue with same request_id'); }
    if (issue.challenge_token) {
      const numbers = issue.possible_duplicates?.map(candidate => candidate.number);
      if (!numbers?.length) fail('duplicate challenge omitted candidates');
      for (const number of numbers) await tool('work_item', { reference: `${project}#${number}` }, `duplicate candidate ${number}`);
      issue = await tool('file_issue', { ...draft, challenge_token: issue.challenge_token, not_duplicate_of: numbers }, 'file_issue confirmed disposable run');
    }
    disposableID = issue.work_item_id;
    if (!disposableID || !issue.revision) fail('file_issue omitted identity/revision; reconcile using request_id ' + run);
    const args = { identifier: disposableID, request_id: `${run}-comment`, body: 'Disposable release smoke comment.' };
    await step('add_comment', () => tool('add_comment', args));
    await step('edit_item', async () => {
      issue = await tool('edit_item', { identifier: issue.work_item_id, request_id: `${run}-edit`, expected_revision: String(issue.revision), title: draft.title + ' edited' });
    });
    await step('move_item', async () => {
      const current = await tool('work_item', { reference: issue.work_item_id }, 'read before move');
      issue = await tool('move_item', { identifier: issue.work_item_id, request_id: `${run}-move`, expected_revision: String(current.revision), target_state: 'Human Review', reason_detail: 'Disposable smoke; never dispatch to runners.' });
      if (issue.state !== 'Human Review') fail('move_item did not confirm Human Review');
    });
  } finally {
    if (disposableID) await step('cancel disposable issue', async () => {
      const current = await tool('work_item', { reference: disposableID }, 'read before cancel');
      const cancelled = await tool('move_item', { identifier: disposableID, request_id: `${run}-cancel`, expected_revision: String(current.revision), target_state: 'Cancelled', reason_detail: 'Disposable release smoke complete.' }, 'cancel disposable issue');
      if (cancelled.state !== 'Cancelled') fail('cancellation not confirmed: ' + disposableID);
    });
  }
}
async function browserChecks(item, change) {
  const { chromium } = await import('@playwright/test');
  if (!process.env.SMOKE_BROWSER_STATE) fail('SMOKE_BROWSER_STATE must name an authenticated Playwright storage-state file');
  const browser = await chromium.launch({ headless: true });
  try {
    const context = await browser.newContext({ storageState: process.env.SMOKE_BROWSER_STATE });
    const page = await context.newPage();
    const boardURL = new URL(`/organizations/${enc(org)}/work/p/${enc(project)}`, base);
    const counts = [], failures = [], responses = [];
    for (let load = 1; load <= 3; load++) {
      const start = Date.now(), requests = [];
      const listener = request => { if (/\/(work-items|board\/counts|board\/status)(\?|$)/.test(request.url())) requests.push(request.url()); };
      page.on('request', listener);
      const responseListener = response => {
        if (response.url().includes(root) && response.status() >= 400) responses.push(response.text().catch(() => '').then(body => failures.push(`HTTP ${response.status()}: ${clean(body).slice(0, 200)}`)));
      };
      page.on('response', responseListener);
      const loaded = page.waitForResponse(response => response.url().includes(`${root}/board/counts`) && response.status() === 200, { timeout: 15000 });
      const [, document] = await Promise.all([loaded, page.goto(boardURL.href, { waitUntil: 'load', timeout: 15000 })]);
      if (document.status() !== 200) fail(`board document HTTP ${document.status()}`);
      await page.waitForFunction(() => document.querySelector('[role="main"], main')?.textContent?.trim(), null, { timeout: 15000 });
      await page.waitForFunction(() => {
        const count = performance.getEntriesByType('resource').filter(entry => /\/(work-items|board\/counts|board\/status)(\?|$)/.test(entry.name)).length;
        if (window.smokeCount !== count) { window.smokeCount = count; window.smokeQuietSince = performance.now(); }
        return performance.now() - window.smokeQuietSince >= 3000;
      }, null, { timeout: 15000 });
      page.off('request', listener); page.off('response', responseListener);
      await Promise.all(responses);
      counts.push(requests.length);
      if (/Unavailable|unexpected payload|Expected array|Access to the board|Sign in/.test(await page.locator('main, [role="main"]').first().innerText())) failures.push('board rendered an error');
      if (new Set(requests).size !== requests.length) failures.push('board endpoint repeated within one load');
      rows.push({ name: `browser board load ${load}`, status: 200, expected: 200, ms: Date.now() - start, note: `${requests.length} board requests`,
        error: !requests.length || failures.length ? clean(failures.join('; ') || 'no board requests observed') : '' });
    }
    rows.push({ name: 'board request count stable', status: 200, expected: 200, note: counts.join('/'), error: new Set(counts).size === 1 ? '' : 'request count changed between three loads' });
    for (const [name, path] of [['activity', `${api}/activity`], ['diagnostics', `${api}/diagnostics`]]) {
      const start = Date.now(), response = await context.request.get(new URL(path, base).href, { maxRedirects: 0, timeout: 15000 });
      rows.push({ name, status: response.status(), expected: 200, ms: Date.now() - start, body: clean(await response.text()) });
    }
    for (const [name, id, query] of [['issue detail page', item, ''], ['Change detail page', change?.work_item_id, `/changes/${enc(change?.change_id)}`]]) {
      await step(name, async () => {
        if (!id) fail('fixture is missing existing issue or recent Change');
        const start = Date.now();
        const endpoint = query ? `${root}/work-items/${enc(id)}${query}` : `${api}/work-items/${enc(id)}`;
        const loaded = page.waitForResponse(response => response.url().includes(endpoint) && response.status() === 200, { timeout: 15000 });
        const [, response] = await Promise.all([loaded, page.goto(new URL(`/organizations/${enc(org)}/work/i/${enc(id)}${query}`, base).href, { waitUntil: 'load', timeout: 15000 })]);
        await page.waitForLoadState('networkidle', { timeout: 15000 });
        const text = await page.locator('main, [role="main"]').first().innerText();
        rows.push({ name, status: response.status(), expected: 200, ms: Date.now() - start, error: /Unavailable|unexpected payload|Expected array|Sign in/.test(text) ? clean(text).slice(0, 200) : '' });
      });
    }
  } finally { await browser.close(); }
}
async function main() {
  if (!base || !org || !project || !token || process.argv.length !== 6) fail('usage: release-smoke.sh BASE_URL ORG_ID PROJECT_ID CREDENTIAL_ENV');
  const url = new URL(base);
  if (url.username || url.password || url.search || url.hash || url.pathname !== '/' || !['http:', 'https:'].includes(url.protocol)) fail('BASE_URL must be an origin without credentials, path or query');
  if (url.protocol === 'http:' && !['127.0.0.1', 'localhost', '[::1]'].includes(url.hostname)) fail('credentials require HTTPS outside loopback');
  await step('board API', async () => {
    const { value: config } = await request('project lanes', root);
    if (!config.states?.length) fail('project omitted configured lanes');
    for (const state of config.states) await step(`lane ${state.name}`, () => request(`lane ${state.name}`, `${root}/work-items?state=${enc(state.name)}&include=board&limit=50`, { board: true }));
    for (const part of ['counts', 'status']) await step(`board ${part}`, () => request(`board ${part}`, `${root}/board/${part}`, { board: true }));
  });
  let items = [], change, item;
  await step('MCP session', async () => {
    await rpc('initialize', { protocolVersion: protocol, capabilities: {}, clientInfo: { name: 'release-smoke', version: '1' } });
    await rpc('notifications/initialized', {}, 'initialized', true);
    let cursor, count = 0;
    const cursors = new Set();
    do {
      const { value } = await rpc('tools/list', cursor ? { cursor } : {});
      if (!Array.isArray(value.result?.tools)) fail('tools/list omitted tools');
      count += value.result.tools.length; cursor = value.result.nextCursor;
      if (count > 1000 || cursors.size >= 20 || cursor && cursors.has(cursor)) fail('tools/list pagination exceeded bounds or repeated a cursor');
      if (cursor) cursors.add(cursor);
    } while (cursor);
    if (!count) fail('tools/list returned an empty catalog');
    rows.at(-1).note = `${count} tools`;
    await step('work_list no query', async () => { const page = await tool('work_list', { limit: 100 }); items = page.items ?? []; });
    await step('work_list with query', () => tool('work_list', { query: run, limit: 1 }));
    item = process.env.SMOKE_ITEM_ID ?? items[0]?.work_item_id;
    for (const [name, args] of [['work_item', { reference: item }], ['list_comments', { identifier: item, limit: 1 }]]) await step(name, () => item ? tool(name, args) : fail('fixture needs an existing item'));
    await step('recent Change', async () => {
      const candidate = items.find(candidate => candidate.change?.change_id || candidate.list_change?.change_id);
      if (candidate) change = { work_item_id: candidate.work_item_id, change_id: candidate.change?.change_id ?? candidate.list_change?.change_id };
      if (!change) for (const candidate of items.slice(0, 5)) {
        const page = await tool('list_changes', { work_item_id: candidate.work_item_id, limit: 1 });
        const changes = Array.isArray(page) ? page : page.items;
        if (changes?.[0]) { change = { ...changes[0], work_item_id: candidate.work_item_id }; break; }
      }
      if (process.env.SMOKE_CHANGE_ID) change = { change_id: process.env.SMOKE_CHANGE_ID, work_item_id: process.env.SMOKE_CHANGE_ITEM_ID ?? item };
      if (!change) fail('seed a recent Change or set SMOKE_CHANGE_ID and SMOKE_CHANGE_ITEM_ID');
      await tool('get_change', { work_item_id: change.work_item_id, change_id: change.change_id });
    });
    await step('issue lifecycle', lifecycle);
  });
  for (const [name, path, expected] of [
    ['existing attempts', `/work-items/${enc(item)}/attempts`, 200], ['missing item attempts', '/work-items/wi_00000000000000000000000000000000/attempts', 404],
    ['missing attempt', `/work-items/${enc(item)}/attempts/attempt_00000000000000000000000000000000`, 404], ['issue detail API', `/work-items/${enc(item)}`, 200],
    ['Change detail API', `/work-items/${enc(change?.work_item_id)}/changes/${enc(change?.change_id)}`, 200],
  ]) await step(name, () => item ? request(name, root + path, { expected }) : fail('fixture needs an existing item'));
  if (runnerToken) await step('runner heartbeat identity dry read', () => {
    if (!process.env.SMOKE_RUNNER_ID) fail('SMOKE_RUNNER_ID required with runner credential');
    return request('runner heartbeat identity dry read', `${api}/runners/${enc(process.env.SMOKE_RUNNER_ID)}`, { credential: runnerToken });
  });
  else rows.push({ name: 'runner heartbeat identity dry read', skipped: true, note: 'no test runner credential; no heartbeat was sent' });
  await step('browser journeys', () => browserChecks(item, change));
  if (session) await step('close MCP session', () => request('close MCP session', '/mcp', { method: 'DELETE', expected: 204, headers: { 'Mcp-Session-Id': session, 'Mcp-Protocol-Version': protocol } }));
}
await step('smoke setup', main);
process.stdout.write(JSON.stringify(rows));
