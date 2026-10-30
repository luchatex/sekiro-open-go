# Sekiro-Go（Go 重写版）

> 本项目使用 Go 重写了 [yint-tech/sekiro-open](https://github.com/yint-tech/sekiro-open) 服务端，
> 线上协议与原 Java 实现兼容，客户端（Java/JS/Python 等）零改动接入。
> 原版介绍与致谢见[仓库根 README](../README.md)；真实客户端联调见 [../tests/](../tests/)。

## 快速开始

```bash
# 构建（Go 1.27+，外部依赖仅 coder/websocket 与 golang/snappy）
make build          # 产出 bin/sekiro（Windows: go build -o bin/sekiro.exe ./cmd/sekiro）

# 运行（默认端口 5612，与原版一致）
./bin/sekiro -port 5612

# 测试
make test

# 提交前置检查（gofmt + vet + 全量测试）
make check

# 竞态检测（-race 需 cgo，Windows 无 gcc 请在 Linux/wsl 或 CI 执行）
make race
```

单静态二进制，无 JVM、无私有 Maven 仓库依赖。

## 监控报表（/op）

内置单文件监控页（HTML+CSS+原生 JS，embed 进二进制，无任何前端框架/外部依赖），
指标口径对齐官方商业版监控报表：总量、成功量、失败量（provider 返回失败）、
系统错误（服务端拦截/超时）、流量、成功率，按 group / action 两级统计。

```bash
# 启动后浏览器打开
http://127.0.0.1:5612/op
```

- **KPI 总览**：group 数、在线 client 数、QPS（1 分钟均值）、累计调用、成功率、流量 In/Out
- **趋势图**：按小时（24h）/ 按天（7d）切换，SVG 堆叠柱（成功/失败/系统错误三色）
- **group 明细表**：六指标 + 24h sparkline，点击行展开 action 子表
- **系统错误分析**：原因分布（timeout / dispatch-failed / caller-abort）+ 最近 100 条事件
  （时间/group/action/详情/clientId/通道），定位「系统错误 N 条」的具体构成
- **数据 API**：`/op/api/stats`、`/op/api/clients`、`/op/api/errors` 返回 JSON 快照（可自建报表/对接 Grafana JSON datasource）
- **实现**：`internal/metrics` 内存统计（小时桶保留 48h、天精度由小时桶按本地时区聚合），
  进程重启清零、无持久化；埋点覆盖 HTTP 与二进制 invoker 两条消费通道
- **开关**：`-op-disable` 或环境变量 `SEKIRO_OP_DISABLE=true` 关闭页面与 API
- **密码保护**（v1.0.0+）：`-op-password` 或环境变量 `SEKIRO_OP_PASSWORD` 启用
  `/op` 与 `/op/api/*` 的登录访问——浏览器打开后弹出密码框，登录成功下发
  12h 会话 Cookie；**连续输错 3 次锁定来源 IP 2 分钟**（期内直接 429）。
  docker compose 在宿主机 `.env` 定义 `OP_PASSWORD` 即生效；未配置密码 =
  与 `/business` API 一致无鉴权，公网部署建议启用或前置网关访问控制
- **curl 脚本访问**（启用密码时）：先 `curl -c cookie.txt -H 'Content-Type: application/json' -X POST -d '{"password":"xxx"}' http://127.0.0.1:5612/op/api/login`，再 `curl -b cookie.txt http://127.0.0.1:5612/op/api/stats`；登录与踢下线接口只接受 JSON body

## Docker 部署

镜像采用多阶段构建（golang:1.27-alpine 编译 → alpine:3.20 运行，约 15MB），非 root 用户运行，内置健康检查（`/business/groupList`）。

```bash
# 方式零：拉取 GHCR 预构建镜像（每个 v* tag 由 CI 自动发布，amd64/arm64）
docker run -d --name sekiro -p 5612:5612 --restart unless-stopped \
  --memory=4g --memory-swap=4g \
  --log-driver=json-file --log-opt max-size=50m --log-opt max-file=3 \
  -e GOMEMLIMIT=3GiB -e GOGC=100 \
  ghcr.io/luchatex/sekiro-open-go:1.0.2

# 方式一：docker compose（推荐，编排文件在仓库根目录，已内置资源限制与日志轮转）
docker compose up -d --build
docker compose ps          # 等待 healthcheck 变 healthy
curl "http://127.0.0.1:5612/business/groupList"

# 方式二：纯 docker
docker build -t sekiro-go .
docker run -d --name sekiro -p 5612:5612 --restart unless-stopped \
  --memory=4g --memory-swap=4g \
  --log-driver=json-file --log-opt max-size=50m --log-opt max-file=3 \
  -e GOMEMLIMIT=3GiB -e GOGC=100 sekiro-go
```

- 宿主机端口通过 `.env`（`SEKIRO_HTTP_PORT`，参考 [`.env.example`](../.env.example)）调整，容器内固定 5612
- 容器配置对齐 `internal/config`：`SEKIRO_PORT` / `SEKIRO_LOCAL_DEBUG` / `SEKIRO_OP_PASSWORD` 等环境变量；完整清单见根 README
- `GOMEMLIMIT` 是 GC 软目标，不是容器硬限制，也不能修复泄漏；4GB `--memory` 才是宿主机保护。`GOGC=50` 会显著增加 GC CPU，不要作为默认值
- 裸 `docker run` 不配置 `--log-opt max-size` 时 json-file 日志无上限；生产请保留上面的日志参数
- 构建默认走 goproxy.cn 国内代理，海外可用 `--build-arg GOPROXY=https://proxy.golang.org,direct` 覆盖
- 服务端无状态（连接与路由在内存），无需挂载卷；水平扩容时多个 provider 直连即可
- 生产建议：前置 Nginx/Caddy 终结 TLS（浏览器侧 wss://），容器侧仅暴露内网；
  完整 WSS 部署指南（含 Cloudflare Tunnel 零证书方案与 localhost 豁免说明）
  见 [../deploy/README.md](../deploy/README.md)

## 与原版的差异

| 项 | 原版（Java） | Go 版 |
|---|---|---|
| 非 sekiro HTTP 请求 | 反向代理到 iinti.cn 文档站 | **返回 404**（按需求剔除，文档见官网） |
| 部署 | JVM（-Xmx1G）+ 脚本 | 单二进制 |
| 压缩 | gzip/snappy（反射探测） | gzip/snappy（内置），snappy 与 snappy-java 字节级一致 |
| WS 分片中文乱码 | 偶发（逐帧 text() 解码缺陷）；**本地参考版已同步修复**（字节级聚合，见 `java/LOCAL-PATCHES.md`） | **已消除**（字节级重组，见下方 D12） |
| 空池退避 | 20s 分支为死代码 | 按注释意图修复（>10min→15s，>30min→20s） |
| HTTP 响应 gzip | Netty HttpContentCompressor，无 Vary 头 | 同协商语义，额外发 `Vary: Accept-Encoding`（更符合 RFC 7231 缓存语义） |
| WS 传输压缩 | 无 | **permessage-deflate（RFC 7692）协商启用**，不支持的客户端自动降级；中文 JSON 文本实测压缩比约 10x+ |
| invoke 参数转发顺序 | fastjson 规范化为字段名字典序 | 保留原始顺序（JSON 对象无序，业务语义等价） |
| query/form 中文参数 | 未编码 UTF-8 解码为乱码（`值`→`￥ﾀﾼ`） | 正确按 UTF-8 解码（更宽容） |

## Shadow 对拍（M9，与原版 Java 服务端同流量对比）

以官方 SekiroClient 作为 provider 分别注册 Go 版（5620）与编译运行的 Java 版（5621），
对拍 HTTP 层全部路径：groupList / clientQueue（缺参/正常）/ invoke（空池、group 缺失、
bind_client 未找到、unknown action）/ 真实客户端 invoke 成功、分段流、100KB 大载荷、
gzip 响应头。**错误路径逐字段一致；成功路径（归一化 clientId 后）逐字节一致**。
对拍方法与环境见 [../tests/](../tests/)。

## 压测（M10，tools/loadtest）

Windows 10 回环环境，原生协议 provider（4 连接）+ HTTP invoker：

| 场景 | 吞吐 | P50 | P99 | max | 错误 |
|---|---|---|---|---|---|
| 256B 响应，c=50，n=20000 | 6038 req/s | 8.0ms | 19.3ms | 51ms | 0 |
| 64KB 响应，c=10，n=2000 | 2821 req/s（≈184MB/s） | 3.0ms | 8.4ms | 17ms | 0 |
| 64KB 响应，c=30，n=3000 | 3337 req/s（≈218MB/s） | 8.6ms | 17.6ms | 33ms | 0 |

压测后服务端内存 35MB，6 个 provider 连接全部健康，全程零异常日志。
压测发现并修复 2 个并发缺陷（写锁缺失导致的帧交错、pending 登记竞态），
修复前吞吐仅 ~600 req/s 且约 0.05% 请求空等 15s 超时。

```bash
go build -o loadtest ./tools/loadtest
./loadtest -mode provider -server 127.0.0.1:5620 -group lg -clients 4 -resp-size 256
./loadtest -mode invoke  -url http://127.0.0.1:5620 -group lg -c 50 -n 20000 -timeout 30s
```

## 架构

```
cmd/sekiro          入口（flag/env 配置，完整清单见根 README）
Dockerfile/.dockerignore  容器化（多阶段构建，alpine 运行时约 15MB）
internal/protocol   sekiro01 帧编解码 + 13 种包类型 + MurmurHash2-64
internal/compress   gzip/snappy/none（仅响应类包、COMPRESS_METHOD 头驱动）
internal/server     单端口协议探测分流（魔数 → 二进制；HTTP 方法行 → http.Server）
internal/group      group 调度核心（actor 模型、轮询/一致性哈希/bind_client、空池退避）
internal/invoke     （并入 group：Record 超时/分段流/seq 关联）
internal/native     二进制通道（client 注册/invoker 连接/心跳 30s+20s）
internal/httpapi    HTTP API + WebSocket 注册通道（分片字节级重组，D12）
internal/orderedjson 保序 JSON（对齐 fastjson OrderedField）
```

## 线上协议摘要

```
帧: magic(8B,"sekiro01") | totalLen(4B大端) | type(1B) | seq(4B) | headerCount(1B,≤127)
    | headers[kLen+key | vLen+value] | payload
HTTP API: /business/invoke | /business/groupList | /business/clientQueue（含 -demo 前缀）
监控:      /op（页面） | /op/api/stats（JSON 快照）
WS 注册:  /register?group=&clientId=（JSON 文本帧，__sekiro_seq__ 关联）
```

## 验证体系

- **黄金向量对拍**：用原 Java 私有 jar 产出基准（`testdata/golden/`）——帧编解码 12 例、
  MurmurHash 200 例、压缩 4 组；向量结果随仓库发布，生成器依赖原 Java 私有 jar，未随仓库发布。
- **单元测试**：编解码 roundtrip、退避时序、重复注册 30s 延迟关闭、注销同一性校验、
  seq 上限重置、JSON 保序、压缩协商门控、Record 响应终结、metrics 计数/小时桶/QPS、
  /op 页面与 stats 端到端埋点（成功/失败/系统错误三路径）。
- **E2E（线上协议级）**：真实 TCP 客户端注册 → HTTP 调用 → 中文响应闭环；
  invoker 通道 seq 映射；压缩响应解码；WS 分片响应**全切分点遍历（75/75）**断言无 U+FFFD 乱码；
  空池快速失败；未知协议拒绝。
- **真实客户端 SDK 联调（官方 SekiroClient API）**：注册/心跳/调用/分段流/100KB 压缩载荷
  全矩阵通过，中文无乱码（[../tests/](../tests/)，含官方 API 关键语义核实记录）。
- **HTTP 内容压缩**：Accept-Encoding 协商 gzip/deflate，对齐 Netty HttpContentCompressor。
- **竞态检测**：`go test -race` 覆盖 connListener 连接分发、group actor 串行化、
  Record 响应时序等并发路径；Windows 无 cgo 时在 Linux/WSL 执行（`make race`）。

## 已知限制（相对商业版/原版）

- 未实现：业务 API 鉴权（`sekiro_token` 校验）、集群、admin 管理台（开源版同样不含）；`/op` 自身可用 `-op-password` 保护
- Java 版反向代理文档站能力未移植（见差异表）
- JS/Python 客户端 SDK 联调未做（Java 客户端为生产同源实现，协议风险已覆盖）；
  上线前建议按开发计划 M9 与 Java 版服务端 shadow 对拍、M10 压测
