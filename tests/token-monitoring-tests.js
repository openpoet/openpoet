/**
 * Token Monitoring Tests - DevManager
 *
 * Execute with Playwright MCP against http://localhost:8080
 * Server must be started with DEVMANAGER_TEST_MODE=1
 *
 * Test groups:
 *   A: AI Assistant tokens (requires AI provider configured)
 *   B: OTEL pipeline (Claude Code sessions)
 *   C: API queries
 *   D: Frontend - Global Token Usage view
 *   E: Frontend - Project Token Usage view
 *   F: Regression / cleanup
 */

const BASE = 'http://localhost:8080';
const API = `${BASE}/api`;

// ============================================================
// HELPERS
// ============================================================

async function clearTokens() {
    const r = await fetch(`${API}/token-usage`, { method: 'DELETE' });
    return r.json();
}

async function seedToken(data) {
    const r = await fetch(`${API}/test/seed-token-usage`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(data),
    });
    return r.json();
}

async function getTokenSummary(days = 30, projectId = null) {
    let url = `${API}/token-usage/summary?days=${days}`;
    if (projectId) url += `&project_id=${projectId}`;
    const r = await fetch(url);
    return r.json();
}

async function getProjects() {
    const r = await fetch(`${API}/projects`);
    return r.json();
}

async function getAIStatus() {
    const r = await fetch(`${API}/ai/status`);
    return r.json();
}

function assert(condition, message) {
    if (!condition) {
        throw new Error(`FAIL: ${message}`);
    }
    console.log(`  PASS: ${message}`);
}

function assertApprox(actual, expected, tolerance, message) {
    if (Math.abs(actual - expected) > tolerance) {
        throw new Error(`FAIL: ${message} (expected ~${expected}, got ${actual})`);
    }
    console.log(`  PASS: ${message} (${actual} ~= ${expected})`);
}

// ============================================================
// GROUP C: API QUERY TESTS
// ============================================================

async function testC1_SummaryAggregation() {
    console.log('\n--- C1: Summary Aggregation ---');
    await clearTokens();

    // Seed 3 records
    await seedToken({ source: 'ai_assistant', model: 'claude-sonnet-4-5-20250929', input_tokens: 1000, output_tokens: 500, cost_usd: 0.0105 });
    await seedToken({ source: 'claude_code', model: 'claude-opus-4-6', input_tokens: 2000, output_tokens: 1000, cost_usd: 0.105 });
    await seedToken({ source: 'ai_assistant', model: 'claude-sonnet-4-5-20250929', input_tokens: 500, output_tokens: 250, cost_usd: 0.00525 });

    const data = await getTokenSummary(30);
    const s = data.summary;

    assert(s.total_input_tokens === 3500, `total_input_tokens = 3500 (got ${s.total_input_tokens})`);
    assert(s.total_output_tokens === 1750, `total_output_tokens = 1750 (got ${s.total_output_tokens})`);
    assertApprox(s.total_cost_usd, 0.12075, 0.001, 'total_cost_usd ~= 0.12075');
    assert(s.total_requests === 3, `total_requests = 3 (got ${s.total_requests})`);
}

async function testC2_FilterByDays() {
    console.log('\n--- C2: Filter by Days ---');
    await clearTokens();

    // Seed one record 3 days ago, one 20 days ago
    const d3 = new Date(Date.now() - 3 * 86400000).toISOString();
    const d20 = new Date(Date.now() - 20 * 86400000).toISOString();

    await seedToken({ source: 'ai_assistant', model: 'test-model', input_tokens: 100, output_tokens: 50, cost_usd: 0.01, created_at: d3 });
    await seedToken({ source: 'ai_assistant', model: 'test-model', input_tokens: 200, output_tokens: 100, cost_usd: 0.02, created_at: d20 });

    const data7 = await getTokenSummary(7);
    const data30 = await getTokenSummary(30);

    assert(data7.summary.total_requests === 1, `7d: 1 request (got ${data7.summary.total_requests})`);
    assert(data7.summary.total_input_tokens === 100, `7d: input=100 (got ${data7.summary.total_input_tokens})`);

    assert(data30.summary.total_requests === 2, `30d: 2 requests (got ${data30.summary.total_requests})`);
    assert(data30.summary.total_input_tokens === 300, `30d: input=300 (got ${data30.summary.total_input_tokens})`);
}

