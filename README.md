# Sekiro Open Go

> [yint-tech/sekiro-open](https://github.com/yint-tech/sekiro-open) 的 **Go 语言重写版**：
> 线上协议与原 Java 实现兼容，官方客户端（Java / JS / Python）零改动接入。
> 单静态二进制、无 JVM 依赖、内置监控报表，并修复了原版的多个线上缺陷。

## Sekiro 是什么（引自原项目）

SEKIRO 是一个多语言、分布式、网络拓扑无关的**服务发布平台**（RPC 框架）：在各自的运行环境里用任意语言编写 handler，把能力发布到中心 API 市场，业务系统通过 RPC 调用远端节点。

它与传统 RPC（Dubbo / Spring Cloud / gRPC）的定位不同，主打**把受限环境中的能力暴露出来**：

- **App / 终端程序**：具备 C 端能力，希望 B 端业务调用——通过 Sekiro 把参数转发到 App 内执行
- **受限网络**：数据只允许组织内网访问，可在内网运行 Sekiro 客户端对外发布 API
- **加密算法**：算法运行在客户端程序中尚未完成逆向，可注入代码发布为 API
- **服务能力保护**：能力提供方不交付代码、不暴露机器信息，调用方仅通过 Sekiro 使用

完整介绍见原项目：[GitHub 仓库](https://github.com/yint-tech/sekiro-open) ·
[官方文档](http://sekiro.iinti.cn/sekiro-doc/) ·
[多语言示例](https://github.com/yint-tech/sekiro-samples)

> 致谢 [yint-tech](https://github.com/yint-tech) 的原版实现——本项目在协议设计、
> 本项目完全基于原版进行go语言复现，并根据大文本优化。

## 为什么重写


原版基于 JVM（Netty + fastjson），对本项目的使用场景（爬虫回传大文本、js-web 浏览器 provider）存在几个痛点：

1. **WS 分片中文乱码**：原版逐 WebSocket 分帧解码 UTF-8，多字节汉字跨分片时产生 `U+FFFD`（实际复现：3MB 中文响应 16 个切分点仅 2 个通过）
2. **部署形态重**：JVM + Maven 私有仓库 + 脚本，期望单文件交付
3. **可观测性弱**：开源版无监控报表（商业版才有），线上排障靠翻日志
4. **WS 无传输压缩**：大文本回传带宽放大 10 倍以上

Go 重写版在保持**线上协议字节级兼容**的前提下解决以上问题，并通过黄金向量对拍 + 真实客户端联调 + Shadow 对拍 + 压测四层验证体系保证等价性。

## 与原 Java 版对比

### 功能

| 项 | 原版（Java） | 本项目（Go） |
|---|---|---|
| 部署形态 | JVM（默认 -Xmx1G）+ Maven 构建 + 启动脚本 | **单静态二进制**（CGO 关闭，无 glibc 依赖），alpine 镜像约 15MB |
| WS 分片中文乱码 | 存在（逐帧 `text()` 解码缺陷） | **已消除**：字节级重组完成后交付，75/75 全切分点 E2E 验证 |
| WS 静默 provider 保活 | Netty IdleStateHandler 依赖 pong 帧走 channelRead 重置计时 | 心跳 ping 收到 pong 即刷新读空闲，行为对齐；断开均带死因日志（`read-error` / `pong-timeout` / `read-idle` + 存活时长） |
| WS 传输压缩 | 无 | **permessage-deflate（RFC 7692）+ ContextTakeover**，中文 JSON 实测压缩比 10x+，不支持端自动降级；协商状态在监控面板可见 |
| HTTP 传输压缩 | gzip 协商（无 Vary 头） | 同协商语义 + `Vary: Accept-Encoding`（RFC 7231） |
| 监控报表 | 开源版无（商业版功能） | **内置 /op 单文件页面**：KPI 总览、24h/7d 趋势、group/action 两级、流量 In/Out、客户端节点面板（在线状态/IP/最后活跃/踢下线）、JSON API 可对接 Grafana |
| 日志 | 固定级别 | `-log-level` 可调（debug/info/warn/error），warn 可关逐笔调用日志 |
| 非 sekiro HTTP 请求 | 反向代理到 iinti.cn 文档站 | 返回 404（文档见原官网） |
| 空池退避 | 20s 分支为死代码 | 按源码注释意图修复 |
| query/form 中文参数 | 未编码 UTF-8 解码为乱码 | 正确按 UTF-8 解码 |
| 内存安全防护 | — | body 读取超时（防挂死连接）、帧长上限 + 压缩炸弹防护、断连全量回收（goroutine/fd 零泄漏验证） |

**协议兼容性**（对拍验证）：sekiro01 帧编解码、13 种包类型、fast-json 信封、
gzip/snappy 帧压缩（snappy 与 snappy-java 字节级一致）、MurmurHash2-64 一致性哈希、
HTTP API 全路径错误语义逐字段一致、成功路径逐字节一致。

### 性能（Go 实测，Windows 10 回环，原生协议 provider ×4 + HTTP invoker）

| 场景 | 吞吐 | P50 | P99 | 错误 |
|---|---|---|---|---|
| 256B 响应，c=50，n=20000 | 6038 req/s | 8.0ms | 19.3ms | 0 |
| 64KB 响应，c=10，n=2000 | 2821 req/s（≈184MB/s） | 3.0ms | 8.4ms | 0 |
| 64KB 响应，c=30，n=3000 | 3337 req/s（≈218MB/s） | 8.6ms | 17.6ms | 0 |

压测后服务端内存约 35MB。未对原 Java 版做同环境基准对比（欢迎 PR 补充）；
以上为同机自测数据，仅作参考基线，非容量结论。

## 快速开始

```bash
# 方式一：源码构建（Go 1.22+，外部依赖仅 coder/websocket 与 golang/snappy）
git clone https://github.com/luchatex/sekiro-open-go.git
cd sekiro-open-go/go
make build                      # 产出 bin/sekiro
./bin/sekiro -port 5612         # 默认端口与原版一致

# 方式二：Docker
docker compose up -d --build    # 编排在仓库根目录，多阶段构建，alpine 约 15MB
curl "http://127.0.0.1:5612/business/groupList"
```

### 配置参数（flag 与环境变量等价）

| flag | 环境变量 | 默认 | 说明 |
|---|---|---|---|
| `-port` | `SEKIRO_PORT` | `5612` | 明文监听端口 |
| `-debug` | `SEKIRO_LOCAL_DEBUG` | 关 | 调试日志（打印完整调用参数，生产慎用） |
| `-log-level` | `SEKIRO_LOG_LEVEL` | `info` | 日志级别；`warn` 可关逐笔调用日志 |
| `-op-disable` | `SEKIRO_OP_DISABLE` | 关 | 关闭 `/op` 监控页及全部 `/op/api/*` |
| `-tls-cert/-tls-key` | `SEKIRO_TLS_CERT/KEY` | 空 | 同时提供则启用 wss 监听 |
| `-tls-port` | `SEKIRO_TLS_PORT` | `5613` | wss 监听端口 |

优雅关停：SIGTERM/SIGINT 等待在途调用回包（上限 20s 后退）。

### 客户端接入

- **Java / Python 客户端**：直接使用 [官方 sekiro-samples](https://github.com/yint-tech/sekiro-samples) SDK，服务端地址指向本项目即可
- **JS 客户端**：`client/sekiroWeb-enhanced.js`（增强版：指数退避自动重连、心跳容错），用法见 [client/USAGE.md](client/USAGE.md)

## 监控报表

启动后浏览器打开 `http://127.0.0.1:5612/op`（单文件页面，embed 进二进制，零前端依赖）：

- **KPI 总览**：group 数、在线 client 数、QPS、累计调用、成功率、流量 In/Out
- **趋势图**：24h（小时精度）/ 7d（天精度）SVG 堆叠柱（成功/失败/系统错误三色）
- **客户端节点面板**：在线状态、设备名、IP、通道类型、WS 压缩协商状态（`deflate`/`off`）、
  连接/最后活跃时间、一键踢下线（带审计日志）
- **数据 API**：`/op/api/stats`、`/op/api/clients`

## 仓库结构

```
sekiro-open-go/
├── go/                      # ★ Go 服务端主线（构建、测试在此执行）
│   ├── cmd/sekiro/          #    入口
│   ├── internal/            #    protocol / compress / server / group / httpapi / metrics ...
│   ├── tools/loadtest/      #    压测工具
│   └── testdata/golden/     #    黄金测试向量（帧编解码 / MurmurHash / 压缩）
├── client/                  # JS 客户端 SDK（sekiroWeb-enhanced.js，进生产）
├── tests/                   # 真实客户端联调（官方 Java SDK demo / Python / JS 回归脚本）
├── deploy/                  # 反代配置（nginx / Caddy）、CentOS 部署与日志管理指南
├── release/                 # 预编译产物清单与校验（二进制不入库）
├── docker-compose.yml
└── README.md
```

> Go 的 `_test.go` 测试与被测代码同包（语言要求，可测包内私有函数），位于 `go/internal/*/`，
> `make test` 一键执行；`tests/` 存放跨语言真实客户端联调资产。

## 测试与质量

```bash
cd go
make test    # 全量测试
make check   # gofmt + vet + 全量测试
make race    # 竞态检测（Linux/WSL）
```

四层验证体系：

1. **黄金向量对拍**：帧编解码 12 例、MurmurHash 200 例、压缩 4 组，与 Java 实现字节级一致
2. **单元 + E2E**：线上协议级真实 TCP/WS 客户端闭环、WS 分片全切分点遍历（75/75）无乱码、
   断连滥用（200 并发）资源零泄漏
3. **Shadow 对拍**：与编译运行的 Java 版同流量对比，HTTP 全路径错误语义逐字段一致、成功路径逐字节一致
4. **真实 SDK 联调**：官方 SekiroClient 注册/心跳/调用/分段流/100KB 压缩载荷全矩阵

## 部署

- **CentOS**：[deploy/deploy-centos.md](deploy/deploy-centos.md)（含 systemd + journald 日志管理、断开重连排查速查表）
- **TLS/WSS**：[deploy/README.md](deploy/README.md)（nginx / Caddy 反代配置、Cloudflare Tunnel 零证书方案）
- **预编译产物**：见 [release/README.md](release/README.md)（linux amd64/arm64、windows、darwin）

## 已知限制

- 未实现：鉴权（sekiro_token）、集群、admin 管理台（原开源版同样不含）
- 非 sekiro HTTP 请求不反代文档站（返回 404）
- `/business` API 与 `/op` 均无鉴权，公网部署务必前置网关做访问控制
- 黄金向量生成器（依赖原 Java 私有 jar）未随仓库发布，向量结果已固化在 `go/testdata/golden/`

## 版本发布

推送 `v*` 标签即自动发布（GitHub Actions 全平台构建 → 全量测试 → 冒烟自检 →
创建 Release 并上传产物与 SHA256SUMS.txt）：

```bash
git tag v1.0.1 && git push origin v1.0.1
```

历史版本见 [Releases](https://github.com/luchatex/sekiro-open-go/releases)。

## License

本项目以 [Apache-2.0](LICENSE) 发布（Copyright © 2026 luchatex）。

本项目为独立实现的 Go 重写版，未包含原项目代码；原项目
[yint-tech/sekiro-open](https://github.com/yint-tech/sekiro-open) 仓库未附带 LICENSE 文件，
其权利归属以原作者声明为准。
