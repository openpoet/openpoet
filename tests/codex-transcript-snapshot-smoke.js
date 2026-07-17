#!/usr/bin/env node

// Verifies that loading a large Codex transcript snapshot renders in one
// batch without freezing the tab (regression test for the session-open
// freeze caused by re-rendering a card's full markdown per streamed chunk).

const assert = require('node:assert/strict');
const { chromium } = require('playwright');

const baseURL = process.env.OPENPOET_E2E_URL || 'http://localhost:8080';

async function main() {
  const browser = await chromium.launch({ headless: true });
  const page = await browser.newPage({ viewport: { width: 1366, height: 900 } });
  const consoleErrors = [];
  page.on('console', msg => {
    if (msg.type() === 'error') {
      consoleErrors.push(msg.text());
    }
  });
  page.on('pageerror', err => {
    consoleErrors.push(err.message);
  });

  try {
    await page.goto(baseURL, { waitUntil: 'domcontentloaded' });
    await page.waitForFunction(() => !!window.structuredView, null, { timeout: 15000 });

    const result = await page.evaluate(() => {
      const sessionId = 'codex-snapshot-smoke';

      // Build a realistic post-cap snapshot: ~1.5MB of text across 300
      // items, most of it delivered as streamed delta chunks that share an
      // event id (append: true) so the client-side merge path is exercised.
      const events = [];
      const chunk = 'lorem ipsum dolor sit amet, consectetur adipiscing elit. '.repeat(9); // ~512B
      const itemCount = 300;
      const chunksPerItem = 10;
      for (let id = 1; id <= itemCount; id++) {
        const kind = id % 5 === 0 ? 'user' : (id % 3 === 0 ? 'command' : 'assistant');
        events.push({
          id,
          kind,
          text: chunk,
          command: kind === 'command' ? `echo test-${id}` : '',
          created_at: new Date().toISOString(),
        });
        for (let c = 1; c < chunksPerItem; c++) {
          events.push({ id, kind, text: chunk, append: true });
        }
      }

      const start = performance.now();
      const ok = window.structuredView.loadCodexTranscriptSnapshot(sessionId, {
        events,
        truncated: true,
      });
      const durationMs = performance.now() - start;

      const view = window.structuredView.views.get(sessionId);
      const container = view?.messagesEl;
      const totalBytes = events.reduce((n, e) => n + (e.text ? e.text.length : 0), 0);
      const firstItem = view?.codexTranscript?.items?.get(1);
      return {
        ok,
        durationMs,
        totalBytes,
        eventCount: events.length,
        itemCount: view?.codexTranscript?.order?.length ?? 0,
        cardCount: container ? container.querySelectorAll('.sv-message').length : 0,
        noticeCount: container ? container.querySelectorAll('.sv-transcript-notice').length : 0,
        firstItemTextLength: firstItem ? firstItem.text.length : 0,
        expectedItemTextLength: chunk.length * chunksPerItem,
      };
    });

    assert.equal(result.ok, true, 'loadCodexTranscriptSnapshot should return true');
    assert.equal(result.itemCount, 300, `expected 300 merged items, got ${result.itemCount}`);
    assert.ok(result.cardCount >= 300, `expected >= 300 rendered cards, got ${result.cardCount}`);
    assert.equal(result.noticeCount, 1, 'expected a single truncation notice');
    assert.equal(
      result.firstItemTextLength,
      result.expectedItemTextLength,
      'delta chunks must concatenate into the full item text'
    );
    assert.ok(
      result.durationMs < 5000,
      `snapshot render took ${Math.round(result.durationMs)}ms (budget 5000ms)`
    );

    const fatalErrors = consoleErrors.filter(t => !t.includes('favicon'));
    assert.deepEqual(fatalErrors, [], `console errors: ${fatalErrors.join(' | ')}`);

    console.log(
      `PASS codex-transcript-snapshot-smoke: rendered ${result.itemCount} items ` +
      `(${result.eventCount} raw events, ${(result.totalBytes / 1e6).toFixed(1)}MB text) ` +
      `in ${Math.round(result.durationMs)}ms`
    );
  } finally {
    await browser.close();
  }
}

main().catch(err => {
  console.error('FAIL codex-transcript-snapshot-smoke:', err.message);
  process.exit(1);
});
