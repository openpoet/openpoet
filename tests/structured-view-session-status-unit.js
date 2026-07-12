#!/usr/bin/env node

const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

class FakeClassList {
  constructor() {
    this.values = new Set();
  }

  add(value) {
    this.values.add(value);
  }

  remove(value) {
    this.values.delete(value);
  }

  contains(value) {
    return this.values.has(value);
  }
}

const stateEl = {
  id: 'mobile-input-state',
  innerHTML: '',
  dataset: {},
  classList: new FakeClassList(),
};
const mobileInput = { placeholder: '' };
const document = {
  createElement() {
    let value = '';
    return {
      get innerHTML() {
        return value.replace(/[&<>"']/g, ch => ({
          '&': '&amp;',
          '<': '&lt;',
          '>': '&gt;',
          '"': '&quot;',
          "'": '&#39;',
        })[ch]);
      },
      set textContent(text) {
        value = String(text ?? '');
      },
    };
  },
  getElementById(id) {
    if (id === 'mobile-input-state') return stateEl;
    if (id === 'mobile-terminal-input') return mobileInput;
    return null;
  },
};

const context = {
  clearInterval,
  clearTimeout,
  console,
  document,
  innerWidth: 375,
  setInterval,
  setTimeout,
};
context.window = context;
context.globalThis = context;

const source = fs.readFileSync('web/static/js/structured-view.js', 'utf8');
vm.runInNewContext(`${source}\nwindow.StructuredViewManagerForTest = StructuredViewManager;`, context);

const StructuredViewManager = context.StructuredViewManagerForTest;
const sv = new StructuredViewManager();

function view(sessionId) {
  return {
    sessionId,
    codexMode: false,
    statusLabel: null,
    inputArea: null,
    textarea: null,
    events: [],
    uuidTypeMap: new Map(),
    messageIdMap: new Map(),
    tokenAccountedIds: new Set(),
    messagesEl: { appendChild() {} },
    currentProgressWidget: null,
    totalTokens: { input: 0, output: 0, cache_read: 0, cache_create: 0 },
  };
}

function visible(label) {
  return stateEl.innerHTML.includes(`Claude is ${label}`);
}

const first = view('first');
const second = view('second');

sv.activeSessionId = first.sessionId;
sv._updateInputState(first, { type: 'user', message: { role: 'user' } });
assert.equal(visible('thinking'), true, 'live active-session event should show thinking');

sv.activeSessionId = second.sessionId;
sv._syncStatusIndicator(second);
assert.equal(stateEl.innerHTML, '', 'switching to an idle session should clear shared status');

sv._updateInputState(first, { type: 'progress', message: null });
assert.equal(first.statusLabel, 'working');
assert.equal(stateEl.innerHTML, '', 'background session event must not change shared status');

sv._appendEventToDOM(second, {
  type: 'historical',
  message: { role: 'user' },
}, { updateStatus: false });
assert.equal(second.statusLabel, null, 'historical replay must not set activity');
assert.equal(stateEl.innerHTML, '');

sv._updateInputState(second, { type: 'user', message: { role: 'user' } });
assert.equal(visible('thinking'), true, 'live selected-session event should show thinking');

sv._updateInputState(second, {
  type: 'assistant',
  message: { role: 'assistant', stop_reason: 'end_turn' },
});
assert.equal(stateEl.innerHTML, '', 'end_turn should clear status');
assert.equal(Object.hasOwn(stateEl.dataset, 'label'), false, 'idle should clear stale label metadata');

sv._updateInputState(second, { type: 'user', message: { role: 'user' } });
assert.equal(visible('thinking'), true, 'same label should render again after idle');

sv.activeSessionId = first.sessionId;
sv._syncStatusIndicator(first);
assert.equal(visible('working'), true, 'switching back should restore only that session state');

sv._updateInputState(second, {
  type: 'assistant',
  message: { role: 'assistant', stop_reason: 'end_turn' },
});
assert.equal(visible('working'), true, 'background idle event must not clear active session status');

clearTimeout(first._statusIdleTimer);
clearTimeout(second._statusIdleTimer);
console.log('Structured view session status unit passed');
