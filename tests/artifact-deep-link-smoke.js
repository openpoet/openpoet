#!/usr/bin/env node

// A link to an artifact, opened cold on a phone-sized browser against a
// running dev instance: every link shape OpenPoet hands out has to land in the
// right viewer, on the right version, without the user finding their way there.

const assert = require('node:assert/strict');
const { chromium } = require('playwright');

const baseURL = process.env.OPENPOET_E2E_URL || 'http://localhost:8080';

const page1 = '<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Link</title></head><body><h1 id="h">Deep link v1</h1></body></html>';
const page2 = page1.replace('Deep link v1', 'Deep link v2');

async function api(page, method, path, body) {
  return page.evaluate(async ({ method, path, body }) => {
    const response = await fetch(path, {
      method,
      headers: body ? { 'Content-Type': 'application/json' } : undefined,
      body: body ? JSON.stringify(body) : undefined,
    });
    const text = await response.text();
    let json = null;
    try { json = JSON.parse(text); } catch {}
    return { status: response.status, json, text };
  }, { method, path, body });
}

// Opens url in a browser context that has never seen the app (no cookies, no
// stored tabs), as a link tapped in a chat app would.
async function openCold(browser, url) {
  const context = await browser.newContext({
    viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true, deviceScaleFactor: 2,
  });
  const page = await context.newPage();
  const errors = [];
  page.on('pageerror', err => errors.push(err.message));
  await page.goto(baseURL + url, { waitUntil: 'load' });
  return { context, page, errors };
}

async function viewerState(page) {
  return page.evaluate(() => {
    const overlay = document.getElementById('artifact-viewer-overlay');
    const frame = document.getElementById('artifact-viewer-frame');
    const rect = overlay ? overlay.getBoundingClientRect() : null;
    return {
      open: !!window.artifactViewer && window.artifactViewer.isOpen(),
      title: document.getElementById('artifact-viewer-name')?.textContent || '',
      version: document.getElementById('artifact-viewer-version')?.value || '',
      frameSrc: frame?.getAttribute('src') || '',
      status: document.getElementById('artifact-viewer-status')?.textContent || '',
      url: location.pathname + location.search + location.hash,
      fitsViewport: !!rect && rect.width <= window.innerWidth + 1 && document.documentElement.scrollWidth <= window.innerWidth + 1,
    };
  });
}

async function waitForViewer(page) {
  await page.waitForFunction(() => window.artifactViewer && window.artifactViewer.isOpen() &&
    document.getElementById('artifact-viewer-name')?.textContent !== 'Artifact', null, { timeout: 15000 });
  // The address is canonicalized once the restore finishes.
  await page.waitForFunction(() => !location.pathname.startsWith('/app') && location.search.includes('doc='), null, { timeout: 15000 });
}

