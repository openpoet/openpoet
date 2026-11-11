#!/usr/bin/env node

// HTML artifact viewer against a running dev instance: the artifact renders in
// the sandboxed iframe, cannot reach cookies, storage, the OpenPoet API, the
// parent page or the network, and a newly published version is offered live.

const assert = require('node:assert/strict');
const { chromium } = require('playwright');

const baseURL = process.env.OPENPOET_E2E_URL || 'http://localhost:8080';

// The probe is the "hostile artifact": it tries every way out and reports what
// happened in its own DOM, which the test reads through the browser.
function probeHTML(label) {
  return `<!doctype html><html><head><meta charset="utf-8"><title>${label}</title>
<style>body{font-family:system-ui;margin:2rem}</style></head>
<body><h1 id="label">${label}</h1><p id="copy">A paragraph of ordinary text.</p><pre id="out">running</pre>
<script>
(async () => {
  const r = {};
  try { r.cookie = 'read:' + document.cookie; } catch (e) { r.cookie = 'ERR ' + e.name; }
  try { localStorage.setItem('probe', '1'); r.storage = 'ok'; } catch (e) { r.storage = 'ERR ' + e.name; }
  try { const x = await fetch('/api/projects'); r.fetch = 'HTTP ' + x.status; } catch (e) { r.fetch = 'ERR ' + e.name; }
  try { const x = await fetch('https://example.com/'); r.external = 'HTTP ' + x.status; } catch (e) { r.external = 'ERR ' + e.name; }
  // Opened directly in a tab there is no parent page to reach (parent === self).
  try { r.parent = parent === self ? 'self' : 'read:' + parent.document.title; } catch (e) { r.parent = 'ERR ' + e.name; }
  try { r.popup = window.open('about:blank') ? 'opened' : 'blocked'; } catch (e) { r.popup = 'ERR ' + e.name; }
  r.image = await new Promise(resolve => {
    const img = new Image();
    img.onload = () => resolve('loaded');
    img.onerror = () => resolve('blocked');
    img.src = '/static/favicon.svg';
    setTimeout(() => resolve('timeout'), 3000);
  });
  r.script = await new Promise(resolve => {
    const s = document.createElement('script');
    s.onload = () => resolve('loaded');
    s.onerror = () => resolve('blocked');
    s.src = '/static/js/theme.js';
    document.head.appendChild(s);
    setTimeout(() => resolve('timeout'), 3000);
  });
  r.origin = String(self.origin);
  r.inlineScript = 'ran';
  document.getElementById('out').textContent = JSON.stringify(r);
})();
</script></body></html>`;
}

async function readProbe(frame) {
  const out = frame.locator('#out');
  await out.waitFor({ state: 'visible', timeout: 15000 });
  await frame.locator('#out:not(:text-is("running"))').waitFor({ timeout: 15000 });
  return JSON.parse(await out.textContent());
}

function assertContained(result, where, { framed = true } = {}) {
  assert.equal(result.inlineScript, 'ran', `${where}: inline script must run`);
  assert.equal(result.origin, 'null', `${where}: artifact must run in an opaque origin`);
  assert.match(result.cookie, /^ERR /, `${where}: cookies must be unreadable (${result.cookie})`);
  assert.match(result.storage, /^ERR /, `${where}: storage must be unavailable (${result.storage})`);
  assert.match(result.fetch, /^ERR /, `${where}: fetch to the OpenPoet API must fail (${result.fetch})`);
  assert.match(result.external, /^ERR /, `${where}: fetch to the internet must fail (${result.external})`);
  if (framed) {
    assert.match(result.parent, /^ERR /, `${where}: the parent page must be unreachable (${result.parent})`);
  } else {
    assert.equal(result.parent, 'self', `${where}: expected a top-level page`);
  }
  assert.notEqual(result.popup, 'opened', `${where}: popups must be blocked`);
  assert.equal(result.image, 'blocked', `${where}: remote images must be blocked`);
  assert.equal(result.script, 'blocked', `${where}: remote scripts must be blocked`);
}

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

