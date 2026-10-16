# sekiro-open-go 发布产物（v1.0.0）

单二进制交付：**静态链接（CGO_ENABLED=0）、无 glibc/运行时依赖、无外部配置文件**。
全部在 `go go1.27.2` 下以 `-trimpath -ldflags "-s -w"` 构建，来源 `go/cmd/sekiro` 与 `go/tools/loadtest`。

## 产物清单

| 文件 | 平台 | 大小 | sha256（前 16 位） |
|---|---|---|---|
| `sekiro-linux-amd64` | Linux x86_64（**CentOS/RHEL/Rocky 首选**） | 7.46 MB | `c72fa4be…` |
| `sekiro-linux-arm64` | Linux aarch64 | 6.88 MB | `a889fdae…` |
| `sekiro-windows-amd64.exe` | Windows x86_64 | 7.67 MB | `2406af00…` |
| `sekiro-darwin-arm64` | macOS Apple Silicon | 7.07 MB | `494104a2…` |
| `loadtest-linux-amd64` | Linux x86_64 压测/自检工具 | 5.70 MB | `3edbc0b2…` |
| `loadtest-windows-amd64.exe` | Windows x86_64 压测/自检工具 | 5.88 MB | `4d340322…` |

构建基线 `6c1756e`（2026-10-16）。本版在上一版（`56f1729`）基础上新增
**WS 压缩协商可观测**（大文本场景）：

- WS 注册日志补 `compression` 字段：`deflate`（permessage-deflate 协商
  成功）/ `off`（客户端未声明扩展）
- `/op/api/clients` 节点面板通道徽标旁新增压缩状态徽标：绿色 `deflate` /
  黄色 `off`（悬停提示常见成因——**前置 nginx 剥了 Sec-WebSocket-Extensions，
  大文本带宽静默放大 ~10 倍**）
- 实测：声明扩展的 WS 拨号报 `deflate`，默认拨号报 `off`（e2e 用例覆盖）

上一版（`56f1729`）的断开死因可观测、（`c79098e`）的 `-log-level`、
（`8c09bbf`）的「客户端节点」面板、（`e67ed94`）的心跳保活与注册可见性
修复均已包含。

### `-log-level` 日志级别参数（`c79098e`）

- 内部部署若不需要逐笔调用记录（/op 监控页已可查），`-log-level warn`
  即可关掉逐笔 invoke Info 日志，日志量降到近零增长（仅注册/告警/审计落盘）
- 优先级：`-debug` > `-log-level` > 默认 info；非法值回退 info 并在
  stderr 提示。实测：warn 级别下 50 笔调用 + provider 注册日志零输出

### /op 监控页「客户端节点」面板（`8c09bbf`）

- **节点信息采集与展示**：`/op/api/clients` 返回全部已注册 group 的节点明细——
  在线状态（绿点/灰点）、设备名（clientId）、group、IP 地址、通道类型
  （native/ws 徽标）、连接时间、最后活跃时间（相对时间「N 秒前」，按服务端
  时钟校正本机偏差）；支持同一设备重复连接标注
- **踢下线**：面板按钮 POST `/op/api/clients/kick`（group+clientId），服务端
  直接断开该 transport，触发既有 Disconnect 链路彻底注销并使该 client 的
  pending 调用立即失败；每次踢下线写入审计日志（来源 IP + 目标节点）
- **权限边界**：`-op-disable` 下两端点同样返回 404，与监控页一致

更早（`e67ed94`）的两个线上问题修复均已包含：

- **WS provider 心跳保活**：静默 js-web provider 此前每 30s 被「读空闲看护」
  误杀（coder/websocket 在 Read 内部消费 pong 不解除阻塞，lastRead 永不刷新），
  前端表现为频繁断开重连循环。现心跳 ping 收到 pong 即刷新读空闲，
  与 Java 版 Netty 行为一致。实测：回 pong 的静默 WS 连接 40s（> 30s 读空闲）
  存活；不回 pong 的 25.0s 被关（20s 首次 ping + 5s pong 超时，符合设计）
- **/op 监控页注册可见性**：client 注册后（尚无调用流量）group 即出现在
  `/op/api/stats`（clients 实时、指标为零），不再「有真实请求才显示」

此前的可靠性修复（body 读取超时、Disconnect 锁边界、断连回收 handler、
帧长防 OOM、压缩炸弹防护、WS 关闭码等）均已包含。

完整校验值见 [SHA256SUMS.txt](./SHA256SUMS.txt)：

```bash
sha256sum -c SHA256SUMS.txt
```

> 注意：产物经 WSL 拷贝后**执行位可能丢失**，Linux 上记得 `chmod +x`。

## 快速运行

