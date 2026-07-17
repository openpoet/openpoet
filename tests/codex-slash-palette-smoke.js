#!/usr/bin/env node

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
    await page.goto(baseURL, { waitUntil: 'networkidle' });

    const desktop = await page.evaluate(async () => {
      const tm = window.terminalManager;
      if (!tm) return { ok: false, error: 'terminalManager missing' };

      const sessionId = 'codex-slash-e2e';
      const originalRequest = tm.requestCodexCommand.bind(tm);
      const calls = [];

      tm.requestCodexCommand = async (_sessionId, action, params = {}) => {
        calls.push({ action, params });
        if (action === 'model/list') {
          return {
            data: [{
              model: 'gpt-e2e',
              displayName: 'GPT E2E',
              description: 'E2E model',
              supportedReasoningEfforts: [{ reasoningEffort: 'low' }],
              serviceTiers: [{ id: 'standard', name: 'Standard' }]
            }]
          };
        }
        if (action === 'permissions/list') {
          return {
            current: { approvalPolicy: 'on-request', sandboxMode: 'workspace-write' },
            presets: [{ id: 'default', label: 'Default' }],
            profiles: { data: [{ id: 'custom-profile', description: 'Custom profile' }] },
            profileApplySupported: false
          };
        }
        if (action === 'resume/list') {
          return {
            data: [{
              id: 'thread-e2e',
              name: 'Thread E2E',
              preview: 'Preview',
              status: 'idle',
              updatedAt: 1780000000
            }]
          };
        }
        return {};
      };

      tm.terminals.set(sessionId, {
        backend: 'codex',
        codexRuntime: 'app-server',
        terminal: { focus() {} },
        ws: { readyState: WebSocket.OPEN, send() {} }
      });
      tm.activeSessionId = sessionId;
      tm._knownInput.set(sessionId, '');

      const opened = tm.codexSlashPalette.handleData(sessionId, '/');
      const rootLabels = [...document.querySelectorAll('.codex-slash-label')].map(el => el.textContent);

      await tm.codexSlashPalette.openCommand('help');
      const helpRequested = calls.some(call => call.action === 'help/read');
      tm.codexSlashPalette.openRoot(sessionId);

      await tm.codexSlashPalette.openCommand('model');
      const modelLabels = [...document.querySelectorAll('.codex-slash-label')].map(el => el.textContent);

      await tm.codexSlashPalette.openCommand('permissions');
      const permissionLabels = [...document.querySelectorAll('.codex-slash-label')].map(el => el.textContent);
      const disabledProfiles = [...document.querySelectorAll('.codex-slash-item:disabled .codex-slash-label')].map(el => el.textContent);

      await tm.codexSlashPalette.openCommand('resume');
      const resumeLabels = [...document.querySelectorAll('.codex-slash-label')].map(el => el.textContent);

      tm.codexSlashPalette.close();
      tm.terminals.delete(sessionId);
      tm._knownInput.delete(sessionId);
      tm.requestCodexCommand = originalRequest;

      return { ok: true, opened, rootLabels, helpRequested, modelLabels, permissionLabels, disabledProfiles, resumeLabels, calls };
    });

    assert.equal(desktop.ok, true, desktop.error || 'desktop smoke failed');
    assert.equal(desktop.opened, true);
    for (const label of ['/help', '/status', '/resume', '/model', '/permissions', '/compact', '/new', '/stop']) {
      assert.ok(desktop.rootLabels.includes(label), `missing root command ${label}`);
    }
    assert.ok(!desktop.rootLabels.includes('/init'), 'unavailable /init command should not be displayed');
    assert.equal(desktop.helpRequested, true, '/help did not dispatch help/read');
    assert.ok(desktop.modelLabels.includes('GPT E2E'), 'model follow-up did not render');
    assert.ok(desktop.permissionLabels.includes('Default'), 'permissions preset did not render');
    assert.ok(desktop.permissionLabels.includes('custom-profile'), 'custom permission profile did not render');
    assert.ok(desktop.disabledProfiles.includes('custom-profile'), 'list-only custom profile should be disabled');
    assert.ok(desktop.resumeLabels.includes('Thread E2E'), 'resume follow-up did not render');

    await page.setViewportSize({ width: 375, height: 812 });
    const mobile = await page.evaluate(async () => {
      const tm = window.terminalManager;
      const input = document.getElementById('mobile-terminal-input');
      if (!tm || !input) return { ok: false, error: 'mobile controls missing' };

      const sessionId = 'codex-slash-mobile-e2e';
      tm.terminals.set(sessionId, {
        backend: 'codex',
        codexRuntime: 'app-server',
        terminal: { focus() {} },
        ws: { readyState: WebSocket.OPEN, send() {} }
      });
      tm.activeSessionId = sessionId;
      tm._knownInput.set(sessionId, '');

      input.value = '/';
      input.dispatchEvent(new Event('input', { bubbles: true }));
      await new Promise(resolve => setTimeout(resolve, 25));

      const opened = !!document.getElementById('codex-slash-palette');
      const sentToTerminal = tm._knownInput.get(sessionId);
      const labels = [...document.querySelectorAll('.codex-slash-label')].map(el => el.textContent);

      tm.codexSlashPalette.close();
      tm.terminals.delete(sessionId);
      tm._knownInput.delete(sessionId);

      return { ok: true, opened, sentToTerminal, labels, inputValue: input.value };
    });

    assert.equal(mobile.ok, true, mobile.error || 'mobile smoke failed');
    assert.equal(mobile.opened, true, 'mobile slash did not open palette');
    assert.equal(mobile.sentToTerminal, '', 'mobile slash leaked into terminal input tracker');
    assert.equal(mobile.inputValue, '', 'mobile slash input was not cleared after palette open');
    assert.ok(mobile.labels.includes('/resume'), 'mobile palette root labels missing');
    assert.deepEqual(consoleErrors, [], `browser console errors:\n${consoleErrors.join('\n')}`);

    console.log('Codex slash palette smoke passed');
  } finally {
    await browser.close();
  }
}

main().catch(err => {
  console.error(err);
  process.exit(1);
});
