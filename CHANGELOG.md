# 更新日志

本项目的所有重要变更记录于此。格式参考
[Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循
[语义化版本](https://semver.org/lang/zh-CN/)：缺陷修复升 patch，新增特性升 minor。

产物与镜像随 `v*` 标签自动构建发布，各版本下载见
[Releases](https://github.com/luchatex/sekiro-open-go/releases)。

## [Unreleased]

### 修复

- **compose 支持 `SEKIRO_OP_ERR_PARAMS`**：参数摘要开关此前只能由 flag 或
  `docker run -e` 设置，`.env` 里写了也不会被 compose 转发进容器；现已补上
  环境变量透传与示例说明。

### 新增

- **监控页显示构建版本**：`/op` 页头展示 `version`，由构建时 `-X` 注入
  （Release 取 tag 号，本地构建默认 `dev`）；镜像与二进制来源一眼可辨。

## [v1.1.2] - 2026-11-27

发布镜像：`ghcr.io/luchatex/sekiro-open-go:1.1.2`（同时生成 `1.1`）。
以「定位失败调用方」为主的补丁版本：系统错误事件新增来源 IP，监控页最近事件
表格同步展示，不再依赖 nginx access log 是否记录 invoke 参数。

### 新增

- **系统错误事件带来源 IP**：`/op/api/errors` 的 `recent` 事件新增 `sourceIp`，
  始终采集并去掉端口；反代后按可信规则取 `X-Real-IP` / `X-Forwarded-For`。
  定位「谁在反复发这条请求」不再依赖 nginx access log 是否记录 invoke 参数。
  参数摘要 `-op-err-params`（默认关）仍然独立，避免业务数据进入监控。

## [v1.1.1] - 2026-11-23

发布镜像：`ghcr.io/luchatex/sekiro-open-go:1.1.1`（同时生成 `1.1`）。
以「降低内存峰值 + 可观测性收尾」为主的补丁版本：每连接嗅探缓冲降到 8KB、
gzip 状态对象池化、单帧上限收紧到默认 16MB；监控页最近事件表格不再撑出卡片，
并补上 README 请求链路示意图。

### 新增

- **README 增加请求链路示意图**：`assets/sekiro-flow.svg` 内嵌展示 Provider 注册、
  心跳、Invoker 调用、group ring 派发、handler 回包与监控埋点六步；另提供
  `assets/sekiro-flow.html` 单文件交互版，可逐步高亮并查看解释。两版均无外部依赖。

### 优化

- **每连接嗅探缓冲 64KB → 8KB**：嗅探只需 8~9 字节，HTTP keep-alive 连接却会
  一直持有该缓冲；1000 条空闲连接因此少占约 56MB。
- **池化 gzip 状态对象**：`gzip.Writer` / `gzip.Reader` 与 64KB 解压 chunk 进池，
  避免每次请求新建 deflate 状态；结果缓冲不进池，防止大响应后缓冲常驻。
  snappy 的 Encode/Decode 没有可复用内部状态，强行池化 dst 需要多一次拷贝且
  净分配不变，因此不改。
- **单帧上限 64MB → 默认 16MB，且可配置**：`SEKIRO_MAX_FRAME_MB` / `-max-frame-mb`
  统一驱动 native 帧、HTTP body 与 WS 读限制；业务大文本通常 <10MB，16MB 留约
  60% 余量，超过时调大配置即可。单帧可占用的内存峰值随之下降。

### 修复

- **监控页「最近事件」表格不再撑出卡片**：固定列宽并对长 `message` / `params`
  做省略号截断，窄屏（≤900px）自动隐藏优先级最低的参数与通道列；点击任意事件行
  可展开完整详情（原文与参数均可读），不再因为省略而丢信息。

## [v1.1.0] - 2026-11-19

发布镜像：`ghcr.io/luchatex/sekiro-open-go:1.1.0`（同时生成 `1.1`）。
以「连不上也看得见」为主的一个特性版本：起因是线上 js-client 报
`no device found for clientId:x`，而设备其实在线——只是注册在另一个 group；
服务端文案无法区分「设备在别组在线 / group 不存在 / 设备已掉线」，且连接侧
（握手、注册、断开）完全没有埋点，只能人工 curl 试探。

### 新增

- **应用层心跳**（WS 注册带 `hb=1`，增强客户端默认启用）：服务端先回一帧
  `{"__sekiro_hello__":1,"hb":10}`，之后客户端定期发 `{"__sekiro_hb__":<ts>}`
  并由服务端回显；连续 3 个周期没有任何下行帧即判定链路半开，主动丢弃旧连接
  重连。解决的是「浏览器没有 WS Ping API，provider 又是纯静默端」——半开链路上
  服务端 30s 内就判死并移出 group，页面却永远停在 `readyState=1`，业务侧只看到
  设备凭空消失。未声明 `hb=1` 的连接行为完全不变；新客户端遇到老服务端（不下发
  通告）自动降级为不发心跳。
- **连接侧统计**（`/op/api/stats` 的 `conn` 字段，监控页「客户端节点」右上角）：
  在线数、注册成功/失败（失败原因 `param-missing` / `accept-error`）、重复注册次数、
  连接终止次数与断开原因（`pong-timeout` / `read-idle` / `read-error` /
  `client-closed` / `bad-frame` / `bad-json` / `bad-seq`）。invoke 指标只覆盖
  「连上之后」，这一组补上「客户端到底连上没有」。

### 修复

- **`bind_client` 失败文案带归属**：错误信息保留 `no device found for clientId:x`
  前缀（兼容按前缀匹配的调用方），尾部追加三种成因的区分——该 clientId 在线于
  哪个 group（`device online in group "ws_list"`）、指定的 group 是否存在
  （`group "y" not found`）、设备是否刚掉线（`device disconnected 12s ago`，
  掉线记录保留 5 分钟）；同时在服务端打一条
  `bind_client miss: device online in another group` 的 Warn 日志。
- **真实断线日志此前是死代码**：读循环在 `tr.read` 返回错误后才判断
  `tr.Active()`，而 `read` 出错时已自行 `markDead`，该分支恒为假——生产 Info
  级别下「对端断开/网络中断」一条日志都不会有，只剩看护主动关闭的两条，排查时
  极易误判成「服务端无故踢人」。改为按「服务端是否主动关闭」判定，并区分
  `client-closed`（1000/1001/1005）与链路异常。
- **看护关闭导致的断线重复计数**：同一条连接被 `pong-timeout` 关闭后，读循环
  收到的读错误会被再计一次断线。现在服务端主动关闭会先登记原因，读循环只记
  一次善后日志，不重复计入统计。
- **监控页口径标注**：「系统错误分析」的累计值与「高频错误聚合」的 24h 窗口
  并排展示却无说明，容易把累计量当成当日故障量；现在分别标注「累计（自进程
  启动）」与「近 24h 共 N 条」。

### 调整

- **Ping 等待由 5s 放宽到 10s**（`protocol.SetServerPongTimeout` 可调）：该窗口包含
  「等写锁 + 对端处理 + 网络往返」，浏览器在页面繁忙或后台节流时回 Pong 可能延迟到
  秒级，5s 实测出现过对健康连接的误杀。判死总时长相应为「写空闲 20s + 10s = 30s」，
  仍是半分钟内发现。
- **心跳间隔建议值 15s → 10s**：客户端按 3 个周期判活，10s × 3 = 30s 与服务端判死
  对齐，消除「服务端已把设备移出 group、客户端还以为在线」的空窗。
- **后台标签页不判定心跳**：浏览器会节流后台页定时器，误判会把健康连接掐掉；
  切回前台立刻补发一次心跳并重置基准。显式配置的 `heartbeatInterval` 优先于服务端建议值。
- **连接侧新增「重复注册」计数**（`conn.duplicate`）：同 group 同 clientId 的新连接
  顶替旧连接；长期偏高即可判定客户端在重连风暴（页面重复实例化、被杀后立刻重连）。
- **`bind_client` 文案补掉线时长**：服务端保留 5 分钟内的掉线记录，错误消息可给出
  `(device disconnected 12s ago)`，而不是笼统的查无此设备；超过窗口不再提及。

## [v1.0.4] - 2026-11-03

发布镜像：`ghcr.io/luchatex/sekiro-open-go:1.0.4`（同时生成 `1.0`）。
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

- **迟到回包在解压/重组之前丢弃**：WS 通道在解析出 `__sekiro_seq__` 后、native
  通道在解压前先判定该序号是否仍在等响应，命中不到就不再「重新序列化整包」/
  「解压」——迟到的大响应（详情页 MB 级）此前要先完整处理一遍再丢弃。每次判定
  只多一次无竞争锁（纳秒级），换来的是完全不碰响应体。
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

[v1.1.2]: https://github.com/luchatex/sekiro-open-go/compare/v1.1.1...v1.1.2
[v1.1.1]: https://github.com/luchatex/sekiro-open-go/compare/v1.1.0...v1.1.1
[v1.1.0]: https://github.com/luchatex/sekiro-open-go/compare/v1.0.4...v1.1.0
[v1.0.4]: https://github.com/luchatex/sekiro-open-go/compare/v1.0.3...v1.0.4
[v1.0.3]: https://github.com/luchatex/sekiro-open-go/compare/v1.0.2...v1.0.3
[v1.0.2]: https://github.com/luchatex/sekiro-open-go/compare/v1.0.1...v1.0.2
[v1.0.1]: https://github.com/luchatex/sekiro-open-go/compare/v1.0.0...v1.0.1
[v1.0.0]: https://github.com/luchatex/sekiro-open-go/releases/tag/v1.0.0
