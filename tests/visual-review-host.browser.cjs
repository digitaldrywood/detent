#!/usr/bin/env node
'use strict';

// Browser contract fixture: this serves the real vendored viewer/host assets while
// simulating Detent's JSON API. It is not an end-to-end test of the Go handlers.

const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');
const { chromium } = require('/Users/michaelvisser/Development/digitaldrywood/detent/node_modules/playwright');

const root = path.resolve(__dirname, '..');
const assetsRoot = path.join(root, 'static', 'visual-review');
const chrome = '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
const png = Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAIAAAACCAYAAABytg0kAAAAFElEQVR42mNkYPj/n4GBgYGJAQoAHgQCAf+aP9sAAAAASUVORK5CYII=', 'base64');
const headSHA = '1'.repeat(40);
const baseSHA = '2'.repeat(40);

function manifest(captureID) {
  return {
    schema_version: 1,
    capture_id: captureID,
    title: 'Host adapter fixture',
    repository: 'digitaldrywood/detent',
    pr: 321,
    head_sha: headSHA,
    base_sha: baseSHA,
    captured_at: '2026-09-04T12:00:00Z',
    summary: 'Exercises the real vendored host and viewer.',
    coverage_notes: 'Simulated API fixture; browser and static assets are real.',
    changed_files: ['static/example.css'],
    assets: ['wide', 'detail'].map((id, index) => ({
      id,
      path: `media/${id}.png`,
      label: index ? 'Detail screenshot' : 'Wide screenshot',
      kind: index ? 'detail' : 'after',
      observed: `Rendered ${id}`,
      width: 2,
      height: 2,
      inspected: true,
      source: {
        commit: headSHA,
        url: 'https://example.test/dashboard',
        provenance: 'browser fixture',
        state: 'loaded',
        role: 'operator',
        theme: 'light',
        conditions: 'desktop',
        viewport: { width: 1440, height: 1100 },
      },
    })),
    changes: [{
      id: 'dashboard',
      title: 'Dashboard polish',
      description: 'Spacing and hierarchy changed.',
      files: ['static/example.css'],
      asset_ids: ['wide', 'detail'],
      status: 'captured',
    }],
  };
}

function emptyFeedback(captureID) {
  return {
    schema_version: 1,
    repository: 'digitaldrywood/detent',
    pr: 321,
    capture_id: captureID,
    head_sha: headSHA,
    authenticated: false,
    author: 'Fixture reviewer',
    recommendation: 'draft',
    exported_at: '2026-09-04T12:00:00Z',
    annotations: [],
  };
}

