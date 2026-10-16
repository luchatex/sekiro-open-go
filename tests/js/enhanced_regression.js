// 增强版 sekiroWeb-enhanced.js 的 Node 驱动：3MB 中文往返回归
// （对照 tools/web/ws_big_payload.js，仅换客户端实现）
const fs = require('fs');
const vm = require('vm');
const crypto = require('crypto');
const http = require('http');

const PORT = process.argv[2] || '5620';
const CHARS = 1048576; // 1M 汉字（JS 字符串按 UTF-16 code unit 切割，字符级安全）

globalThis.window = { WebSocket: WebSocket };
const path = require('path');
const code = fs.readFileSync(path.join(__dirname, 'sekiroWeb-enhanced.js'), 'utf8');
vm.runInThisContext(code);

const unit = '中文字符串测试';
let big = '';
while (big.length < CHARS) big += unit;
big = big.slice(0, CHARS);
const bigBuf = Buffer.from(big, 'utf8');
const expectHash = crypto.createHash('sha256').update(bigBuf).digest('hex');
console.log(`payload: ${big.length} chars, ${bigBuf.length} utf8 bytes`);

let reconnects = 0;
const client = new SekiroClient(
    `ws://127.0.0.1:${PORT}/business/register?group=ws-enh&clientId=node-enh-1`,
    { onReconnect: (delay) => { reconnects++; console.log(`reconnect scheduled: ${delay}ms`); } });

client.registerAction('bigChinese', function (request, resolve, reject) { resolve(big); });
client.registerAction('ping', function (request, resolve, reject) { resolve('pong'); });

function invoke(action, timeoutMs) {
    return new Promise((res, rej) => {
        const t0 = Date.now();
        const req = http.get(`http://127.0.0.1:${PORT}/business/invoke?group=ws-enh&action=${action}`,
            { timeout: timeoutMs }, (r) => {
                const cs = [];
                r.on('data', (c) => cs.push(c));
                r.on('end', () => res({ status: r.statusCode, body: Buffer.concat(cs), ms: Date.now() - t0 }));
            });
        req.on('error', rej);
        req.on('timeout', () => req.destroy(new Error('client timeout')));
    });
}

(async () => {
    for (let i = 0; i < 20; i++) {
        await new Promise((r) => setTimeout(r, 500));
        try {
            const r = await invoke('ping', 5000);
            if (r.body.toString('utf8').includes('pong')) break;
        } catch (e) { }
    }
    let allPass = true;
    for (let n = 1; n <= 3; n++) {
        try {
            const r = await invoke('bigChinese', 60000);
            const text = r.body.toString('utf8');
            const bad = (text.match(/\uFFFD/g) || []).length;
            const parsed = JSON.parse(text);
            const data = parsed.data == null ? '' : String(parsed.data);
            const hash = crypto.createHash('sha256').update(Buffer.from(data, 'utf8')).digest('hex');
            const ok = hash === expectHash && bad === 0 && parsed.status === 0;
            console.log(`[run ${n}] http=${r.status} ${r.ms}ms data=${data.length} chars U+FFFD=${bad} => ${ok ? 'PASS' : 'FAIL'}`);
            if (!ok) allPass = false;
        } catch (e) {
            console.log(`[run ${n}] FAIL: ${e.message}`);
            allPass = false;
        }
        await new Promise((r) => setTimeout(r, 300));
    }
    console.log(allPass ? 'ALL 3 RUNS PASS (enhanced client)' : 'HAS FAILURES');
    process.exit(allPass ? 0 : 1);
})();