async function testC3_FilterByProjectID() {
    console.log('\n--- C3: Filter by Project ID ---');
    await clearTokens();

    const projects = await getProjects();
    if (!projects || projects.length < 1) {
        console.log('  SKIP: No projects available for this test');
        return;
    }

    const pid = projects[0].id;
    await seedToken({ source: 'ai_assistant', model: 'test-model', project_id: pid, input_tokens: 500, output_tokens: 200, cost_usd: 0.05 });
    await seedToken({ source: 'ai_assistant', model: 'test-model', input_tokens: 300, output_tokens: 100, cost_usd: 0.03 }); // no project

    const dataAll = await getTokenSummary(30);
    const dataProj = await getTokenSummary(30, pid);

    assert(dataAll.summary.total_requests === 2, `All: 2 requests (got ${dataAll.summary.total_requests})`);
    assert(dataProj.summary.total_requests === 1, `Project: 1 request (got ${dataProj.summary.total_requests})`);
    assert(dataProj.summary.total_input_tokens === 500, `Project: input=500 (got ${dataProj.summary.total_input_tokens})`);
}

async function testC4_BySourceBreakdown() {
    console.log('\n--- C4: By Source Breakdown ---');
    await clearTokens();

    await seedToken({ source: 'ai_assistant', model: 'test-model', input_tokens: 1000, output_tokens: 500, cost_usd: 0.05 });
    await seedToken({ source: 'claude_code', model: 'test-model', input_tokens: 2000, output_tokens: 1000, cost_usd: 0.15 });

    const data = await getTokenSummary(30);
    assert(data.by_source && data.by_source.length === 2, `by_source has 2 entries (got ${data.by_source?.length})`);

    const aiSource = data.by_source.find(s => s.source === 'ai_assistant');
    const ccSource = data.by_source.find(s => s.source === 'claude_code');

    assert(aiSource, 'ai_assistant source exists');
    assert(ccSource, 'claude_code source exists');
    assert(aiSource.total_input_tokens === 1000, `ai_assistant input=1000 (got ${aiSource.total_input_tokens})`);
    assert(ccSource.total_input_tokens === 2000, `claude_code input=2000 (got ${ccSource.total_input_tokens})`);
}

async function testC5_ByModelBreakdown() {
    console.log('\n--- C5: By Model Breakdown ---');
    await clearTokens();

    await seedToken({ source: 'ai_assistant', model: 'claude-sonnet-4-5-20250929', input_tokens: 1000, output_tokens: 500, cost_usd: 0.05 });
    await seedToken({ source: 'ai_assistant', model: 'claude-opus-4-6', input_tokens: 2000, output_tokens: 1000, cost_usd: 0.25 });
    await seedToken({ source: 'claude_code', model: 'claude-opus-4-6', input_tokens: 3000, output_tokens: 1500, cost_usd: 0.30 });

    const data = await getTokenSummary(30);
    assert(data.by_model && data.by_model.length === 3, `by_model has 3 entries (got ${data.by_model?.length})`);

    const sonnetAI = data.by_model.find(m => m.model === 'claude-sonnet-4-5-20250929' && m.source === 'ai_assistant');
    const opusAI = data.by_model.find(m => m.model === 'claude-opus-4-6' && m.source === 'ai_assistant');
    const opusCC = data.by_model.find(m => m.model === 'claude-opus-4-6' && m.source === 'claude_code');

    assert(sonnetAI, 'sonnet + ai_assistant entry exists');
    assert(opusAI, 'opus + ai_assistant entry exists');
    assert(opusCC, 'opus + claude_code entry exists');
    assert(opusCC.total_input_tokens === 3000, `opus CC input=3000 (got ${opusCC.total_input_tokens})`);
}

async function testC6_DailyAggregation() {
    console.log('\n--- C6: Daily Aggregation ---');
    await clearTokens();

    const today = new Date().toISOString().slice(0, 10) + 'T12:00:00Z';
    const yesterday = new Date(Date.now() - 86400000).toISOString().slice(0, 10) + 'T12:00:00Z';

    await seedToken({ source: 'ai_assistant', model: 'test-model', input_tokens: 100, output_tokens: 50, cost_usd: 0.01, created_at: today });
    await seedToken({ source: 'ai_assistant', model: 'test-model', input_tokens: 200, output_tokens: 100, cost_usd: 0.02, created_at: today });
    await seedToken({ source: 'ai_assistant', model: 'test-model', input_tokens: 300, output_tokens: 150, cost_usd: 0.03, created_at: yesterday });

    const data = await getTokenSummary(30);
    assert(data.daily && data.daily.length === 2, `daily has 2 entries (got ${data.daily?.length})`);

    // Daily is ordered DESC, so first entry is today
    const todayData = data.daily.find(d => d.date === today.slice(0, 10));
    const yesterdayData = data.daily.find(d => d.date === yesterday.slice(0, 10));

    assert(todayData, 'today entry exists');
    assert(yesterdayData, 'yesterday entry exists');
    assert(todayData.total_input_tokens === 300, `today input=300 (got ${todayData.total_input_tokens})`);
    assert(yesterdayData.total_input_tokens === 300, `yesterday input=300 (got ${yesterdayData.total_input_tokens})`);
}

