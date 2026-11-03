# 更新日志

本项目的所有重要变更记录于此。格式参考
[Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循
[语义化版本](https://semver.org/lang/zh-CN/)：缺陷修复升 patch，新增特性升 minor。

产物与镜像随 `v*` 标签自动构建发布，各版本下载见
[Releases](https://github.com/luchatex/sekiro-open-go/releases)。

## [未发布]

以「系统错误可诊断」为主的一个补丁版本：起因是线上 `/op` 报出 224 条系统错误，
但原有报表只能看到「超时 147 条」，无法判断是上游变慢还是请求挂死。

### 新增

- **迟到响应统计**（`/op/api/errors` 的 `lateTotal` / `late`）：服务端按超时
  回包后 provider 才到达的响应会被计数（按 group / action 聚合，含最大延迟），
  并在监控页页头展示「迟到响应 N 条（最长 +Xs）」。这是区分「上游只是慢」与
  「上游挂死」的客观依据——迟到只差几秒应调大 `invoke_timeout`，迟到几十秒
  以上应断链重试。迟到回包本身仍按原逻辑丢弃，调用结果不变。
- **高频错误聚合**（`byAction`）：同 group / action / 原因归为一行、按次数降序，
  让「每分钟固定几条同类错误」直接显形。此前只能在 100 条事件里自己找规律；
  聚合条目上限 200 行、24h 未再发生即清理，不随事件环滚动丢失。
- **请求参数摘要**（`-op-err-params` / `SEKIRO_OP_ERR_PARAMS`，**默认关**）：
  开启后系统错误事件附带 `params` 摘要（跳过 `group`/`action`/`invoke_timeout`/
  `__sekiro_seq__` 等框架键，截断 256B），用于把「反复失败的同一条请求」
  指认出来；默认关闭，避免爬取 URL、文书 ID 等业务参数进入监控数据。

### 优化

- 监控页「系统错误分析」拆成原因分布 + 高频聚合 + 最近事件三段，事件表新增
  参数列；未开启参数摘要时在表头提示开关名，避免对着空列排查。
- `go/README.md` 新增「排障：怎么读系统错误」：按聚合 → 迟到响应 → 参数摘要
  三步定位，并说明 `caller-abort` 与调用方超时的关系。

## [v1.0.3] - 2026-11-02

发布镜像：`ghcr.io/luchatex/sekiro-open-go:1.0.3`（同时生成 `1.0`）。
以监控报表（`/op`）的正确性与开销为主的一个补丁版本。

### 修复

- **按天视图时间轴错到 2174 年**：`/op/api/stats` 的 `daily.buckets` 曾在小时间
  单位上多乘 3600，前端据此渲染出 2174 年。现在小时桶与天桶各按自己的单位换算
  为 unix 秒，天标签恢复为本地 0 点。
- **刷新间隔设置不生效**：页面脚本末尾有一处写死的 2s 定时器，会覆盖页头选择的
  2s / 5s / 10s / 暂停。现在首屏与登录后走同一条轮询路径，只保留一个定时器。
- **全局原因分布口径不一**：`/op/api/stats` 的 `sysErrReasons` 原本由各 group
  求和，漏掉 group 为空的错误（如调用方断连的 `caller-abort`），其合计小于
  `/op/api/errors` 的 `total`。现在统一取全局聚合。

### 优化

- **按天视图补齐 7 天**：天数据改为独立的天桶（保留 7 天），小时桶只覆盖 24h 图
  （保留 26h）。此前小时桶仅留 48h 而天图窗口是 7 天，导致后 4 天恒为 0。
  单维度桶数 34 个、约 3.0 KB；作为对比，把小时桶留满 7 天需 168 个桶、15.5 KB。
- **快照分配降七成**：`/op/api/stats` 的 group 聚合改用定长窗口数组按下标累加，
  并预分配输出切片，不再为每个 group 的每个桶新建对象。60 个维度 / 20 个 group
  时单次分配 121 KB → 36 KB，分配次数 207 → 162。

### 变更

- 监控页默认刷新间隔 2s → 5s（页头可切换）。
- 错误原因与事件详情改为中文展示（上游超时 / 分配失败 / 调用方断连），英文原因码
  并列保留便于对照日志；`/op/api/errors` 与 `/op/api/stats` 的 JSON 仍返回英文
  原文，与 Java 版文案保持一致，自建报表/Grafana 不受影响。

### 测试

- 新增快照分配预算用例（`TotalAlloc` 确定性断言，防止回退到 map 聚合）、
  天桶/小时桶单位与保留窗口用例、无 group 错误计入全局的用例。

## [v1.0.2] - 2026-10-30

发布镜像：`ghcr.io/luchatex/sekiro-open-go:1.0.2`（同时生成 `1.0`）。
在 v1.0.1 基础上的稳定性修复：

- 修复二进制 invoker 响应被原地压缩后，指标把压缩字节误记为逻辑出流量、并可能
  误判 `status` 的问题。
- 修复 gzip 流式解压上限与运行时限额判据不一致的问题，超限始终返回错误，不再出现
  `(nil, nil)`。
- OP 登录成功也触发过期会话清理；`/op/api/logout` 限定 POST。
- native 写路径改为 defer 解锁，避免异常时永久持有写锁。
- 默认 metrics 实例改为原子替换；修复 Go CI `-race` 发现的测试同步问题，并消除
  `/op` 指标测试的异步时序抖动。

## [v1.0.1] - 2026-10-30

发布镜像：`ghcr.io/luchatex/sekiro-open-go:1.0.1`（同时生成 `1.0`）。
发布流水线只生成语义化标签，不覆盖 `latest`；生产请固定完整版本。

缺陷修复版本：

- 修复断线/重连 Client 残留、未知 group 创建永久 actor、取消不生效等内存与
  goroutine 生命周期问题；空池退避与二进制 invoker 现可被调用方取消。
- 修复断线清理阻塞 group actor、`attached` 定时器竞态、空分段 pending 残留、
  HTTP 响应与 gzip 关闭竞态。
- 加固 gzip/snappy 解压预算：不再按不可信 gzip ISIZE 预分配；OP 登录/kick 增加
  body 读超时、JSON Content-Type 校验、可信代理 IP 判断和 TLS Secure Cookie。
- nginx 示例启用 upstream keepalive，修正 `/op` upstream 引用与重复 location。
- Docker 编排增加可配置内存硬限制、`GOMEMLIMIT`/`GOGC` 基线和 json-file 日志
  轮转；`docker run` 示例同步对应参数。

## [v1.0.0] - 2026-10-19

首个正式版本，涵盖：

- 与原 Java 版兼容的线上协议（HTTP / WebSocket / native 三通道）
- 监控报表 `/op`（含 `-op-password` 登录保护）
- 出流量按压缩后实际字节统计
- 修复原版 WebSocket 分片场景下的 UTF-8 解码乱码

单静态二进制、无 JVM 依赖。

[v1.0.3]: https://github.com/luchatex/sekiro-open-go/compare/v1.0.2...v1.0.3
[v1.0.2]: https://github.com/luchatex/sekiro-open-go/compare/v1.0.1...v1.0.2
[v1.0.1]: https://github.com/luchatex/sekiro-open-go/compare/v1.0.0...v1.0.1
[v1.0.0]: https://github.com/luchatex/sekiro-open-go/releases/tag/v1.0.0
