# sekiroWeb-enhanced.js 使用文档

`sekiroWeb-enhanced.js` 是官方 `demo-web/sekiroWeb.js` 的**生产可用增强版**，
API 完全兼容——业务代码从官方版迁移**只需替换 script 引用，零改动**。

> 定位区分：本目录只放客户端库（进生产）；回归/联调脚本在 [`../tests/js/`](../tests/js/)。

## 快速开始

### 浏览器（provider：接收服务端下发指令并返回结果）

```html
<script src="sekiroWeb-enhanced.js"></script>
<script>
  // 写法一（推荐）：基础地址 + group/clientId，自动拼接注册路径
  var client = new SekiroClient('ws://127.0.0.1:5612', {
    group: 'demoGroup',
    clientId: 'my-web-client-1'
  });

  // 写法二（与官方一致）：完整注册地址
  // var client = new SekiroClient('ws://127.0.0.1:5612/business/register?group=demoGroup&clientId=my-web-client-1');

  // 注册 action：服务端通过 /business/invoke?group=demoGroup&action=test 调用
  client.registerAction('test', function (request, resolve, reject) {
    // request = 服务端下发的全部参数（含 URL query 与 body）
    resolve('hello ' + request.param);   // 成功：返回任意对象/字符串
    // reject('业务失败原因');            // 失败：status=-1
  });
</script>
```

> ⚠️ **注册地址是关键**：WS 必须连到 `/business/register?group=xx&clientId=xx`，
> 连到根路径会握手失败（code=1006）。增强版两种写法都支持；地址不含 `/register`
> 且未提供 `group`/`clientId` 时**构造即抛错**并给出两种正确写法。

### Node.js（本地调试）

```js
// Node 22+ 自带原生 WebSocket（undici），无需安装依赖
require('./sekiroWeb-enhanced.js');
const client = new SekiroClient('ws://127.0.0.1:5612', { group: 'demoGroup', clientId: 'node-1' });
client.registerAction('test', (req, resolve) => resolve({ ok: true }));
```

### 完整示例（含断线感知）

```js
var client = new SekiroClient('wss://sekiro.example.com', {
  group: 'demoGroup',
  clientId: 'prod-web-1',
  onOpen: function () {
    console.log('已连接');                  // 首次连接成功 & 每次重连成功
  },
  onReconnect: function (delayMs) {
    console.log('断线，' + delayMs + 'ms 后重连');  // 每次调度重连时回调
    // 适合在这里做 UI 状态提示（如页面顶栏显示"重连中"）
  },
  onClose: function (event, humanReason) {
    console.log('连接关闭：' + humanReason); // 带 code 诊断的关闭原因
  },
  onError: function (event) {
    // 浏览器不透传错误细节，诊断以紧随其后的 close code 为准
  }
});

if (client.isConnected()) { /* 连接可用 */ }
```

## API 参考

### `new SekiroClient(wsURL, options?)`

| 参数 | 必填 | 说明 |
|---|---|---|
| `wsURL` | ✅ | 必须以 `ws://`/`wss://` 开头；**要么是完整注册地址**（`…/business/register?group=xx&clientId=xx`）**要么配合 options 的 group+clientId 自动拼接**，否则构造即抛错 |
| `options.group` | 二选一 | group 名（与 `clientId` 一起提供时自动拼注册地址） |
| `options.clientId` | 二选一 | provider 客户端 ID；同 group 重复 ID 会让旧连接延迟 30s 关闭 |
| `options.onOpen` | – | 连接建立回调（含重连成功） |
| `options.onReconnect(delayMs)` | – | 每次调度重连时回调，参数为本次等待毫秒数 |
| `options.onClose(event, humanReason)` | – | 连接关闭回调，`humanReason` 为 code 的中文诊断 |
| `options.onError(event)` | – | 连接错误回调（细节以 onClose 的 code 为准） |
| `options.heartbeat` | – | 应用层心跳开关，默认 `true`（连到旧服务端会自动降级，见下节） |
| `options.heartbeatInterval` | – | 心跳间隔（毫秒），默认取服务端通告值 15000；显式配置后以本值为准 |
| `options.onHeartbeatLost(idleMs)` | – | 心跳失联判定触发时回调（此时会自动重建连接），`idleMs` 为无下行帧时长 |

