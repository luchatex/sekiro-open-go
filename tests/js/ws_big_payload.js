// sekiro-samples demo-web（sekiroWeb.js）的 Node 本地化驱动：
// - window shim 适配官方 resolveWebSocketFactory 的 window 分支（零改动官方代码）
// - provider 注册 action=bigChinese，响应 3MB 中文字符串（1,048,576 汉字）
// - 主循环反复 HTTP invoke，校验响应：无 U+FFFD、长度与 SHA256 一致
//
// 运行：node ws_big_payload.js [port]

const fs = require('fs');
const vm = require('vm');
const crypto = require('crypto');
const http = require('http');

const PORT = process.argv[2] || '5620';
const GROUP = 'ws-big';
const CLIENT_ID = 'node-web-1';
const CHARS = 1048576; // 1M 汉字 ≈ 3MB UTF-8 字节

// ---- window shim（Node 22 原生 WebSocket）----
globalThis.window = { WebSocket: WebSocket };

// ---- 加载官方 sekiroWeb.js（保持原文件零改动）----
const code = fs.readFileSync('G:/tmp/sekiro-samples/demo-web/sekiroWeb.js', 'utf8');
vm.runInThisContext(code);

// ---- 3MB 中文载荷 ----
const unit = '中文字符串测试'; // 7 汉字
let big = '';
while (big.length < CHARS) big += unit;
big = big.slice(0, CHARS);
const bigBuf = Buffer.from(big, 'utf8');
const expectHash = crypto.createHash('sha256').update(bigBuf).digest('hex');
console.log(`payload: ${big.length} chars, ${bigBuf.length} utf8 bytes, sha256=${expectHash.slice(0, 16)}...`);

// ---- 启动官方 SekiroClient（WS provider）----
const client = new SekiroClient(
    `ws://127.0.0.1:${PORT}/business/register?group=${GROUP}&clientId=${CLIENT_ID}`);

client.registerAction('bigChinese', function (request, resolve, reject) {
    resolve(big);
});
client.registerAction('ping', function (request, resolve, reject) {
    resolve('pong');
});

// ---- invoke + 校验 ----
function invoke(action, timeoutMs) {
    return new Promise((resolvePromise, rejectPromise) => {
        const t0 = Date.now();
        const req = http.get(
            `http://127.0.0.1:${PORT}/business/invoke?group=${GROUP}&action=${action}`,
            { timeout: timeoutMs },
            (res) => {
                const chunks = [];
                res.on('data', (c) => chunks.push(c));
                res.on('end', () => resolvePromise({
                    status: res.statusCode,
                    body: Buffer.concat(chunks),
                    ms: Date.now() - t0,
                }));
            });
        req.on('error', rejectPromise);
        req.on('timeout', () => { req.destroy(new Error('client timeout')); });
    });
}

function checkResponse(r, n) {
    const text = r.body.toString('utf8');
    const bad = (text.match(/\uFFFD/g) || []).length;
    let parsed;
    try {
        parsed = JSON.parse(text);
    } catch (e) {
        console.log(`[run ${n}] FAIL: http=${r.status} ${r.ms}ms body=${r.body.length}B JSON parse error: ${e.message}`);
        return false;
    }
    const data = parsed.data == null ? '' : String(parsed.data);
    const dataBuf = Buffer.from(data, 'utf8');
    const hash = crypto.createHash('sha256').update(dataBuf).digest('hex');
    const okHash = hash === expectHash;
    const okStatus = parsed.status === 0 && parsed.clientId === CLIENT_ID;
    console.log(`[run ${n}] http=${r.status} ${r.ms}ms body=${r.body.length}B data=${data.length} chars ` +
        `(${dataBuf.length}B) U+FFFD=${bad} sha256=${hash.slice(0, 16)}... ` +
        `status=${parsed.status} => ${okHash && okStatus && bad === 0 ? 'PASS' : 'FAIL'}`);
    return okHash && okStatus && bad === 0;
}

(async () => {
    // 等 WS 注册就绪
    for (let i = 0; i < 20; i++) {
        await new Promise((r) => setTimeout(r, 500));
        try {
            const r = await invoke('ping', 5000);
            if (r.body.toString('utf8').includes('pong')) break;
        } catch (e) { /* retry */ }
    }
    let allPass = true;
    const RUNS = 10;
    for (let n = 1; n <= RUNS; n++) {
        try {
            const r = await invoke('bigChinese', 60000);
            if (!checkResponse(r, n)) allPass = false;
        } catch (e) {
            console.log(`[run ${n}] FAIL: ${e.message}`);
            allPass = false;
        }
        await new Promise((r) => setTimeout(r, 300));
    }
    console.log(allPass ? `ALL ${RUNS} RUNS PASS` : 'HAS FAILURES');
    process.exit(allPass ? 0 : 1);
})();
