#!/usr/bin/env node

// The link paths the server hands out (docs/links.md) and the query-string
// state the app turns each of them into.

const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

function loadOpenPoetClass() {
  const context = {
    Blob, URL, URLSearchParams, clearInterval, clearTimeout, console,
    document: {}, fetch: async () => ({}), history: {}, location: {}, localStorage: {}, navigator: {},
    setInterval, setTimeout,
  };
  context.window = context;
  context.globalThis = context;
  const source = fs.readFileSync('web/static/js/app.js', 'utf8')
    .replace(/\/\/ Initialize app[\s\S]*$/, 'window.OpenPoetForTest = OpenPoet;');
  vm.runInNewContext(source, context);
  return { OpenPoet: context.OpenPoetForTest, context };
}

const { OpenPoet, context } = loadOpenPoetClass();
const route = (path, search) => {
  const result = OpenPoet.routeFromAppPath(path, search);
  return result ? { ...result } : null; // plain object for deepEqual across vm contexts
};

assert.deepEqual(route('/app/artifact/ab12cd34'), { doc: 'artifact:ab12cd34' });
assert.deepEqual(route('/app/artifact/ab12cd34/'), { doc: 'artifact:ab12cd34' });
assert.deepEqual(route('/app/artifact/ab12cd34', '?v=3'), { doc: 'artifact:ab12cd34', v: '3' });
assert.deepEqual(route('/app/artifact/ab12cd34', '?v=0'), { doc: 'artifact:ab12cd34' });
assert.deepEqual(route('/app/artifact/ab12cd34', '?v=abc'), { doc: 'artifact:ab12cd34' });
assert.deepEqual(route('/app/doc/cd34ab12'), { doc: 'cd34ab12' });
assert.deepEqual(route('/app/doc/plan%3Asess-1'), { doc: 'plan:sess-1' });
assert.deepEqual(route('/app/session/0f8fad5b-d9cb-469f-a165-70867728950e'), { view: 'terminal', session: '0f8fad5b-d9cb-469f-a165-70867728950e' });
assert.deepEqual(route('/app/project/4'), { view: 'project-detail', project: '4' });
assert.deepEqual(route('/app/task/4/12'), { view: 'tasks', doc: 'task:4:12' });

// Not link paths, or link paths that carry something that is not an id.
for (const path of [
  '/', '/app', '/app/', '/app/artifact', '/app/artifact/', '/artifact/ab12cd34', '/app/unknown/x',
  '/app/artifact/ab12cd34/extra', '/app/artifact/<script>', '/app/artifact/a%20b', '/app/artifact/%E0%A4%A',
  '/app/project/abc', '/app/task/4', '/app/task/4/abc', '/app/task/x/12', '/app/doc/a b',
]) {
  assert.equal(route(path), null, `routeFromAppPath(${JSON.stringify(path)}) should be null`);
}

// Adopting a link path rewrites the address into the canonical query form.
function adopt(pathname, search = '', hash = '') {
  const replaced = [];
  context.location = { pathname, search, hash };
  context.history = { replaceState: (_state, _title, url) => replaced.push(url) };
  const app = Object.create(OpenPoet.prototype);
  const adopted = app._adoptAppPathRoute();
  return { adopted, url: replaced[0] };
}
assert.deepEqual(adopt('/app/artifact/ab12cd34'), { adopted: true, url: '/?doc=artifact%3Aab12cd34' });
assert.deepEqual(adopt('/app/artifact/ab12cd34', '?v=2'), { adopted: true, url: '/?doc=artifact%3Aab12cd34&v=2' });
assert.deepEqual(adopt('/app/task/4/12'), { adopted: true, url: '/?view=tasks&doc=task%3A4%3A12' });
// An /app path that is not a link goes home instead of sticking in the address.
assert.deepEqual(adopt('/app/nothing/here'), { adopted: true, url: '/' });
// Any other address is left alone.
assert.deepEqual(adopt('/', '?view=tasks'), { adopted: false, url: undefined });
assert.deepEqual(adopt('/application'), { adopted: false, url: undefined });

console.log('app link routes unit: OK');