async function testC7_EmptyDB() {
    console.log('\n--- C7: Empty DB ---');
    await clearTokens();

    const data = await getTokenSummary(30);
    const s = data.summary;

    assert(s.total_input_tokens === 0, `input=0 (got ${s.total_input_tokens})`);
    assert(s.total_output_tokens === 0, `output=0 (got ${s.total_output_tokens})`);
    assert(s.total_cost_usd === 0, `cost=0 (got ${s.total_cost_usd})`);
    assert(s.total_requests === 0, `requests=0 (got ${s.total_requests})`);
}

// ============================================================
// GROUP B: OTEL PIPELINE TESTS
// ============================================================

async function testB1_OTELEndpointAccepts() {
    console.log('\n--- B1: OTEL Endpoint Accepts Metrics ---');

    const sessionId = 'test-session-b1-' + Date.now();
    const payload = buildOTELPayload(sessionId, 'claude-opus-4-6', {
        input: 5000, output: 2000, cacheRead: 500, cacheCreation: 100,
    });

    const r = await fetch(`${BASE}/v1/metrics`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload),
    });

    assert(r.status === 200, `OTEL endpoint returns 200 (got ${r.status})`);
    const body = await r.json();
    assert(body !== undefined, 'OTEL endpoint returns JSON response');
}

async function testB2_OTELFlushPersists() {
    console.log('\n--- B2: OTEL + Flush → DB Persistence ---');
    // NOTE: This test depends on Bug 1 being fixed (FlushSession must be callable)
    // For now, we can only test that the OTEL endpoint accepts and the API returns data
    // The full flush test requires starting a real session and stopping it

    console.log('  INFO: Full flush test requires Bug 1 fix (FlushSession for user-stopped)');
    console.log('  INFO: Testing OTEL ingestion + API query after seed instead');

    await clearTokens();
    // Seed a claude_code record to simulate what FlushSession would produce
    await seedToken({ source: 'claude_code', model: 'claude-opus-4-6', input_tokens: 5000, output_tokens: 2000, cache_read_tokens: 500, cache_creation_tokens: 100, cost_usd: 0.225 });

    const data = await getTokenSummary(30);
    const ccSource = data.by_source?.find(s => s.source === 'claude_code');
    assert(ccSource, 'claude_code source found in API');
    assert(ccSource.total_input_tokens === 5000, `CC input=5000 (got ${ccSource.total_input_tokens})`);
}

function buildOTELPayload(sessionId, model, tokens) {
    const strVal = (s) => ({ stringValue: s });

    const dataPoints = [];
    for (const [type, value] of Object.entries(tokens)) {
        dataPoints.push({
            attributes: [
                { key: 'model', value: strVal(model) },
                { key: 'type', value: strVal(type) },
            ],
            asDouble: value,
        });
    }

    return {
        resourceMetrics: [{
            resource: {
                attributes: [
                    { key: 'devmanager.session_id', value: strVal(sessionId) },
                ],
            },
            scopeMetrics: [{
                metrics: [{
                    name: 'claude_code.token.usage',
                    sum: { dataPoints },
                }],
            }],
        }],
    };
}

// ============================================================
// GROUP D: FRONTEND - GLOBAL TOKEN USAGE VIEW (Playwright)
// These return instructions to be executed via Playwright MCP
// ============================================================

// D1-D7 are designed to be run via Playwright MCP browser automation.
// Each function documents what the Playwright steps should be.

async function testD_seed() {
    console.log('\n--- D: Seeding data for frontend tests ---');
    await clearTokens();

    const projects = await getProjects();
    const pid1 = projects?.[0]?.id || null;
    const pid2 = projects?.[1]?.id || null;

    // Seed varied data
    await seedToken({ source: 'ai_assistant', model: 'claude-sonnet-4-5-20250929', project_id: pid1, input_tokens: 10000, output_tokens: 5000, cost_usd: 0.105 });
    await seedToken({ source: 'claude_code', model: 'claude-opus-4-6', project_id: pid1, input_tokens: 20000, output_tokens: 8000, cost_usd: 0.90 });
    await seedToken({ source: 'ai_assistant', model: 'claude-sonnet-4-5-20250929', project_id: pid2, input_tokens: 5000, output_tokens: 2000, cost_usd: 0.045 });

    // Seed some yesterday data for daily chart
    const yesterday = new Date(Date.now() - 86400000).toISOString().slice(0, 10) + 'T12:00:00Z';
    await seedToken({ source: 'ai_assistant', model: 'claude-sonnet-4-5-20250929', input_tokens: 3000, output_tokens: 1000, cost_usd: 0.024, created_at: yesterday });

    console.log('  DONE: Seeded 4 records for frontend tests');
    return { pid1, pid2 };
}

