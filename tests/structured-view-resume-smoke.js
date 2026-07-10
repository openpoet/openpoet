#!/usr/bin/env node

const assert = require('node:assert/strict');
const { chromium } = require('playwright');

const baseURL = process.env.OPENPOET_E2E_URL || 'http://localhost:8080';

async function main() {
  const browser = await chromium.launch({ headless: true });
  const page = await browser.newPage({ viewport: { width: 1366, height: 900 } });
  const consoleErrors = [];
  page.on('console', msg => {
    if (msg.type() === 'error') consoleErrors.push(msg.text());
  });
  page.on('pageerror', err => consoleErrors.push(err.message));

  try {
    await page.goto(baseURL, { waitUntil: 'networkidle' });

    const result = await page.evaluate(async () => {
      const sv = window.structuredView;
      if (!sv) return { ok: false, error: 'structuredView missing' };

      const sessionId = 'claude-resume-e2e';
      const view = sv.createView(sessionId);
      if (!view) return { ok: false, error: 'structured view container missing' };

      view.loaded = true;
      sv.appendEvent(sessionId, {
        type: 'user',
        uuid: 'abandoned-new-session',
        timestamp: new Date().toISOString(),
        message: { role: 'user', content_blocks: [{ type: 'text', text: '/resume' }] },
      });
      view.totalTokens.input = 99;
      view.uuidTypeMap.set('stale', 'user');
      sv._updateTokenBar(view);

      sv.handleSourceChange(sessionId, { replaying: true });
      const reset = {
        eventCount: view.events.length,
        uuidCount: view.uuidTypeMap.size,
        inputTokens: view.totalTokens.input,
        placeholder: view.messagesEl.textContent.trim(),
        loaded: view.loaded,
      };

      sv.appendEvent(sessionId, {
        type: 'user',
        uuid: 'resumed-user',
        timestamp: new Date().toISOString(),
        message: { role: 'user', content_blocks: [{ type: 'text', text: 'mensagem do histórico retomado' }] },
      });
      sv.appendEvent(sessionId, {
        type: 'assistant',
        uuid: 'resumed-assistant',
        timestamp: new Date().toISOString(),
        message: {
          role: 'assistant',
          message_id: 'resumed-message',
          stop_reason: 'end_turn',
          content_blocks: [{ type: 'text', text: 'resposta após o resume' }],
          usage: { input_tokens: 7, output_tokens: 5 },
        },
      });

      // Reproduce the fetch race: an old source load completes after the
      // source-change notification. Its response must not overwrite replayed
      // events from the resumed transcript.
      const staleId = 'claude-resume-stale-fetch-e2e';
      const staleView = sv.createView(staleId);
      const originalFetch = window.fetch;
      let releaseStale;
      const staleResponse = new Promise(resolve => { releaseStale = resolve; });
      window.fetch = async url => {
        if (String(url).includes(`/api/sessions/${staleId}/events`)) {
          await staleResponse;
          return {
            json: async () => [{
              type: 'user',
              uuid: 'stale-event',
              timestamp: new Date().toISOString(),
              message: { role: 'user', content_blocks: [{ type: 'text', text: 'stale history' }] },
            }],
          };
        }
        return originalFetch(url);
      };

      const oldLoad = sv.loadEvents(staleId);
      sv.handleSourceChange(staleId, { replaying: true });
      sv.appendEvent(staleId, {
        type: 'user',
        uuid: 'fresh-event',
        timestamp: new Date().toISOString(),
        message: { role: 'user', content_blocks: [{ type: 'text', text: 'fresh resumed history' }] },
      });
      releaseStale();
      await oldLoad;
      window.fetch = originalFetch;

      const rendered = view.messagesEl.textContent;
      const staleRendered = staleView.messagesEl.textContent;
      sv.dispose(sessionId);
      sv.dispose(staleId);

      return {
        ok: true,
        reset,
        eventCount: view.events.length,
        rendered,
        inputTokens: view.totalTokens.input,
        outputTokens: view.totalTokens.output,
        staleRendered,
      };
    });

    assert.equal(result.ok, true, result.error || 'structured resume smoke failed');
    assert.deepEqual(result.reset, {
      eventCount: 0,
      uuidCount: 0,
      inputTokens: 0,
      placeholder: 'Loading resumed conversation...',
      loaded: true,
    });
    assert.equal(result.eventCount, 2);
    assert.match(result.rendered, /mensagem do histórico retomado/);
    assert.match(result.rendered, /resposta após o resume/);
    assert.equal(result.inputTokens, 7);
    assert.equal(result.outputTokens, 5);
    assert.match(result.staleRendered, /fresh resumed history/);
    assert.doesNotMatch(result.staleRendered, /stale history/);
    assert.deepEqual(consoleErrors, [], `browser console errors:\n${consoleErrors.join('\n')}`);

    await page.screenshot({ path: '/tmp/openpoet-structured-resume-smoke.png', fullPage: true });
    console.log('Structured view Claude /resume smoke passed');
  } finally {
    await browser.close();
  }
}

main().catch(err => {
  console.error(err);
  process.exit(1);
});
