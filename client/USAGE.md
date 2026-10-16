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

### `client.registerAction(action, handler)`

- `action`：字符串（非字符串直接抛错）
- `handler(request, resolve, reject)`：
  - `request`：服务端下发的参数对象（invoke 的全部 query/body 参数）
  - `resolve(response)`：成功响应；response 可为对象/数组/字符串，自动包 envelope
  - `reject(errorMessage)`：失败响应（调用方收到 `status=-1, message=errorMessage`）
- handler 内抛出的异常自动转为失败响应，不会导致进程/页面崩溃

### `client.isConnected()`

连接是否可用（`readyState === OPEN`）。发送类操作在未连接时**不会静默丢弃**——
日志会明示「消息未送达（连接未建立）」。

## 与官方 sekiroWeb.js 的差异（5+2 项）

| # | 类型 | 内容 |
|---|---|---|
| 1 | 修复 | 官方 `onping` 回调引用未定义的 `socket.pong()`，服务端 Ping 到达即抛 ReferenceError（浏览器自动回 Pong）——已移除 |
| 2 | 增强 | 重连改为**指数退避**（1s 起步、×2 递增、30s 封顶、±500ms 抖动），替代官方固定 2s |
| 3 | 增强 | `onOpen` / `onReconnect` / `onClose` / `onError` 四个生命周期钩子 |
| 4 | 增强 | 大字符串响应仅 `{`/`[` 开头才 `JSON.parse`（3MB 文本省一次昂贵解析） |
| 5 | 修复 | `sendSuccess` 的 `code` falsy 判断缺陷（`code=0` 不再被误改写） |
| 6 | 增强 | **连接错误诊断**（本次新增，见下节） |
| 7 | 增强 | `isConnected()` 状态查询 + 未连接时发送保护（不静默丢消息） |

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

## 服务端配套验证

```bash
# 服务端（go/ 目录构建）+ 监控页
cd go && go build -o bin/sekiro.exe ./cmd/sekiro && ./bin/sekiro.exe --port 5612
# 浏览器打开 http://127.0.0.1:5612/op 观察 provider 在线数与调用量

# 3MB 中文乱码回归（增强版客户端）
node tests/js/enhanced_regression.js 5612
```
