#!/usr/bin/env node

const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

function loadOpenPoetClass() {
  const context = {
    Blob,
    URL,
    URLSearchParams,
    clearInterval,
    clearTimeout,
    console,
    document: {},
    fetch: async () => ({}),
    history: {},
    location: {},
    localStorage: {},
    navigator: {},
    setInterval,
    setTimeout,
  };
  context.window = context;
  context.globalThis = context;

  const source = fs.readFileSync('web/static/js/app.js', 'utf8')
    .replace(/\/\/ Initialize app[\s\S]*$/, 'window.OpenPoetForTest = OpenPoet;');
  vm.runInNewContext(source, context);
  return { OpenPoet: context.OpenPoetForTest, context };
}

const { OpenPoet, context } = loadOpenPoetClass();
const elements = new Map([
  ['filter-status', { value: 'in_progress' }],
  ['filter-priority', { value: 'high' }],
  ['filter-project', { value: '7' }],
  ['filter-search', { value: 'release notes' }],
]);
context.document = {
  getElementById(id) { return elements.get(id) || null; },
  querySelector(selector) {
    if (selector === '.tab-btn.active') return { dataset: { tab: 'agents' } };
    return null;
  },
};

const app = Object.create(OpenPoet.prototype);
Object.assign(app, {
  currentView: 'terminal',
  currentSession: 'session-abc',
  sessions: [{ id: 'session-abc', project_id: 7 }],
  _allTasksViewMode: 'flat',
  _skillSearch: '',
  _skillFilterCategory: '',
  _skillFilterStatus: '',
  _restoringURLState: false,
  _initialURLRestored: true,
});
context.app = app;
context.docViewer = {
  getURLState() { return { kind: 'plan', sessionId: 'session-abc' }; },
};

let current = new URL('http://localhost:8080/?tunnel=preserved');
Object.defineProperty(context, 'location', {
  configurable: true,
  get() { return current; },
});
context.history = {
  calls: [],
  pushState(_state, _title, next) {
    this.calls.push({ method: 'pushState', next });
    current = new URL(next, current);
  },
  replaceState(_state, _title, next) {
    this.calls.push({ method: 'replaceState', next });
    current = new URL(next, current);
  },
};

const terminalState = app._currentURLState();
assert.deepEqual(JSON.parse(JSON.stringify(terminalState)), {
  view: 'terminal',
  project: '7',
  session: 'session-abc',
  doc: 'plan:session-abc',
});

const terminalURL = app._urlForState(terminalState);
assert.equal(
  terminalURL,
  '/?tunnel=preserved&view=terminal&project=7&session=session-abc&doc=plan%3Asession-abc',
  'canonical URL should preserve unrelated query parameters',
);

app._syncURLState();
assert.equal(context.history.calls.at(-1).method, 'pushState');
assert.equal(current.searchParams.get('session'), 'session-abc');
assert.equal(current.searchParams.get('doc'), 'plan:session-abc');
assert.equal(app._hasExplicitURLState(), true);

app._syncURLState();
assert.equal(context.history.calls.length, 1, 'identical state should not add browser history');

current = new URL('http://localhost:8080/?tunnel=preserved');
assert.equal(app._hasExplicitURLState(), false, 'unrelated query params must not override restored local tabs');

current = new URL('http://localhost:8080/?view=tasks&project=7&doc=task%3A7%3A42&task_status=todo&task_priority=medium&task_project=7&task_q=docs&task_mode=flat');
const parsed = app._readURLState();
assert.equal(parsed.view, 'tasks');
assert.equal(parsed.project, '7');
assert.equal(parsed.doc, 'task:7:42');
assert.equal(parsed.taskStatus, 'todo');
assert.equal(parsed.taskPriority, 'medium');
assert.equal(parsed.taskProject, '7');
assert.equal(parsed.taskSearch, 'docs');
assert.equal(parsed.taskMode, 'flat');

app.currentView = 'tasks';
context.docViewer = { getURLState: () => null };
const taskState = app._currentURLState();
assert.equal(taskState.taskStatus, 'in_progress');
assert.equal(taskState.taskPriority, 'high');
assert.equal(taskState.taskProject, '7');
assert.equal(taskState.taskSearch, 'release notes');
assert.equal(taskState.taskMode, 'flat');

app._restoringURLState = true;
app._syncURLState();
assert.equal(context.history.calls.length, 1, 'popstate restoration must not write history');

const template = fs.readFileSync('web/templates/index.html', 'utf8');
const viewerSource = fs.readFileSync('web/static/js/doc-viewer.js', 'utf8');
assert.match(template, /id="doc-review-download-pdf"[^>]+title="Download as PDF"/);
assert.match(viewerSource, /async _downloadPDF\(\)/);
assert.match(viewerSource, /printWindow\.print\(\)/);

class FakeClassList {
  constructor(...values) { this.values = new Set(values); }
  add(value) { this.values.add(value); }
  remove(value) { this.values.delete(value); }
  contains(value) { return this.values.has(value); }
}

function fakeElement(classes = []) {
  const listeners = new Map();
  return {
    classList: new FakeClassList(...classes),
    dataset: {},
    firstChild: null,
    innerHTML: '',
    textContent: '',
    addEventListener(type, listener) { listeners.set(type, listener); },
    appendChild() {},
    insertBefore() {},
    listeners,
  };
}

const viewerElements = {
  'doc-review-overlay': fakeElement(['hidden']),
  'doc-review-name': fakeElement(),
  'doc-review-content': fakeElement(),
  'doc-review-footer': fakeElement(['hidden']),
  'doc-review-close': fakeElement(),
  'doc-review-download': fakeElement(),
  'doc-review-download-pdf': fakeElement(),
};
const routeNotifications = [];
const viewerContext = {
  console,
  document: {
    addEventListener() {},
    createDocumentFragment() { return {}; },
    createElement() { return fakeElement(); },
    getElementById(id) { return viewerElements[id] || null; },
  },
  requestAnimationFrame(callback) { callback(); },
  setTimeout,
};
viewerContext.window = viewerContext;
viewerContext.globalThis = viewerContext;
viewerContext.app = {
  _onDocumentURLStateChanged(options) { routeNotifications.push(options); },
};
vm.runInNewContext(
  viewerSource.replace(/document\.addEventListener\('DOMContentLoaded'[\s\S]*$/, 'window.DocViewerForTest = DocViewer;'),
  viewerContext,
);
const viewer = new viewerContext.DocViewerForTest();
viewer.openWithContent('Task', 'task body', {
  urlState: { kind: 'task', projectId: 7, taskId: 42 },
});
assert.equal(viewer.getURLState().kind, 'task');
viewer.openWithContent('Document', 'document body', {
  urlState: { kind: 'document', id: 'doc-1' },
});
assert.equal(viewer.getURLState().id, 'doc-1');
assert.equal(viewer._history.length, 1);
viewer.close();
assert.equal(viewer.getURLState().kind, 'task', 'viewer close should restore prior URL state');
viewer.close();
assert.equal(viewer.getURLState(), null, 'closing the root document should clear URL state');
assert.equal(routeNotifications.length, 4);
assert.ok(viewerElements['doc-review-download-pdf'].listeners.has('click'));

console.log('URL state and document PDF unit passed');
