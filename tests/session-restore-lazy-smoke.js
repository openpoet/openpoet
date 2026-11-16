#!/usr/bin/env node

// Startup with many saved session tabs must not open them one by one.
// Restoring used to call openTerminal for every saved tab, and each call
// reloaded the whole session list and replayed a terminal buffer, so the UI
// waited on N slow round trips. Now the list loads once, only the active tab
// gets a terminal, the other tabs connect when opened, and their pending
// hook state loads in the background.
//
// The page comes from the dev server; app.js and terminal.js are served from
// this working tree and the session API is mocked, so the test needs no real
// sessions and checks the code under review rather than the dev build.

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require('playwright');

const baseURL = process.env.OPENPOET_E2E_URL || 'http://localhost:8080';
const JS_DIR = path.resolve(__dirname, '..', 'web', 'static', 'js');
const TOTAL = 40;
const RUNNING = 6;

const ids = Array.from({ length: TOTAL }, (_, i) => `00000000-0000-4000-8000-${String(i).padStart(12, '0')}`);
const row = i => ({
  id: ids[i], project_id: 1, status: i < RUNNING ? 'running' : 'completed', pid: 0,
  start_time: new Date(Date.now() - i * 3600e3).toISOString(),
  name: `Lazy session ${i}`, task_id: { Int64: 0, Valid: false },
  last_activity_at: { Time: new Date().toISOString(), Valid: true },
  plan_content: '', backend: 'claude_code', model: '', effort: '', requested_model: '',
});

async function main() {
  const browser = await chromium.launch({ headless: true });
  const context = await browser.newContext({ viewport: { width: 1366, height: 900 }, serviceWorkers: 'block' });
  const page = await context.newPage();
  const pageErrors = [];
  page.on('pageerror', err => pageErrors.push(err.message));
  const seen = { list: [], output: [], ws: [], hooks: [] };

  try {
    await page.route(/\/static\/js\/(app|terminal)\.js(\?.*)?$/, route => {
      const name = new URL(route.request().url()).pathname.split('/').pop();
      route.fulfill({ status: 200, contentType: 'application/javascript', body: fs.readFileSync(path.join(JS_DIR, name), 'utf8') });
    });
    await page.route(/\/api\/sessions(\?.*)?$/, async route => {
      if (route.request().method() !== 'GET') return route.continue();
      seen.list.push(route.request().url());
      await new Promise(r => setTimeout(r, 150)); // the list is the slow call
      route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(ids.map((_, i) => row(i))) });
    });
    await page.route(/\/api\/sessions\/active-details$/, route => route.fulfill({
      status: 200, contentType: 'application/json',
      body: JSON.stringify(ids.slice(0, RUNNING).map((_, i) => ({ ...row(i), project_name: 'Lazy project', project_type: 'local', execution_mode: 'idle' }))),
    }));
    await page.route(/\/api\/sessions\/[0-9a-f-]{36}\/output$/, route => {
      seen.output.push(route.request().url().split('/').at(-2));
      route.fulfill({ status: 200, contentType: 'application/octet-stream', body: 'history line\r\n'.repeat(50) });
    });
    await page.route(/\/api\/sessions\/[0-9a-f-]{36}\/task$/, route => route.fulfill({ status: 404, contentType: 'application/json', body: '{"error":"no task"}' }));
    await page.route(/\/api\/sessions\/[0-9a-f-]{36}\/(tools|documents|events)(\?.*)?$/, route => route.fulfill({ status: 200, contentType: 'application/json', body: '[]' }));
    await page.route(/\/api\/sessions\/[0-9a-f-]{36}\/plan$/, route => route.fulfill({ status: 200, contentType: 'application/json', body: '{}' }));
    await page.route(/\/api\/hooks\/pending\/[0-9a-f-]{36}$/, route => {
      seen.hooks.push(route.request().url().split('/').pop());
      route.fulfill({ status: 200, contentType: 'application/json', body: '{}' });
    });
    await page.routeWebSocket(/\/ws\/session\//, ws => { seen.ws.push(ws.url().split('/').pop()); ws.onMessage(() => {}); });

    const activeId = ids[3];
    await page.addInitScript(({ tabs, activeId }) => {
      localStorage.setItem('openpoet-tabs-state', JSON.stringify({ tabs, activeSessionId: activeId, timestamp: Date.now() }));
    }, { tabs: ids.slice(0, RUNNING).map(id => ({ sessionId: id, projectName: 'P', sessionName: id })), activeId });

    await page.goto(baseURL, { waitUntil: 'domcontentloaded' });
    await page.waitForFunction(id => window.app?._initialURLRestored && window.terminalManager?.activeSessionId === id, activeId, { timeout: 20000 });
    // Background prefetch of the other tabs' hook state runs on idle.
    await page.waitForFunction(n => document.querySelectorAll('.terminal-tab').length === n, RUNNING);
    await page.waitForTimeout(1500);

    // Startup: one list load, light view, only the active tab connected.
    assert.equal(seen.list.length, 1, `session list loaded ${seen.list.length} times: ${seen.list.join(', ')}`);
    assert.match(seen.list[0], /[?&]view=summary\b/);
    assert.deepEqual(seen.ws, [activeId], 'only the active tab connects at startup');
    assert.deepEqual(seen.output, [activeId], 'only the active tab replays its history at startup');
    assert.deepEqual(new Set(seen.hooks), new Set(ids.slice(0, RUNNING)), 'every restored tab has its hook state loaded');
    const startup = await page.evaluate(() => ({
      view: window.app.currentView,
      terminals: [...window.terminalManager.terminals.keys()],
      tabs: [...document.querySelectorAll('.terminal-tab')].map(t => t.dataset.sessionId),
      active: document.querySelector('.terminal-tab.active')?.dataset.sessionId,
    }));
    assert.equal(startup.view, 'terminal');
    assert.deepEqual(startup.terminals, [activeId]);
    assert.equal(startup.tabs.length, RUNNING);
    assert.equal(startup.active, activeId);

    // Opening another tab connects it on demand, without a list reload.
    const clicked = ids[1];
    await page.click(`.terminal-tab[data-session-id="${clicked}"] .terminal-tab-name`);
    await page.waitForFunction(id => window.terminalManager.activeSessionId === id
      && document.getElementById(`terminal-container-${id}`)?.classList.contains('active'), clicked);
    assert.ok(seen.ws.includes(clicked), 'clicked tab connected');
    assert.equal(seen.list.length, 1, 'opening a tab does not reload the session list');

    // Keyboard tab switching also connects an unconnected tab.
    const next = await page.evaluate(() => {
      const order = [...window.app.openTabs.keys()];
      const target = order[(order.indexOf(window.terminalManager.activeSessionId) + 1) % order.length];
      window.app.switchToNextTab();
      return target;
    });
    await page.waitForFunction(id => window.terminalManager.activeSessionId === id && window.terminalManager.hasSession(id), next);
    assert.ok(seen.ws.includes(next), 'next tab connected');

    assert.deepEqual(pageErrors.filter(m => !/sandboxed/.test(m)), [], `page errors:\n${pageErrors.join('\n')}`);
    console.log('Session restore lazy-load smoke passed');
  } finally {
    await browser.close();
  }
}

main().catch(err => {
  console.error(err);
  process.exit(1);
});
