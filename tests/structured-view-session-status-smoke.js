#!/usr/bin/env node

const assert = require('node:assert/strict');
const { chromium } = require('playwright');

async function main() {
  const browser = await chromium.launch({ headless: true });
  const page = await browser.newPage({ viewport: { width: 375, height: 812 } });
  const consoleErrors = [];
  page.on('console', msg => {
    if (msg.type() === 'error') consoleErrors.push(msg.text());
  });
  page.on('pageerror', err => consoleErrors.push(err.message));

  try {
    await page.setContent(`
      <!doctype html>
      <html>
        <body>
          <div id="terminal-containers-wrapper"></div>
          <div id="mobile-input-state" class="mobile-input-state"></div>
          <textarea id="mobile-terminal-input"></textarea>
        </body>
      </html>
    `);
    await page.addScriptTag({ path: 'web/static/js/structured-view.js' });

    const result = await page.evaluate(async () => {
      const sv = window.structuredView;
      if (!sv) return { ok: false, error: 'structuredView missing' };

      const firstId = 'claude-status-first-e2e';
      const secondId = 'claude-status-second-e2e';
      const first = sv.createView(firstId);
      const second = sv.createView(secondId);
      if (!first || !second) return { ok: false, error: 'structured view container missing' };

      first.loaded = true;
      second.loaded = true;
      const stateEl = document.getElementById('mobile-input-state');
      const text = () => stateEl?.textContent.trim() || '';

      sv.show(firstId);
      sv.appendEvent(firstId, {
        type: 'user',
        timestamp: new Date().toISOString(),
        message: { role: 'user', content_blocks: [{ type: 'text', text: 'live first turn' }] },
      });
      const firstLive = text();

      sv.show(secondId);
      const switchedToIdle = text();

      sv.appendEvent(firstId, {
        type: 'progress',
        timestamp: new Date().toISOString(),
        message: null,
      });
      const backgroundEvent = text();

      // Loading an old transcript ending in a user message must not imply that
      // the agent is currently thinking.
      second.loaded = false;
      const originalFetch = window.fetch;
      window.fetch = async url => {
        if (String(url).includes(`/api/sessions/${secondId}/events`)) {
          return {
            json: async () => [{
              type: 'user',
              timestamp: '2025-01-01T00:00:00Z',
              message: { role: 'user', content_blocks: [{ type: 'text', text: 'old unfinished turn' }] },
            }],
          };
        }
        return originalFetch(url);
      };
      await sv.loadEvents(secondId);
      window.fetch = originalFetch;
      const historicalReplay = text();

      sv.appendEvent(secondId, {
        type: 'user',
        timestamp: new Date().toISOString(),
        message: { role: 'user', content_blocks: [{ type: 'text', text: 'live second turn' }] },
      });
      const secondLive = text();
      sv.appendEvent(secondId, {
        type: 'assistant',
        timestamp: new Date().toISOString(),
        message: { role: 'assistant', stop_reason: 'end_turn', content_blocks: [] },
      });
      const secondIdle = text();

      // Reusing the same label after becoming idle must rebuild the indicator.
      sv.appendEvent(secondId, {
        type: 'user',
        timestamp: new Date().toISOString(),
        message: { role: 'user', content_blocks: [{ type: 'text', text: 'another live turn' }] },
      });
      const repeatedLabel = text();

      sv.dispose(firstId);
      sv.dispose(secondId);

      return {
        ok: true,
        firstLive,
        switchedToIdle,
        backgroundEvent,
        historicalReplay,
        secondLive,
        secondIdle,
        repeatedLabel,
      };
    });

    assert.equal(result.ok, true, result.error || 'structured status smoke failed');
    assert.equal(result.firstLive, 'Claude is thinking');
    assert.equal(result.switchedToIdle, '');
    assert.equal(result.backgroundEvent, '');
    assert.equal(result.historicalReplay, '');
    assert.equal(result.secondLive, 'Claude is thinking');
    assert.equal(result.secondIdle, '');
    assert.equal(result.repeatedLabel, 'Claude is thinking');
    assert.deepEqual(consoleErrors, [], `browser console errors:\n${consoleErrors.join('\n')}`);

    await page.screenshot({ path: '/tmp/openpoet-structured-session-status-smoke.png', fullPage: true });
    console.log('Structured view session status smoke passed');
  } finally {
    await browser.close();
  }
}

main().catch(err => {
  console.error(err);
  process.exit(1);
});