### 应用层心跳（半开连接检测）

浏览器没有 WS Ping API，而 provider 平时是纯静默端——链路被中间设备悄悄切断
（NAT 回收、休眠唤醒、网络切换、网关静默丢流）时，服务端 25s 内就会靠 Ping 判死
并把设备移出 group，而页面这边**永远停在 `readyState=1`、控制台一条日志都没有**。
业务侧的表现是「设备凭空消失，直到有人刷新页面才回来」。

增强版的做法：

1. 注册地址自动追加 `hb=1`（`heartbeat: false` 可关闭）；
2. 新服务端会回一帧能力通告 `{"__sekiro_hello__":1,"hb":10}`，客户端据此每 10s 发
   `{"__sekiro_hb__":<ts>}`，服务端原样回显；
3. 连续 **3 个周期**（10s × 3 = 30s，与服务端判死时长 20s+10s 对齐）没有任何下行帧
   → 判定链路已半开，立刻丢弃旧连接并重连（不等关闭握手，因为关闭帧同样可能传不回来）；
4. 连到**旧服务端**时收不到通告：本连接不发心跳、行为与官方版完全一致，
   不会因未知帧被 4400 踢掉。

两个细节：页面处于**后台标签页**时看护不做判定（浏览器会节流后台定时器，误判会把
健康连接掐掉），切回前台立刻补发一次心跳再重新计时；`heartbeatInterval` 显式配置时
优先于服务端建议值。

```js
var client = new SekiroClient('wss://sekiro.example.com', {
  group: 'demoGroup', clientId: 'prod-web-1',
  onHeartbeatLost: function (idleMs) {
    console.warn('心跳失联 ' + idleMs + 'ms，已自动重连');
  }
});
// 特殊网络想更激进地判活：heartbeatInterval: 5000（3 个周期 = 15s 判定）```

### `client.registerAction(action, handler)`

- `action`：字符串（非字符串直接抛错）
- `handler(request, resolve, reject)`：
  - `request`：服务端下发的参数对象（invoke 的全部 query/body 参数）
  - `resolve(response)`：成功响应；response 可为对象/数组/字符串，自动包 envelope
  - `reject(errorMessage)`：失败响应（调用方收到 `status=-1, message=errorMessage`）
- handler 内抛出的异常自动转为失败响应，不会导致进程/页面崩溃

### `client.registerLongAction(action, handler, options)`（慢请求专用）

适用于单次处理确实要几十秒甚至上百秒的 action。它会自动每 `progressInterval`
毫秒发一帧 `{"__sekiro_progress__":seq,"__sekiro_seq__":seq}`，服务端**只刷新该调用
的超时、不向调用方转发任何数据**；resolve/reject 时自动停止。

```js
client.registerLongAction('searchCprq', async function (req) {
  return await searchInWorker(req); // 长任务放 Web Worker
}, { progressInterval: 10000 });
```

- `options.progressInterval`：默认 10000ms（10s），应小于服务端 invoke 超时的一半
- `options.singleFlight`：默认 `true`，同一 action 已有在途任务时立即
  `reject('busy, retry with backoff')`，不排队堆积；设 `false` 可关闭
- `options.busyMessage`：自定义忙信号文案
- handler 可用回调风格或返回 Promise；两者都只结算一次
- **务必让主线程可让出**：长任务若同步阻塞主线程，原来的应用层心跳（10s × 3）
  和进度帧都会停摆，页面仍会被当成假离线。用 Web Worker 或分段 `await`。

调用方仍需注意：服务端每 10s 收到进度帧会滚动续期，但调用方自己的 HTTP 超时
必须覆盖任务总时长（例如 120s），否则会在中途主动断开。

### `client.isConnected()`

连接是否可用（`readyState === OPEN`）。发送类操作在未连接时**不会静默丢弃**——
日志会明示「消息未送达（连接未建立）」。

## 与官方 sekiroWeb.js 的差异（5+3 项）