```bash
# Linux（CentOS 部署详见 ../deploy/deploy-centos.md）
chmod +x sekiro-linux-amd64
./sekiro-linux-amd64 -port 5612

# Windows（PowerShell，或用 NSSM/sc.exe 注册为服务）
.\sekiro-windows-amd64.exe -port 5612
```

通用参数（环境变量等价）：

| flag | 环境变量 | 默认 | 说明 |
|---|---|---|---|
| `-port` | `SEKIRO_PORT` | `5612` | 明文监听端口，与原版一致 |
| `-debug` | `SEKIRO_LOCAL_DEBUG` | 关 | 调试日志（会打印完整调用参数，含业务参数） |
| `-log-level` | `SEKIRO_LOG_LEVEL` | `info` | 日志级别 debug/info/warn/error；`warn` 可关掉逐笔调用日志（监控页仍可查调用记录），配合 journald/logrotate 控制日志体积 |
| `-op-disable` | `SEKIRO_OP_DISABLE` | 关 | 关闭 `/op` 监控页及全部 `/op/api/*` 端点（stats/clients/kick） |
| `-tls-cert` / `-tls-key` | `SEKIRO_TLS_CERT` / `SEKIRO_TLS_KEY` | 空 | 同时提供则启用 wss 监听（浏览器 https 页面直连 wss，规避 LNA 只拦明文 ws 的问题） |
| `-tls-port` | `SEKIRO_TLS_PORT` | `5613` | wss 监听端口（证书为空时不启用） |

优雅关停：Linux/macOS 发 `SIGTERM`（等待在途调用回包，上限 20s 后退）；Windows 用 Ctrl+C。

## 自检流程（各平台通用）

```bash
# 1) 起服务端
./sekiro-<平台> -port 5612 &

# 2) 起 2 个模拟 provider
./loadtest-<平台> -mode provider -server 127.0.0.1:5612 -group smoke -clients 2 &

# 3) 核对注册与调用
curl -s "http://127.0.0.1:5612/business/groupList"        # {"data":["smoke"],...}
curl -s "http://127.0.0.1:5612/business/clientQueue?group=smoke"
curl -s -o /dev/null -w "%{http_code}\n" "http://127.0.0.1:5612/op"   # 200

# 4) 并发压测 1000 请求
./loadtest-<平台> -mode invoke -url http://127.0.0.1:5612 -group smoke -c 10 -n 1000
```

## 实测基线

| 环境 | 并发/请求数 | 吞吐 | p50 | p99 | 错误 |
|---|---|---|---|---|---|
| Windows 11 x64（本机，`c79098e` 版） | 10 / 1000 | 2589 req/s | 2.8 ms | 18.6 ms | 0 |
| WSL Ubuntu 24.04（`f4e1d03` 版） | 10 / 1000 | 2273–2699 req/s | 3.2–3.8 ms | 13.2–16.5 ms | 0 |

> 基线为同机自测（服务端与压测工具同机），仅用于冒烟判定，非目标机容量结论。
> `6c1756e` 版另验证压缩协商可观测：声明扩展的 WS 报 `deflate`、默认拨号报
> `off`（e2e）；`56f1729` 版验证断开死因日志：被动断开/心跳失败/读空闲三条
> 路径均输出 reason + lifetime；`c79098e` 版验证 `-log-level`（warn 下 50 笔
> 调用日志零输出、非法值回退 info）。
> `8c09bbf` 版验证「客户端节点」面板（产物级）：页面元素齐全、
> 2 节点可见（ip/transport 正确）、kick 后节点移除、stats clients 联动回落。
> 另验证 200 并发断连滥用下服务端无泄漏（goroutine/连接数回归基线）。
> `e67ed94` 版验证：真实 WS provider 回 pong 静默存活 40s（> 30s 读空闲）、
> 不回 pong 的 25.0s 被关并移出注册表；注册无流量的 group 立即出现在 `/op/api/stats`。

## 可复现构建

```bash
cd go
export CGO_ENABLED=0
GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o ../release/sekiro-linux-amd64   ./cmd/sekiro
GOOS=linux   GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o ../release/sekiro-linux-arm64   ./cmd/sekiro
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o ../release/sekiro-windows-amd64.exe ./cmd/sekiro
GOOS=darwin  GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o ../release/sekiro-darwin-arm64  ./cmd/sekiro
# 压测工具同理，包路径换成 ./tools/loadtest
```

质量门禁：Linux 原生 `go test -race ./...` 全绿（含协议黄金向量对拍与端到端用例），`go vet` 与 `gofmt` 干净。

> 本目录二进制**不入库**（`.gitignore` 忽略），只有本说明与 SHA256SUMS.txt 纳入版本管理。
