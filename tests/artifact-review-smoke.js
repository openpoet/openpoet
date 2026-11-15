#!/usr/bin/env node

// Proposals against a running dev instance: a proposal is an artifact that
// waits for the user. It is named by a bare document id (what a chat card
// carries), opens in the artifact viewer with the buttons that decide it, is
// decided once, and its old /app/doc/<id> link leads to the artifact.
//
// The proposals are made for a project the test creates in a temporary
// directory and removes at the end: approving a memory proposal rewrites the
// memory document (and the CLAUDE.md) of its project.

const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { chromium } = require('playwright');

const baseURL = process.env.OPENPOET_E2E_URL || 'http://localhost:8080';

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
  let proposalId = '';
  let projectId = 0;
  const projectDir = fs.mkdtempSync(path.join(os.tmpdir(), 'openpoet-e2e-review-'));

  try {
    await page.goto(baseURL, { waitUntil: 'networkidle' });

    const project = await api(page, 'POST', '/api/projects', {
      name: `e2e-review-${Date.now()}`, path: projectDir, type: 'local',
    });
    assert.equal(project.status, 201, project.text);
    projectId = project.json.id;

    const proposed = await api(page, 'POST', '/api/memory-doc/propose', {
      project_id: projectId, content: '# E2E proposal\n\nA rule that **will be rejected**.', summary: 'E2E proposal, to be rejected',
    });
    assert.equal(proposed.status, 200, proposed.text);
    proposalId = proposed.json.doc_id;

    // --- it is an artifact with a pending review, and not one of the project's documents
    const detail = await api(page, 'GET', `/api/artifacts/${proposalId}`);
    assert.equal(detail.status, 200, detail.text);
    assert.deepEqual(detail.json.review, { kind: 'memory', status: 'pending' });
    assert.equal(detail.json.format, 'markdown');
    const listed = await api(page, 'GET', `/api/artifacts?project_id=${projectId}`);
    assert.ok(!listed.json.some(artifact => artifact.id === proposalId), 'a proposal is listed among the documents of its project');

    // --- the bare id opens the artifact viewer, with the decision under the page
    await page.evaluate(id => window.docViewer.open(id), proposalId);
    await page.locator('#artifact-viewer-overlay:not(.hidden)').waitFor();
    assert.equal(await page.locator('#doc-review-overlay').evaluate(el => el.classList.contains('hidden')), true,
      'a proposal must not open in the markdown document overlay');
    const frame = page.frameLocator('#artifact-viewer-frame');
    await frame.locator('#openpoet-markdown h1:text-is("E2E proposal")').waitFor({ timeout: 15000 });
    const review = page.locator('#artifact-viewer-review:not(.hidden)');
    await review.waitFor();
    assert.equal(await page.locator('#artifact-viewer-review-status').textContent(), 'Memory document change · waiting for your decision');
    assert.deepEqual(await page.locator('#artifact-viewer-review-actions button').allTextContents(), ['Reject', 'Approve']);

    // --- rejecting decides it and closes the viewer
    await page.locator('#artifact-viewer-review-actions button:text-is("Reject")').click();
    await page.locator('#artifact-viewer-overlay.hidden').waitFor({ state: 'attached' });
    const decided = await api(page, 'GET', `/api/artifacts/${proposalId}`);
    assert.deepEqual(decided.json.review, { kind: 'memory', status: 'rejected' });

    // --- decided once: the routes refuse, and the viewer offers no button
    const again = await api(page, 'POST', `/api/memory-doc/approve/${proposalId}`);
    assert.notEqual(again.status, 200, 'a rejected proposal was approved');
    await page.evaluate(id => window.docViewer.open(id), proposalId);
    await review.waitFor();
    assert.equal(await page.locator('#artifact-viewer-review-status').textContent(), 'Memory document change · rejected');
    assert.equal(await page.locator('#artifact-viewer-review-actions button').count(), 0);
    assert.equal(await page.locator('#artifact-viewer-review').getAttribute('data-status'), 'rejected');
    await page.locator('#artifact-viewer-close').click();
    await page.locator('#artifact-viewer-overlay.hidden').waitFor({ state: 'attached' });

    // --- a plain artifact has no decision bar
    const plain = await api(page, 'POST', '/api/artifacts', { title: 'E2E plain notes', markdown: '# Plain notes' });
    assert.equal(plain.status, 201, plain.text);
    await page.evaluate(id => window.artifactViewer.open(id), plain.json.id);
    await frame.locator('#openpoet-markdown h1:text-is("Plain notes")').waitFor({ timeout: 15000 });
    assert.equal(await page.locator('#artifact-viewer-review').evaluate(el => el.classList.contains('hidden')), true);
    await api(page, 'PATCH', `/api/artifacts/${plain.json.id}`, { status: 'archived' });
    await page.locator('#artifact-viewer-close').click();

    // --- the document link of the proposal leads to the artifact
    const redirected = await page.goto(`${baseURL}/app/doc/${proposalId}`, { waitUntil: 'networkidle' });
    assert.equal(redirected.request().redirectedFrom()?.url(), `${baseURL}/app/doc/${proposalId}`);
    await page.locator('#artifact-viewer-overlay:not(.hidden)').waitFor({ timeout: 15000 });
    await page.frameLocator('#artifact-viewer-frame').locator('#openpoet-markdown h1:text-is("E2E proposal")').waitFor({ timeout: 15000 });
    assert.ok(decodeURIComponent(page.url()).includes(`artifact:${proposalId}`), `URL does not name the artifact: ${page.url()}`);
    const link = await api(page, 'GET', `/api/links?kind=document&id=${proposalId}`);
    assert.equal(link.json.path, `/app/artifact/${proposalId}`);
    await page.locator('#artifact-viewer-close').click();
    await page.locator('#artifact-viewer-overlay.hidden').waitFor({ state: 'attached' });

    // --- approving a proposal applies it, from the same bar
    const second = await api(page, 'POST', '/api/memory-doc/propose', {
      project_id: projectId, content: '# E2E memory\n\nA rule that is approved.', summary: 'E2E proposal, to be approved',
    });
    assert.equal(second.status, 200, second.text);
    await page.evaluate(id => window.docViewer.open(id), second.json.doc_id);
    await review.waitFor();
    await page.locator('#artifact-viewer-review-actions button:text-is("Approve")').click();
    await page.locator('#artifact-viewer-overlay.hidden').waitFor({ state: 'attached' });
    const approved = await api(page, 'GET', `/api/artifacts/${second.json.doc_id}`);
    assert.deepEqual(approved.json.review, { kind: 'memory', status: 'approved' });
    const memory = await api(page, 'GET', `/api/projects/${projectId}/memory-doc`);
    assert.equal(memory.status, 200, memory.text);
    assert.match(memory.json.content, /A rule that is approved\./);
    await api(page, 'PATCH', `/api/artifacts/${second.json.doc_id}`, { status: 'archived' });

    assert.deepEqual(pageErrors, [], `page errors: ${pageErrors.join(' | ')}`);
    console.log('artifact review smoke: OK');
  } finally {
    if (proposalId) {
      await api(page, 'PATCH', `/api/artifacts/${proposalId}`, { status: 'archived' }).catch(() => {});
    }
    if (projectId) {
      await api(page, 'DELETE', `/api/projects/${projectId}`).catch(() => {});
    }
    await browser.close();
    fs.rmSync(projectDir, { recursive: true, force: true });
  }
}

main().catch(err => {
  console.error(err);
  process.exit(1);
});
