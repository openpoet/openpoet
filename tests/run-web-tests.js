#!/usr/bin/env node

// Runner for the JS side of the test suite. The Go tests are `make test`; this
// covers the browser and DOM tests under tests/.
//
// Three groups, because they need different things:
//   unit    — node + vm only, no browser
//   browser — Playwright, but the page content is built in-process (no server)
//   server  — Playwright against a running OpenPoet dev instance
//
// Usage:
//   node tests/run-web-tests.js                 # every group (server group is
//                                               # skipped when nothing listens)
//   node tests/run-web-tests.js --unit          # one group only
//   node tests/run-web-tests.js --strict        # a skipped group fails the run
//   OPENPOET_E2E_URL=http://127.0.0.1:8082 node tests/run-web-tests.js

const { spawnSync } = require('node:child_process');
const path = require('node:path');

const REPO_ROOT = path.resolve(__dirname, '..');

const GROUPS = {
  unit: [
    'tests/structured-view-session-status-unit.js',
    'tests/url-state-unit.js',
  ],
  browser: [
    'tests/structured-view-session-status-smoke.js',
    'tests/url-state-browser-smoke.js',
    'tests/voice-chunked-upload-smoke.js',
  ],
  server: [
    'tests/codex-slash-palette-smoke.js',
    'tests/codex-transcript-snapshot-smoke.js',
    'tests/structured-view-resume-smoke.js',
  ],
};

// tests/token-monitoring-tests.js is a scripted plan for a human or an agent to
// drive through the Playwright MCP server, not an executable test. It is listed
// here so nobody mistakes its absence for an oversight.
const NOT_EXECUTABLE = ['tests/token-monitoring-tests.js'];

const args = process.argv.slice(2);
const strict = args.includes('--strict');
const selected = Object.keys(GROUPS).filter(name => args.includes(`--${name}`));
const groups = selected.length > 0 ? selected : Object.keys(GROUPS);

const baseURL = process.env.OPENPOET_E2E_URL || 'http://localhost:8080';

// Port 8081 is production. Never point a test suite at it.
if (/:8081(\/|$)/.test(baseURL)) {
  console.error(`refusing to run against ${baseURL}: port 8081 is production`);
  process.exit(2);
}

async function serverIsUp(url) {
  try {
    const response = await fetch(url, {
      method: 'GET',
      signal: AbortSignal.timeout(3000),
    });
    return response.ok;
  } catch {
    return false;
  }
}

function runFile(file) {
  const started = Date.now();
  const result = spawnSync(process.execPath, [file], {
    cwd: REPO_ROOT, // the browser tests addScriptTag() repo-relative paths
    stdio: 'inherit',
    env: { ...process.env, OPENPOET_E2E_URL: baseURL },
  });
  return { ok: result.status === 0, ms: Date.now() - started };
}

async function main() {
  let failed = 0;
  let skipped = 0;

  for (const group of groups) {
    const files = GROUPS[group];

    if (group === 'server' && !(await serverIsUp(baseURL))) {
      console.log(`\n## ${group}: SKIPPED — nothing answering at ${baseURL}`);
      console.log('   start a dev instance first: ./.scripts/dev-server.sh start');
      console.log('   or point the suite elsewhere: OPENPOET_E2E_URL=http://127.0.0.1:8082');
      skipped += files.length;
      continue;
    }

    console.log(`\n## ${group}${group === 'server' ? ` (against ${baseURL})` : ''}`);
    for (const file of files) {
      const { ok, ms } = runFile(file);
      console.log(`${ok ? 'PASS' : 'FAIL'} ${file} (${ms}ms)`);
      if (!ok) failed++;
    }
  }

  console.log(`\n${failed === 0 ? 'OK' : 'FAILED'}: ${failed} failing, ${skipped} skipped`);
  if (skipped > 0) {
    console.log(`not executable here (Playwright MCP only): ${NOT_EXECUTABLE.join(', ')}`);
  }
  if (failed > 0 || (strict && skipped > 0)) {
    process.exit(1);
  }
}

main().catch(err => {
  console.error(err);
  process.exit(1);
});