async function main() {
  const browser = await chromium.launch({ headless: true });
  const context = await browser.newContext({ viewport: { width: 1366, height: 900 } });
  const page = await context.newPage();
  const pageErrors = [];
  page.on('pageerror', err => pageErrors.push(err.message));
  let artifactId = '';

  try {
    await page.goto(baseURL, { waitUntil: 'networkidle' });
    const sessionId = `e2e-artifact-${Date.now()}`;

    // --- publish v1
    const created = await api(page, 'POST', '/api/artifacts', {
      title: 'E2E sandbox probe', html: probeHTML('probe v1'), session_id: sessionId,
    });
    assert.equal(created.status, 201, created.text);
    artifactId = created.json.id;
    assert.equal(created.json.version, 1);

    // --- it is listed with the session's documents, as an artifact
    const docs = await api(page, 'GET', `/api/sessions/${sessionId}/documents`);
    assert.deepEqual(
      docs.json.map(d => [d.id, d.type, d.status]),
      [[`artifact:${artifactId}`, 'artifact', 'v1']],
    );

    // --- open through the document viewer entry point (what cards and lists use)
    const renderResponse = page.waitForResponse(r => r.url().includes(`/api/artifacts/${artifactId}/render`));
    await page.evaluate(id => window.docViewer.open(`artifact:${id}`), artifactId);
    const response = await renderResponse;
    const csp = response.headers()['content-security-policy'] || '';
    for (const directive of ["sandbox allow-scripts", "default-src 'none'", "connect-src 'none'", "form-action 'none'", "frame-ancestors 'self'"]) {
      assert.ok(csp.includes(directive), `render CSP lacks "${directive}": ${csp}`);
    }
    assert.equal(response.headers()['x-content-type-options'], 'nosniff');
    assert.equal(response.headers()['set-cookie'], undefined, 'the render response must not set cookies');

    await page.locator('#artifact-viewer-overlay:not(.hidden)').waitFor();
    assert.equal(await page.locator('#doc-review-overlay').evaluate(el => el.classList.contains('hidden')), true,
      'an artifact must not open in the markdown document overlay');
    assert.equal(await page.locator('#artifact-viewer-frame').getAttribute('sandbox'), 'allow-scripts',
      'the iframe sandbox must allow scripts and nothing else');
    assert.equal(await page.locator('#artifact-viewer-name').textContent(), 'E2E sandbox probe');

    const frame = page.frameLocator('#artifact-viewer-frame');
    assert.equal(await frame.locator('#label').textContent(), 'probe v1');
    assertContained(await readProbe(frame), 'viewer iframe');

    // The artifact is addressable in the URL.
    assert.ok(decodeURIComponent(page.url()).includes(`artifact:${artifactId}`), `URL lacks the artifact: ${page.url()}`);

    // --- a new version is offered live, without swapping the page under the reader
    const updated = await api(page, 'PUT', `/api/artifacts/${artifactId}`, {
      html: probeHTML('probe v2'), base_version: 1, change_note: 'second pass',
    });
    assert.equal(updated.status, 200, updated.text);
    assert.equal(updated.json.version, 2);
    const banner = page.locator('#artifact-viewer-banner:not(.hidden)');
    await banner.waitFor({ timeout: 10000 });
    assert.match(await banner.textContent(), /Version 2 was just published/);
    assert.equal(await frame.locator('#label').textContent(), 'probe v1', 'the open version must not change by itself');
    await page.locator('#artifact-viewer-banner-open').click();
    await frame.locator('#label:text-is("probe v2")').waitFor({ timeout: 10000 });
    assert.equal(await page.locator('#artifact-viewer-banner').evaluate(el => el.classList.contains('hidden')), true);
    assert.deepEqual(await page.locator('#artifact-viewer-version option').evaluateAll(os => os.map(o => o.value)), ['2', '1']);

    // Older versions stay readable from the version picker.
    await page.locator('#artifact-viewer-version').selectOption('1');
    await frame.locator('#label:text-is("probe v1")').waitFor({ timeout: 10000 });
    assert.match(await page.locator('#artifact-viewer-banner').textContent(), /viewing version 1/);

    // A stale base version is refused.
    const conflict = await api(page, 'PUT', `/api/artifacts/${artifactId}`, { html: probeHTML('probe v3'), base_version: 1 });
    assert.equal(conflict.status, 409, conflict.text);
    assert.equal(conflict.json.code, 'artifact_version_conflict');

    // --- reload: the URL reopens the artifact
    await page.reload({ waitUntil: 'networkidle' });
    await page.locator('#artifact-viewer-overlay:not(.hidden)').waitFor({ timeout: 10000 });
    await page.frameLocator('#artifact-viewer-frame').locator('#label:text-is("probe v2")').waitFor({ timeout: 10000 });

    // --- close unloads the artifact
    await page.locator('#artifact-viewer-close').click();
    await page.locator('#artifact-viewer-overlay.hidden').waitFor({ state: 'attached' });
    assert.equal(await page.locator('#artifact-viewer-frame').getAttribute('src'), 'about:blank');
    assert.ok(!decodeURIComponent(page.url()).includes('artifact:'), `URL still names the artifact: ${page.url()}`);

    // --- opened directly in a tab (no iframe), the response's own CSP still sandboxes it
    const direct = await context.newPage();
    await direct.goto(`${baseURL}/api/artifacts/${artifactId}/render`, { waitUntil: 'load' });
    assertContained(await readProbe(direct), 'direct navigation', { framed: false });
    await direct.close();

    assert.deepEqual(pageErrors, [], `page errors: ${pageErrors.join(' | ')}`);
    console.log('artifact viewer smoke: OK');
  } finally {
    if (artifactId) {
      await api(page, 'PATCH', `/api/artifacts/${artifactId}`, { status: 'archived' }).catch(() => {});
    }
    await browser.close();
  }
}

main().catch(err => {
  console.error(err);
  process.exit(1);
});