| # | 类型 | 内容 |
|---|---|---|
| 1 | 修复 | 官方 `onping` 回调引用未定义的 `socket.pong()`，服务端 Ping 到达即抛 ReferenceError（浏览器自动回 Pong）——已移除 |
| 2 | 增强 | 重连改为**指数退避**（1s 起步、×2 递增、30s 封顶、±500ms 抖动），替代官方固定 2s |
| 3 | 增强 | `onOpen` / `onReconnect` / `onClose` / `onError` 四个生命周期钩子 |
| 4 | 增强 | 大字符串响应仅 `{`/`[` 开头才 `JSON.parse`（3MB 文本省一次昂贵解析） |
| 5 | 修复 | `sendSuccess` 的 `code` falsy 判断缺陷（`code=0` 不再被误改写） |
| 6 | 增强 | **连接错误诊断**（见下节） |
| 7 | 增强 | `isConnected()` 状态查询 + 未连接时发送保护（不静默丢消息） |
| 8 | 增强 | **应用层心跳**（注册带 `hb=1`）：半开链路 3 个周期内发现并重连；旧服务端自动降级 |

## 连接错误诊断与排查

### 构造阶段（同步抛错，立即可见）

| 错误信息 | 原因 |
|---|---|
| `无效的 wsURL "..." —— 必须以 ws:// 或 wss:// 开头` | URL 漏了协议前缀（如写成 `127.0.0.1:5612`） |
| `[Mixed Content] 当前页面为 https…` | https 页面连非本机 `ws://`——改 wss（见下）或 provider 放本机 |

> https 页面连 `ws://127.0.0.1` / `ws://localhost` **不受此限制**（浏览器安全上下文豁免）。

### 连接阶段（看控制台 close 日志的 code 诊断）

| 日志片段 | 原因与处理 |
|---|---|
| `code=1006（连接未建立即断开 —— 服务端未启动/端口错误/防火墙或代理拦截/网络不可达）` | **最常见**。先 `curl http://host:5612/business/groupList` 确认服务端可达；再核对端口与防火墙；若控制台显示连接目标是根路径 `/` 而非 `/business/register`，说明注册地址没拼对（增强版此时会在构造阶段直接抛错提示）；**https 页面连 ws://127.0.0.1 失败**且服务端正常时，为浏览器 LNA 策略拦截（`net::ERR_BLOCKED_BY_LOCAL_NETWORK_ACCESS_CHECKS`，见上节——改用 http 页面或 wss 反代） |
| `code=1015（TLS 握手失败 —— wss 证书不受信/过期）` | wss 证书问题：自签证书需用 mkcert 装入信任库（页面级「继续前往」对 wss 无效）；或证书过期 |
| `code=1001（对端离开）` | 服务端重启/主动关闭，等待自动重连即可 |
| `code=1008（被服务端策略拒绝）` | 服务端鉴权/策略拦截（开源版通常不会出现） |
| `code=1011（服务端内部错误）` | 查服务端日志 |

### 重连机制

断线后自动重连，间隔 `1s → 2s → 4s → … → 30s`（封顶）+ 随机抖动；
连接成功后间隔重置回 1s。全程自动，无需业务代码干预。

## HTTPS / WSS 部署对接

https 页面必须用 `wss://`。服务端保持明文，反代/隧道层终结 TLS：

- Caddy / Nginx 反代配置、Cloudflare Tunnel 零证书方案：
  见仓库 `deploy/README.md`
- 服务端 `deploy/` 目录下有现成配置文件

```js
// 生产（推荐）
new SekiroClient('wss://sekiro.example.com', { group: 'demoGroup', clientId: 'prod-1' });
```

### https 页面连本机 provider（wss + 自签证书）——✅ 实测可行

浏览器 LNA 只拦明文 `ws://`，对 `wss://127.0.0.1` 不拦（实测）。自签
证书把 CA 导入系统信任库后即可直连，完整步骤见仓库 `deploy/README.md`
「本机 wss + 自签证书」一节（openssl 生成 + certutil 导入 + 服务端
`--tls-cert` 启用，5 分钟）：