async function startFixture() {
  const states = new Map();
  const requests = [];
  let firstSaveRelease = null;
  let firstSaveSeenResolve = null;
  let firstSaveSeen = Promise.resolve();
  const mime = { '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css' };

  const server = http.createServer(async (req, res) => {
    const url = new URL(req.url, 'http://fixture.test');
    const pageMatch = url.pathname.match(/^\/projects\/([^/]+)\/visual-reviews\/([^/]+)\/?$/);
    const apiMatch = url.pathname.match(/^\/api\/v1\/projects\/([^/]+)\/visual-reviews\/([^/]+)(\/draft)?$/);
    const mediaMatch = url.pathname.match(/^\/projects\/([^/]+)\/visual-reviews\/([^/]+)\/media\/([^/]+\.png)$/);
    if (pageMatch) return sendFile(res, path.join(assetsRoot, 'host.html'), 'text/html');
    if (url.pathname.startsWith('/static/visual-review/')) {
      const name = path.basename(url.pathname);
      const file = path.join(assetsRoot, name);
      if (!['host-adapter.js', 'viewer.js', 'schema.js', 'style.css', 'host.css'].includes(name)) return send(res, 404, 'not found');
      return sendFile(res, file, mime[path.extname(file)]);
    }
    if (mediaMatch) return send(res, 200, png, 'image/png');
    if (apiMatch) {
      const project = decodeURIComponent(apiMatch[1]);
      const captureID = decodeURIComponent(apiMatch[2]);
      const state = states.get(project) || { revision: 0, feedback: null, writable: true, mode: 'normal' };
      states.set(project, state);
      if (req.method === 'GET' && !apiMatch[3]) {
        return json(res, 200, {
          manifest: manifest(captureID), feedback: state.feedback, revision: state.revision,
          writable: state.writable, read_only_reason: state.writable ? '' : 'This is an older capture. Browsing remains available.',
        });
      }
      if (req.method === 'PUT' && apiMatch[3]) {
        const body = JSON.parse(await readBody(req));
        requests.push({ project, body });
        if (state.mode === 'delay-first' && requests.filter(r => r.project === project).length === 1) {
          firstSaveSeenResolve();
          await new Promise(resolve => { firstSaveRelease = resolve; });
        }
        if (state.mode === 'conflict') return json(res, 409, { error: 'conflict' });
        if (state.mode === 'network') return send(res, 503, 'offline');
        if (body.expected_revision !== state.revision) return json(res, 409, { error: 'revision conflict' });
        state.feedback = body.feedback;
        state.revision++;
        return json(res, 200, { revision: state.revision, updated_at: new Date().toISOString() });
      }
    }
    send(res, 404, 'not found');
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  return {
    url: `http://127.0.0.1:${server.address().port}`,
    requests,
    state(project) {
      if (!states.has(project)) states.set(project, { revision: 0, feedback: null, writable: true, mode: 'normal' });
      return states.get(project);
    },
    delayFirstSave() {
      firstSaveSeen = new Promise(resolve => { firstSaveSeenResolve = resolve; });
    },
    firstSaveSeen: () => firstSaveSeen,
    releaseFirstSave: () => firstSaveRelease?.(),
    close: () => new Promise((resolve, reject) => server.close(err => err ? reject(err) : resolve())),
  };
}

function sendFile(res, file, contentType) {
  send(res, 200, fs.readFileSync(file), contentType);
}
function send(res, status, body, contentType = 'text/plain') {
  res.writeHead(status, { 'Content-Type': contentType, 'Cache-Control': 'no-store' });
  res.end(body);
}
function json(res, status, value) {
  send(res, status, JSON.stringify(value), 'application/json');
}
function readBody(req) {
  return new Promise((resolve, reject) => {
    let body = '';
    req.setEncoding('utf8');
    req.on('data', chunk => { body += chunk; });
    req.on('end', () => resolve(body));
    req.on('error', reject);
  });
}

async function observedPage(browser, fixture, project, captureID = 'capture-fixture') {
  const context = await browser.newContext({ acceptDownloads: true });
  const page = await context.newPage();
  const errors = [];
  page.on('pageerror', error => errors.push(`pageerror: ${error.message}`));
  page.on('console', message => {
	if (message.type() === 'error' && !message.text().startsWith('Failed to load resource:')) errors.push(`console: ${message.text()}`);
  });
  await page.addInitScript(() => {
    globalThis.__cspViolations = [];
    document.addEventListener('securitypolicyviolation', event => globalThis.__cspViolations.push(event.violatedDirective));
  });
  await page.goto(`${fixture.url}/projects/${project}/visual-reviews/${captureID}/`, { waitUntil: 'networkidle' });
  await page.waitForSelector('#title:not(:has-text("Loading"))');
  return { context, page, errors };
}

async function assertClean(page, errors) {
  assert.deepEqual(errors, [], `unexpected browser errors:\n${errors.join('\n')}`);
  assert.deepEqual(await page.evaluate(() => globalThis.__cspViolations), [], 'unexpected CSP violations');
}

async function addNote(page, text) {
	if (!await page.locator('#lightbox').evaluate(dialog => dialog.open)) {
		await page.getByRole('button', { name: 'Open and annotate Wide screenshot' }).click();
	}
	await page.locator('#comment').fill(text);
	await page.locator('#annotation-form button[type=submit]').click();
}

async function run() {
  const fixture = await startFixture();
  const browser = await chromium.launch({ executablePath: chrome, channel: 'chrome', headless: true });
  let failures = 0;
  async function test(name, fn) {
    try {
      await fn();
      console.log(`ok - ${name}`);
    } catch (error) {
      failures++;
      console.error(`not ok - ${name}\n${error.stack || error}`);
    }
  }

  try {
    await test('serializes annotation and per-asset saves, then reloads the persisted draft', async () => {
      const project = 'serialized';
      fixture.state(project).mode = 'delay-first';
      fixture.delayFirstSave();
      const { context, page, errors } = await observedPage(browser, fixture, project);
      await page.locator('#author').fill('Ada');
      await page.locator('#author').dispatchEvent('change');
      await fixture.firstSaveSeen();
      await addNote(page, 'Align the summary with the card edge.');
	  await page.locator('#modal-approval').getByLabel('Approve Wide screenshot').check();
	  await page.locator('#close-zoom').click();
	  await page.getByRole('button', { name: 'Open and annotate Detail screenshot' }).click();
	  await page.locator('#modal-approval').getByLabel('Approve Detail screenshot').check();
      assert.equal(await page.locator('#recommendation option[value="recommend-approval"]').isDisabled(), true, 'recommendation stays disabled while a save is pending');
      fixture.releaseFirstSave();
      await page.waitForFunction(() => document.querySelector('#detent-save-status')?.textContent.startsWith('Saved on this Detent node'));
      assert.equal(await page.locator('#recommendation option[value="recommend-approval"]').isDisabled(), false, 'recommendation re-enables after completed saves');
      const writes = fixture.requests.filter(r => r.project === project);
      assert.ok(writes.length >= 2, 'pending edits should cause a follow-up save');
      assert.deepEqual(writes.map(r => r.body.expected_revision), writes.map((_, index) => index), 'saves use sequential expected revisions');
      assert.equal(writes.at(-1).body.feedback.annotations[0].text, 'Align the summary with the card edge.');
      assert.equal(writes.at(-1).body.feedback.asset_approvals.length, 2);
      await page.reload({ waitUntil: 'networkidle' });
      assert.equal(await page.locator('#annotations li').count(), 1, 'annotation survives a new document context');
      assert.equal(await page.getByLabel('Approve Wide screenshot').first().isChecked(), true);
      assert.equal(await page.getByLabel('Approve Detail screenshot').first().isChecked(), true);
      const image = page.locator('#stage img');
	  await image.waitFor({ state: 'attached' });
      assert.equal(await image.evaluate(img => img.currentSrc.endsWith('/projects/serialized/visual-reviews/capture-fixture/media/wide.png')), true, 'transport-rewritten path and extension are retained');
      assert.deepEqual(await image.evaluate(img => [img.complete, img.naturalWidth, img.naturalHeight]), [true, 2, 2], 'genuine PNG loads');
      await assertClean(page, errors);
      await context.close();
    });

    await test('keeps a 409 conflict visible and protects pending work from navigation', async () => {
      const project = 'conflict';
      fixture.state(project).mode = 'conflict';
      const { context, page, errors } = await observedPage(browser, fixture, project);
	  await page.getByRole('button', { name: 'Open and annotate Wide screenshot' }).click();
      await page.locator('#comment').fill('Unsaved conflict note');
      await page.waitForFunction(() => document.querySelector('#detent-save-status')?.textContent.startsWith('Conflict:'));
      assert.match(await page.locator('#detent-save-status').textContent(), /Export your work, then reload/);
      const prevented = await page.evaluate(() => {
        const event = new Event('beforeunload', { cancelable: true });
        return !window.dispatchEvent(event) && event.defaultPrevented;
      });
      assert.equal(prevented, true, 'beforeunload is prevented after conflict');
      await page.locator('#notice').evaluate(node => { node.textContent = 'viewer notice changed'; });
      assert.match(await page.locator('#detent-save-status').textContent(), /^Conflict:/, 'adapter status remains independently visible');
      await assertClean(page, errors);
      await context.close();
    });

    await test('network failure preserves the latest feedback for export', async () => {
      const project = 'network';
      fixture.state(project).mode = 'network';
      const { context, page, errors } = await observedPage(browser, fixture, project);
	  await page.getByRole('button', { name: 'Open and annotate Wide screenshot' }).click();
      await page.locator('#comment').fill('Newest text must be exportable');
      await page.waitForFunction(() => document.querySelector('#detent-save-status')?.textContent.includes('Save failed'));
      await page.locator('#annotation-form button[type=submit]').click();
	  await page.locator('#close-zoom').click();
      const downloadPromise = page.waitForEvent('download');
      await page.locator('#export').click();
      const download = await downloadPromise;
      const exported = JSON.parse(fs.readFileSync(await download.path(), 'utf8'));
      assert.equal(exported.annotations.at(-1).text, 'Newest text must be exportable');
      assert.match(await page.locator('#detent-save-status').textContent(), /Save failed/, 'failure remains visible after export');
      await assertClean(page, errors);
      await context.close();
    });

    await test('read-only mode blocks mutations while retaining media browsing', async () => {
      const project = 'readonly';
      const state = fixture.state(project);
      state.writable = false;
      state.revision = 4;
      state.feedback = emptyFeedback('capture-fixture');
      state.feedback.annotations.push({
        id: 'a-existing', change_id: 'dashboard', asset_id: 'wide', kind: 'note', text: 'Existing note',
        author: 'Prior reviewer', created_at: '2026-09-04T12:05:00Z',
      });
      const { context, page, errors } = await observedPage(browser, fixture, project);
      for (const selector of ['#author', '#recommendation', '#annotation-form button[type=submit]', '#undo', '#redo', '#annotations button', '[data-approval]']) {
        const controls = page.locator(selector);
        assert.ok(await controls.count(), `${selector} exists`);
        for (let index = 0; index < await controls.count(); index++) assert.equal(await controls.nth(index).isDisabled(), true, `${selector} is disabled`);
      }
      assert.match(await page.locator('#detent-save-status').textContent(), /older capture/i);
      await page.getByRole('button', { name: 'Open and annotate Detail screenshot' }).click();
      assert.equal(await page.locator('#lightbox').evaluate(dialog => dialog.open), true, 'media browsing remains available');
      assert.equal(await page.locator('#stage img').evaluate(img => img.complete && img.naturalWidth === 2), true);
      assert.equal(fixture.requests.filter(r => r.project === project).length, 0, 'read-only browsing performs no writes');
      await assertClean(page, errors);
      await context.close();
    });
  } finally {
    await browser.close();
    await fixture.close();
  }
  if (failures) process.exitCode = 1;
}

run().catch(error => {
  console.error(error.stack || error);
  process.exitCode = 1;
});