async function main() {
  const browser = await chromium.launch({ headless: true });
  const setup = await browser.newPage();
  let artifactId = '';
  try {
    await setup.goto(baseURL, { waitUntil: 'networkidle' });
    const created = await api(setup, 'POST', '/api/artifacts', { title: 'E2E deep link', html: page1, session_id: `e2e-deep-link-${Date.now()}` });
    assert.equal(created.status, 201, created.text);
    artifactId = created.json.id;
    assert.equal(created.json.link, `/app/artifact/${artifactId}`);
    const updated = await api(setup, 'PUT', `/api/artifacts/${artifactId}`, { html: page2, base_version: 1 });
    assert.equal(updated.status, 200, updated.text);
    const document_ = await api(setup, 'POST', '/api/documents', { title: 'E2E deep link doc', content: '# Deep link doc\n\nBody.' });
    assert.equal(document_.status, 201, document_.text);
    const docId = document_.json.id;

    // The link the server hands out is the path form.
    const link = await api(setup, 'GET', `/api/links?kind=artifact&id=${artifactId}`);
    assert.equal(link.status, 200, link.text);
    assert.equal(link.json.path, `/app/artifact/${artifactId}`);
    const canonical = `/?view=projects&doc=artifact%3A${artifactId}`;

    // --- the path link opens the artifact viewer on the current version
    let opened = await openCold(browser, link.json.path);
    await waitForViewer(opened.page);
    let state = await viewerState(opened.page);
    assert.equal(state.title, 'E2E deep link');
    assert.equal(state.version, '2');
    assert.match(state.frameSrc, new RegExp(`/api/artifacts/${artifactId}/render\\?v=2$`));
    assert.equal(state.url, canonical, 'the address is rewritten into the canonical form');
    assert.equal(state.fitsViewport, true, 'the viewer fits a phone screen');
    const heading = await opened.page.frameLocator('#artifact-viewer-frame').locator('#h').textContent();
    assert.equal(heading, 'Deep link v2');
    assert.deepEqual(opened.errors, []);
    await opened.context.close();

    // --- a version link opens that version and keeps it in the address
    opened = await openCold(browser, `/app/artifact/${artifactId}?v=1`);
    await waitForViewer(opened.page);
    state = await viewerState(opened.page);
    assert.equal(state.version, '1');
    assert.match(state.frameSrc, /render\?v=1$/);
    assert.equal(state.url, `${canonical}&v=1`);
    // Reloading that address opens the same version again.
    await opened.page.reload({ waitUntil: 'load' });
    await waitForViewer(opened.page);
    assert.equal((await viewerState(opened.page)).version, '1');
    // Moving to the latest version drops it from the address.
    await opened.page.selectOption('#artifact-viewer-version', '2');
    await opened.page.waitForFunction(() => !location.search.includes('v='), null, { timeout: 5000 });
    assert.equal((await viewerState(opened.page)).url, canonical);
    await opened.context.close();

    // --- the query form and the notification form open it too
    for (const shape of [`/?doc=artifact:${artifactId}`, `/#link=${encodeURIComponent(`/app/artifact/${artifactId}`)}`]) {
      opened = await openCold(browser, shape);
      await waitForViewer(opened.page);
      state = await viewerState(opened.page);
      assert.equal(state.title, 'E2E deep link', `opened ${shape}`);
      assert.equal(state.url, canonical, `opened ${shape}`);
      await opened.context.close();
    }

    // --- a Doc link opens the document viewer, not the artifact viewer
    opened = await openCold(browser, `/app/doc/${docId}`);
    await opened.page.waitForFunction(() => window.docViewer && !window.docViewer.overlay.classList.contains('hidden'), null, { timeout: 15000 });
    await opened.page.waitForFunction(() => !location.pathname.startsWith('/app'), null, { timeout: 15000 });
    assert.equal(await opened.page.evaluate(() => window.artifactViewer.isOpen()), false);
    assert.equal(await opened.page.evaluate(() => location.pathname + location.search), `/?view=projects&doc=${docId}`);
    await opened.context.close();

    // --- a link to an artifact that does not exist says so
    opened = await openCold(browser, '/app/artifact/zzzz0000');
    await opened.page.waitForFunction(() => /does not exist/.test(document.getElementById('artifact-viewer-status')?.textContent || ''), null, { timeout: 15000 });
    await opened.context.close();

    // --- an /app path that is not a link goes home
    opened = await openCold(browser, '/app/nothing/here');
    await opened.page.waitForFunction(() => location.pathname === '/', null, { timeout: 15000 });
    assert.equal(await opened.page.evaluate(() => window.artifactViewer.isOpen()), false);
    await opened.context.close();

    console.log('artifact deep link smoke: OK');
  } finally {
    if (artifactId) {
      await api(setup, 'PATCH', `/api/artifacts/${artifactId}`, { status: 'archived' }).catch(() => {});
    }
    await browser.close();
  }
}

main().catch(err => {
  console.error(err);
  process.exit(1);
});