```js
new SekiroClient('wss://127.0.0.1:5613', { group: 'demoGroup', clientId: 'web-1' });
```

注意：明文 `ws://127.0.0.1` 从 https 页面连接仍会被 LNA 拦截
（`net::ERR_BLOCKED_BY_LOCAL_NETWORK_ACCESS_CHECKS`），必须走 wss。

## 与原有 client 的兼容性

新增能力全部是**可选**的，原有 provider 页面和调用方代码都可以不改：

| 场景 | 是否需要改 | 说明 |
|---|---|---|
| 官方 `sekiroWeb.js` provider | 不改 | 服务端只是新增进度帧识别，旧注册/回包路径不变；长任务靠调用方传够 `invoke_timeout` |
| 现有 fetch / axios 调用方 | 不改 | 历史拼写 `invoke_timeOut` 服务端已兼容，100s 意图会生效 |
| 增强版 provider | 可选 | 换 `sekiroWeb-enhanced.js` 后可用 `registerLongAction` 自动发进度帧 + 忙信号 |
| 调用方封装 | 可选 | `sekiroInvoker.js` 是替代手写 fetch 的参考实现，不接入也不影响旧调用 |

**旧客户端长任务示例**：provider 不声明 `hb=1`、不发进度帧，调用方仍写
`invoke_timeOut=1200`，900ms 后回包，服务端必须成功。该路径已有 E2E 覆盖
（`TestOldWSClientLongInvokeWithAliasTimeout`）。

## 调用方参考实现（慢请求防堆积）

`client/sekiroInvoker.js` 是给调用方用的参考封装，把方案里的四条约束内置了：
正确的 `invoke_timeout`（毫秒）、调用方 HTTP 超时（默认 120s）、**每 clientId
并发 1**、最多 2 次退避 + 抖动重试，并带幂等缓存与熔断。

```js
const SekiroInvoker = require('./sekiroInvoker.js');  // 浏览器用 <script> 引入即可

const invoker = new SekiroInvoker('https://sekiro.example.com', {
  timeoutMs: 120000,        // 调用方 HTTP 超时，须 > 任务总时长
  serverTimeoutMs: 100000,  // 写进 invoke_timeout，服务端按此等待
  breakerFailures: 3        // 同 clientId 5 分钟内 3 次失败 → 熔断 5 分钟
  // path: '/business/invoke'（默认；本地 demo 可用 demo:true）
  // method: 'GET'（默认）或 'POST'（form body）
});

const res = await invoker.invoke('ws_list', 'searchCprq',
  { startDate: '2023-07-16', endDate: '2023-11-07', pageNum: 40 },
  { bindClient: '15696699241' });
```

- 默认走生产路径 `/business/invoke`；沿用 `/business-demo/invoke` 的旧脚本传
  `demo:true` 或 `path:'/business-demo/invoke'` 即可。
- 默认 `GET`，大参数可传 `method:'POST'`（`application/x-www-form-urlencoded`）。
- 同一 `bindClient` 的请求自动串行；不同 clientId 互不影响。没有 `bindClient`
  的池化调用默认不串行，避免人为压低吞吐；需要时传 `serialize:true`。
- 重试只针对 `busy` / `timeout` / `upstream lose connection` 和网络错误；
  业务失败（如 `no device found`）不重试。
- 相同参数默认按幂等键缓存 5 分钟；写操作请传 `idempotencyTTL:0` 关闭缓存，
  或显式指定 `idempotencyKey`。
- 熔断期间直接抛 `circuit open`，不再向慢设备发请求。

本地自测：`node client/test-sekiroInvoker.js`（无需服务端，注入 fake fetch）。

## 服务端配套验证

```bash
# 服务端（go/ 目录构建）+ 监控页
cd go && go build -o bin/sekiro.exe ./cmd/sekiro && ./bin/sekiro.exe --port 5612
# 浏览器打开 http://127.0.0.1:5612/op 观察 provider 在线数与调用量
# 若服务端启用 -op-password，打开 /op 会先弹出登录框（连续 3 次错锁 IP 2 分钟）

# 3MB 中文乱码回归（增强版客户端）
node tests/js/enhanced_regression.js 5612
```
