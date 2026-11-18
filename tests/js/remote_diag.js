// 全链路诊断：用仓库内的 sekiroWeb-enhanced.js 在 Node 里注册一个临时 client，
// 验证「握手 → 注册 → 定向调用」是否通。排障用，不是回归用例；跑完自动退出。
//
// 用法（Git Bash 必须禁路径转换，否则 "/register" 会被改写成
// C:/Program Files/Git/register，表现为 WS 永远 readyState=0）：
//   MSYS_NO_PATHCONV=1 node tests/js/remote_diag.js [WsBase] [HttpBase] [RegisterPath] [InvokePath]
//
// 例：
//   # 官方路径（本地）
//   node tests/js/remote_diag.js ws://127.0.0.1:5612 http://127.0.0.1:5612 /business/register /business/invoke
//   # 反代只放行部分路径时（常见：只放行 /register + /business-demo/*）
//   node tests/js/remote_diag.js wss://sekiro.example.com https://sekiro.example.com /register /business-demo/invoke
const fs = require('fs');
const vm = require('vm');
const path = require('path');

const WSBASE = process.argv[2] || 'ws://127.0.0.1:5612';
const BASE = process.argv[3] || 'http://127.0.0.1:5612';
const REG = process.argv[4] || '/business/register';
const INV = process.argv[5] || '/business/invoke';

const GROUP = process.env.GROUP || 'diagGroup';
const CID = process.env.CID || 'diag-node-' + Date.now().toString().slice(-6);

globalThis.window = { WebSocket: WebSocket };
vm.runInThisContext(fs.readFileSync(path.join(__dirname, '../../client/sekiroWeb-enhanced.js'), 'utf8'));

const t0 = Date.now();
const log = (...a) => console.log(`[+${Date.now() - t0}ms]`, ...a);

const client = new SekiroClient(`${WSBASE}${REG}?group=${GROUP}&clientId=${CID}`, {
  onOpen: () => log('onOpen: WS 握手成功，注册完成'),
  onClose: (ev, reason) => log('onClose:', reason),
  onError: (ev) => log('onError:', ev && ev.message ? ev.message : ev),
  onReconnect: (d) => log('onReconnect: 计划', d, 'ms 后重连'),
});
client.registerAction('ping', (req, resolve) => resolve({ pong: true, who: CID, echo: req.p || null }));

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function invoke(qs, label, timeoutMs = 8000) {
  const ac = new AbortController();
  const timer = setTimeout(() => ac.abort(), timeoutMs);
  const start = Date.now();
  try {
    const r = await fetch(`${BASE}${INV}?${qs}`, { signal: ac.signal });
    const body = (await r.text()).slice(0, 300);
    log(`invoke[${label}] http=${r.status} ${Date.now() - start}ms body=${body}`);
  } catch (e) {
    log(`invoke[${label}] 失败 ${Date.now() - start}ms: ${e.name}: ${e.message}`);
  } finally {
    clearTimeout(timer);
  }
}

(async () => {
  await sleep(5000);
  log('isConnected =', client.isConnected(), 'readyState =', client.socket && client.socket.readyState);
  if (client.isConnected()) {
    await invoke(`group=${GROUP}&action=ping&bind_client=${CID}&p=hello`, '本客户端 ping');
    await invoke(`group=${GROUP}&action=ping`, '本 group 轮询 ping');
  }
  // 定向一个不存在的 clientId：验证服务端文案是否带归属提示（不会打真实业务）
  await invoke(`group=${GROUP}&action=ping&bind_client=ghost-${Date.now()}`, 'bind_client 不存在', 6000);
  log('结束');
  process.exit(0);
})();