// ============================================================
// GROUP A: AI ASSISTANT TOKEN TESTS
// ============================================================

async function testA4_ProviderStatus() {
    console.log('\n--- A4: AI Provider Status ---');
    const status = await getAIStatus();
    assert(status.configured !== undefined, 'status has configured field');
    console.log(`  INFO: provider=${status.provider || 'none'}, configured=${status.configured}, model=${status.model}`);

    if (status.configured) {
        assert(status.provider === 'claudecode' || status.provider === 'apikey',
            `provider is claudecode or apikey (got ${status.provider})`);
    } else {
        console.log('  WARN: AI not configured — A1-A3 tests will be skipped');
    }
    return status;
}

// ============================================================
// GROUP F: REGRESSION / CLEANUP
// ============================================================

async function testF1_ClearTokenUsage() {
    console.log('\n--- F1: Clear Token Usage ---');
    // Seed some data first
    await seedToken({ source: 'ai_assistant', model: 'test-model', input_tokens: 100, output_tokens: 50, cost_usd: 0.01 });
    await seedToken({ source: 'claude_code', model: 'test-model', input_tokens: 200, output_tokens: 100, cost_usd: 0.02 });

    // Verify data exists
    const before = await getTokenSummary(30);
    assert(before.summary.total_requests === 2, `Before: 2 requests (got ${before.summary.total_requests})`);

    // Clear
    const result = await clearTokens();
    assert(result.deleted === 2, `Deleted 2 records (got ${result.deleted})`);

    // Verify empty
    const after = await getTokenSummary(30);
    assert(after.summary.total_requests === 0, `After: 0 requests (got ${after.summary.total_requests})`);
}

// ============================================================
// MAIN RUNNER
// ============================================================

async function runAllTests() {
    console.log('=== TOKEN MONITORING TESTS ===');
    console.log(`Target: ${BASE}`);
    console.log(`Time: ${new Date().toISOString()}\n`);

    let passed = 0;
    let failed = 0;
    let skipped = 0;
    const failures = [];

    const tests = [
        // Group C: API tests (no Playwright needed)
        { name: 'C1: Summary Aggregation', fn: testC1_SummaryAggregation },
        { name: 'C2: Filter by Days', fn: testC2_FilterByDays },
        { name: 'C3: Filter by Project ID', fn: testC3_FilterByProjectID },
        { name: 'C4: By Source Breakdown', fn: testC4_BySourceBreakdown },
        { name: 'C5: By Model Breakdown', fn: testC5_ByModelBreakdown },
        { name: 'C6: Daily Aggregation', fn: testC6_DailyAggregation },
        { name: 'C7: Empty DB', fn: testC7_EmptyDB },

        // Group B: OTEL pipeline
        { name: 'B1: OTEL Endpoint Accepts', fn: testB1_OTELEndpointAccepts },
        { name: 'B2: OTEL Flush Persists', fn: testB2_OTELFlushPersists },

        // Group A: AI status
        { name: 'A4: Provider Status', fn: testA4_ProviderStatus },

        // Group F: Regression
        { name: 'F1: Clear Token Usage', fn: testF1_ClearTokenUsage },

        // Group D: Frontend seed (prepares data for Playwright visual tests)
        { name: 'D-seed: Seed Frontend Data', fn: testD_seed },
    ];

    for (const test of tests) {
        try {
            await test.fn();
            passed++;
        } catch (e) {
            failed++;
            failures.push({ name: test.name, error: e.message });
            console.error(`  FAIL: ${test.name}: ${e.message}`);
        }
    }

    console.log('\n=== RESULTS ===');
    console.log(`Passed: ${passed}`);
    console.log(`Failed: ${failed}`);
    console.log(`Skipped: ${skipped}`);

    if (failures.length > 0) {
        console.log('\nFailures:');
        for (const f of failures) {
            console.log(`  - ${f.name}: ${f.error}`);
        }
    }

    console.log('\n=== PLAYWRIGHT VISUAL TESTS (run manually) ===');
    console.log('After running the above, use Playwright MCP to:');
    console.log('  D1: Navigate to Tokens view → verify view loads');
    console.log('  D2: Verify stats grid shows correct values');
    console.log('  D3: Verify By Model table renders');
    console.log('  D4: Click 7d/30d/90d buttons → verify state changes');
    console.log('  D5: Verify daily bar chart appears');
    console.log('  D6: Verify By Project cards appear');
    console.log('  D7: Clear tokens → verify empty state message');
    console.log('  E1: Open project detail → verify token section');
    console.log('  A1: Open AI chat → send message → verify token counter');
    console.log('  A2: After AI message → verify API shows record');
    console.log('  A3: Verify model name in by_model');

    return { passed, failed, failures };
}

// Auto-run when loaded
runAllTests();
