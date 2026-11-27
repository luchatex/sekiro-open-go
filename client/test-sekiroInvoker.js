// sekiroInvoker 本地验证：node client/test-sekiroInvoker.js
const assert = require('assert');
const SekiroInvoker = require('./sekiroInvoker.js');

function response(body, status) {
  return { ok: (status || 200) < 400, status: status || 200, json: async () => body };
}

async function testParamsAndIdempotency() {
  const calls = [];
  const fetchImpl = async (url, init) => {
    calls.push({ url, init });
    return response({ status: 0, data: 'ok' });
  };
  const inv = new SekiroInvoker('http://127.0.0.1:5612', {
    fetch: fetchImpl, serverTimeoutMs: 100000, timeoutMs: 1000, retryBaseMs: 1
  });
  const r1 = await inv.invoke('g', 'a', { pageNum: 2 }, { bindClient: 'c1' });
  const r2 = await inv.invoke('g', 'a', { pageNum: 2 }, { bindClient: 'c1' });
  assert.strictEqual(r1.data, 'ok');
  assert.strictEqual(r2.data, 'ok');
  assert.strictEqual(calls.length, 1, '幂等命中应只发一次请求');
  const q = new URL(calls[0].url).searchParams;
  assert.strictEqual(q.get('group'), 'g');
  assert.strictEqual(q.get('action'), 'a');
  assert.strictEqual(q.get('bind_client'), 'c1');
  assert.strictEqual(q.get('invoke_timeout'), '100000');
  assert.strictEqual(q.get('pageNum'), '2');
  assert.ok(q.get('__invoke_trace_id'));
  console.log('ok: 参数与幂等');
}

async function testRetryOnBusy() {
  let n = 0;
  const fetchImpl = async () => {
    n++;
    return n === 1 ? response({ status: -1, message: 'busy, retry with backoff' })
                   : response({ status: 0, data: 'done' });
  };
  const inv = new SekiroInvoker('http://x', { fetch: fetchImpl, retryBaseMs: 1, timeoutMs: 500 });
  const r = await inv.invoke('g', 'a', {}, { bindClient: 'c2' });
  assert.strictEqual(n, 2);
  assert.strictEqual(r.data, 'done');
  console.log('ok: busy 退避重试');
}

async function testPerClientConcurrency() {
  let active = 0, maxActive = 0;
  const fetchImpl = async () => {
    active++;
    maxActive = Math.max(maxActive, active);
    await new Promise((r) => setTimeout(r, 30));
    active--;
    return response({ status: 0, data: 'ok' });
  };
  const inv = new SekiroInvoker('http://x', { fetch: fetchImpl, retryBaseMs: 1, timeoutMs: 500 });
  await Promise.all([
    inv.invoke('g', 'a', { i: 1 }, { bindClient: 'c3' }),
    inv.invoke('g', 'a', { i: 2 }, { bindClient: 'c3' })
  ]);
  assert.strictEqual(maxActive, 1, '同一 clientId 应串行');
  console.log('ok: 每 clientId 并发 1');
}

async function testCircuitBreaker() {
  let n = 0;
  const fetchImpl = async () => {
    n++;
    return response({ status: -1, message: 'timeout' });
  };
  const inv = new SekiroInvoker('http://x', {
    fetch: fetchImpl, maxRetries: 0, retryBaseMs: 1, timeoutMs: 500,
    breakerFailures: 2, breakerCooldownMs: 10000
  });
  for (let i = 0; i < 2; i++) {
    await assert.rejects(inv.invoke('g', 'a', { i }, { bindClient: 'c4' }), /timeout/);
  }
  assert.strictEqual(n, 2);
  await assert.rejects(inv.invoke('g', 'a', { i: 3 }, { bindClient: 'c4' }), /circuit open/);
  assert.strictEqual(n, 2, '熔断后不应再发请求');
  console.log('ok: 熔断');
}

(async () => {
  await testParamsAndIdempotency();
  await testRetryOnBusy();
  await testPerClientConcurrency();
  await testCircuitBreaker();
  console.log('all invoker tests passed');
})().catch((e) => {
  console.error(e);
  process.exit(1);
});
