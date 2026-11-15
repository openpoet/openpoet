#!/usr/bin/env node

// Markdown artifacts against a running dev instance: the markdown is rendered
// into a page inside the same sandbox as an HTML artifact, a mermaid block is
// drawn there with no network, the reader annotates the rendered text, the
// source downloads as a .md file, a link clicked in the page is shown before it
// is opened, and printing happens in a frame of its own, the only one that may
// open a dialog.

const assert = require('node:assert/strict');
const { chromium } = require('playwright');

const baseURL = process.env.OPENPOET_E2E_URL || 'http://localhost:8080';

function markdownSource(targetId) {
  return [
    '# Rollout notes',
    '',
    'First line',
    'second line with **bold** text and a rollback point.',
    '',
    '```mermaid',
    'graph TD; Plan-->Ship;',
    '```',
    '',
    `[The plan](/app/artifact/${targetId})`,
    '[The docs](https://example.com/docs)',
    '[A sibling file](other.md)',
    '[Top of the page](#rollout-notes)',
    '',
  ].join('\n');
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
  const context = await browser.newContext({ viewport: { width: 1366, height: 900 }, acceptDownloads: true });
  // A headless browser has no print dialog. The stand-in records which document
  // asked to print, and ends the way a closed dialog does.
  const printed = [];
  await context.exposeBinding('__e2eReportPrint', (_source, href) => { printed.push(href); });
  await context.addInitScript(() => {
    window.print = () => {
      window.__e2eReportPrint(location.href);
      window.dispatchEvent(new Event('afterprint'));
    };
  });
  // The page never reaches the internet; the link it opens is answered here.
  await context.route('https://example.com/**', route => route.fulfill({ contentType: 'text/html', body: '<title>docs</title>' }));

  const page = await context.newPage();
  const pageErrors = [];
  page.on('pageerror', err => pageErrors.push(err.message));
  const created = [];

  try {
    await page.goto(baseURL, { waitUntil: 'networkidle' });

    const target = await api(page, 'POST', '/api/artifacts', {
      title: 'E2E link target', html: '<!doctype html><html><body><h1 id="target">target page</h1></body></html>',
    });
    assert.equal(target.status, 201, target.text);
    created.push(target.json.id);

    const source = markdownSource(target.json.id);
    const published = await api(page, 'POST', '/api/artifacts', { title: 'E2E markdown notes', markdown: source });
    assert.equal(published.status, 201, published.text);
    assert.equal(published.json.format, 'markdown');
    assert.equal(published.json.size_bytes, Buffer.byteLength(source));
    const artifactId = published.json.id;
    created.push(artifactId);

    // Plain text is not an HTML document, and says where it belongs.
    const refused = await api(page, 'POST', '/api/artifacts', { title: 'E2E plain', html: 'no markup here' });
    assert.equal(refused.status, 400, refused.text);
    assert.match(refused.json.error, /as markdown/);

    // --- rendered inside the same sandbox as any artifact
    const renderResponse = page.waitForResponse(r => r.url().includes(`/api/artifacts/${artifactId}/render`));
    await page.evaluate(id => window.artifactViewer.open(id), artifactId);
    const response = await renderResponse;
    const csp = response.headers()['content-security-policy'] || '';
    assert.ok(csp.startsWith('sandbox allow-scripts;'), `render CSP: ${csp}`);
    assert.ok(!csp.includes('allow-modals'), 'the frame the artifact is read in must not open dialogs');
    assert.ok(csp.includes("script-src 'unsafe-inline'") && csp.includes("connect-src 'none'"), `render CSP: ${csp}`);
    assert.equal(await page.locator('#artifact-viewer-frame').getAttribute('sandbox'), 'allow-scripts');

    const frame = page.frameLocator('#artifact-viewer-frame');
    await frame.locator('#openpoet-markdown h1#rollout-notes').waitFor({ timeout: 15000 });
    assert.equal(await frame.locator('#openpoet-markdown strong').textContent(), 'bold');
    assert.equal(await frame.locator('#openpoet-markdown p').first().locator('br').count(), 1, 'a newline in a paragraph is a line break');
    assert.equal(await frame.locator('body').evaluate(() => self.origin), 'null', 'the markdown page must run in an opaque origin');
    assert.equal(await frame.locator('body').evaluate(() => window.__openpoetArtifactBridge === true), true);

    // --- the mermaid block is drawn inside the sandbox, from the inlined library
    await frame.locator('.mermaid svg').waitFor({ timeout: 20000 });
    assert.equal(await frame.locator('pre > code.language-mermaid').count(), 0, 'the diagram source was not replaced by the diagram');
    assert.match(await frame.locator('.mermaid svg').textContent(), /Plan[\s\S]*Ship/);

    // --- the reader annotates the rendered text
    await page.locator('#artifact-viewer-annotate').click();
    await frame.locator('#openpoet-markdown p').first().evaluate(el => {
      const text = [...el.childNodes].find(node => node.nodeType === 3 && node.data.includes('rollback point'));
      const start = text.data.indexOf('rollback point');
      const range = document.createRange();
      range.setStart(text, start);
      range.setEnd(text, start + 'rollback point'.length);
      const selection = getSelection();
      selection.removeAllRanges();
      selection.addRange(range);
      el.dispatchEvent(new MouseEvent('mouseup', { bubbles: true }));
    });
    await page.locator('#artifact-panel-composer:not(.hidden)').waitFor();
    assert.equal(await page.locator('#artifact-composer-quote').textContent(), 'rollback point');
    await page.locator('#artifact-composer-text').fill('Name the rollback point.');
    await page.locator('#artifact-composer-save').click();
    await page.locator('#artifact-panel-list .artifact-annotation').first().waitFor();
    await frame.locator('openpoet-annotations .box.text').first().waitFor({ state: 'attached' });
    await page.locator('#artifact-viewer-annotate').click();

    // An agent that reads the note on a later version is told the passage is
    // still there: it is checked on the rendered page, not on the source.
    const patched = await api(page, 'POST', `/api/artifacts/${artifactId}/patch`, {
      base_version: 1, edits: [{ old: '**bold** text', new: '**bolder** text' }],
    });
    assert.equal(patched.status, 200, patched.text);
    assert.equal(patched.json.format, 'markdown');
    const placed = await api(page, 'GET', `/api/artifacts/${artifactId}/annotations?placement=1&status=open`);
    assert.equal(placed.json.length, 1);
    assert.deepEqual([placed.json[0].placement.checked, placed.json[0].placement.found], [true, true]);
    await page.locator('#artifact-viewer-banner-open').click();
    await frame.locator('#openpoet-markdown strong:text-is("bolder")').waitFor({ timeout: 10000 });
    await frame.locator('.mermaid svg').waitFor({ timeout: 20000 });

    // --- the source downloads as markdown
    assert.equal(await page.locator('#artifact-viewer-download').getAttribute('title'), 'Download markdown');
    const downloading = page.waitForEvent('download');
    await page.locator('#artifact-viewer-download').click();
    const download = await downloading;
    assert.match(download.suggestedFilename(), /-v2\.md$/);
    const stream = await download.createReadStream();
    const chunks = [];
    for await (const chunk of stream) chunks.push(chunk);
    assert.equal(Buffer.concat(chunks).toString('utf8'), source.replace('**bold** text', '**bolder** text'));

    // --- a link clicked in the page is shown, in full, before anything opens
    const linkBar = page.locator('#artifact-viewer-link');
    await frame.locator('a:text-is("The docs")').click();
    await linkBar.locator('span:text-is("https://example.com/docs")').waitFor();
    assert.equal(await page.locator('#artifact-viewer-link-open').textContent(), 'Open in new tab');
    await frame.locator('#openpoet-markdown h1#rollout-notes').waitFor(); // the frame did not navigate away
    const opening = context.waitForEvent('page');
    await page.locator('#artifact-viewer-link-open').click();
    const opened = await opening;
    await opened.waitForURL('https://example.com/docs');
    assert.equal(await opened.evaluate(() => window.opener), null, 'the opened page must not get a handle on OpenPoet');
    await opened.close();
    assert.equal(await linkBar.evaluate(el => el.classList.contains('hidden')), true);

    // A relative link has nothing to be relative to, and a link inside the page stays inside it.
    await frame.locator('a:text-is("A sibling file")').click();
    await frame.locator('a:text-is("Top of the page")').click();
    await page.waitForTimeout(300);
    assert.equal(await linkBar.evaluate(el => el.classList.contains('hidden')), true);
    await frame.locator('#openpoet-markdown h1#rollout-notes').waitFor();

    // The artifact's own script cannot open anything: its message only proposes.
    await frame.locator('body').evaluate(() => {
      parent.postMessage({ source: 'openpoet-artifact-bridge', type: 'link', href: 'javascript:alert(1)' }, '*');
      parent.postMessage({ source: 'openpoet-artifact-bridge', type: 'link', href: '/api/projects' }, '*');
    });
    await page.waitForTimeout(300);
    assert.equal(await linkBar.evaluate(el => el.classList.contains('hidden')), true, 'a forged link that is not a link was offered');
    assert.equal(context.pages().length, 1, 'a message from the frame opened a page');

    // --- printing: a frame of its own, the only one that may open a dialog
    const printResponse = page.waitForResponse(r => r.url().includes(`/api/artifacts/${artifactId}/render`) && r.url().includes('print=1'));
    await page.locator('#artifact-viewer-pdf').click();
    const printCSP = (await printResponse).headers()['content-security-policy'] || '';
    assert.ok(printCSP.startsWith('sandbox allow-scripts allow-modals;'), `print CSP: ${printCSP}`);
    await page.waitForFunction(() => !document.querySelector('.artifact-viewer-print-frame'), null, { timeout: 30000 });
    assert.equal(printed.length, 1, `print was asked ${printed.length} times`);
    assert.match(printed[0], new RegExp(`/api/artifacts/${artifactId}/render\\?v=2&print=1$`));
    assert.equal(await page.locator('#artifact-viewer-frame').getAttribute('sandbox'), 'allow-scripts');

    // --- a link into OpenPoet opens there once the user says so
    await frame.locator('a:text-is("The plan")').click();
    await linkBar.locator(`span:text-is("/app/artifact/${target.json.id}")`).waitFor();
    assert.equal(await page.locator('#artifact-viewer-link-open').textContent(), 'Open');
    await page.locator('#artifact-viewer-link-open').click();
    await frame.locator('#target:text-is("target page")').waitFor({ timeout: 10000 });
    assert.equal(await page.locator('#artifact-viewer-name').textContent(), 'E2E link target');
    assert.equal(await page.locator('#artifact-viewer-download').getAttribute('title'), 'Download HTML');

    // --- what the document route creates is a markdown artifact, opened by its document id
    const document = await api(page, 'POST', '/api/documents', { title: 'E2E document', content: '# E2E document\n\nA short answer.' });
    assert.equal(document.status, 201, document.text);
    created.push(document.json.id);
    assert.equal(document.json.link, `/app/doc/${document.json.id}`);
    const stored = await api(page, 'GET', `/api/artifacts/${document.json.id}`);
    assert.equal(stored.json.format, 'markdown');
    await page.evaluate(id => window.docViewer.open(id), document.json.id);
    await frame.locator('#openpoet-markdown h1:text-is("E2E document")').waitFor({ timeout: 10000 });
    assert.equal(await page.locator('#doc-review-overlay').evaluate(el => el.classList.contains('hidden')), true,
      'a document must open in the artifact viewer');
    assert.equal(await page.locator('#artifact-viewer-download').getAttribute('title'), 'Download markdown');

    assert.deepEqual(pageErrors, [], `page errors: ${pageErrors.join(' | ')}`);
    console.log('artifact markdown smoke: OK');
  } finally {
    for (const id of created) {
      await api(page, 'PATCH', `/api/artifacts/${id}`, { status: 'archived' }).catch(() => {});
    }
    await browser.close();
  }
}

main().catch(err => {
  console.error(err);
  process.exit(1);
});
