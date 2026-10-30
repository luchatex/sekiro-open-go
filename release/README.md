# 发布产物

**产物已由 GitHub Actions 自动发布**：推送 `v*` 标签即触发全平台构建
（全量测试 → 冒烟自检 → Release），二进制与 `SHA256SUMS.txt` 直接到
[Releases 页面](https://github.com/luchatex/sekiro-open-go/releases)下载，
不再本地手工出包。

```bash
git tag v1.0.1 && git push origin v1.0.1   # 自动构建并发布
```

本地构建（调试/自用，非发布途径）：

```bash
cd go
export CGO_ENABLED=0
GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o sekiro ./cmd/sekiro
# GOOS/GOARCH 按需切换：linux/arm64、windows/amd64、darwin/arm64
```

部署与自检方法见 [deploy/](../deploy/) 与 [go/README.md](../go/README.md)。

## v1.0.1（2026-10-30）

缺陷修复版本：

- 修复断线/重连 Client 残留、未知 group 创建永久 actor、取消不生效等内存与
  goroutine 生命周期问题；空池退避与二进制 invoker 现可被调用方取消。
- 修复断线清理阻塞 group actor、`attached` 定时器竞态、空分段 pending 残留、
  HTTP 响应与 gzip 关闭竞态。
- 加固 gzip/snappy 解压预算：不再按不可信 gzip ISIZE 预分配；OP 登录/kick
  增加 body 读超时、JSON Content-Type 校验、可信代理 IP 判断和 TLS Secure Cookie。
- nginx 示例启用 upstream keepalive，修正 `/op` upstream 引用与重复 location。
- Docker 编排增加可配置内存硬限制、`GOMEMLIMIT`/`GOGC` 基线和 json-file
  日志轮转；`docker run` 示例同步对应参数。

## v1.0.0

首个正式版本，涵盖：

- 与原 Java 版兼容的线上协议（HTTP / WebSocket / native 三通道）
- 监控报表 `/op`（含 `-op-password` 登录保护）
- 出流量按压缩后实际字节统计
- 修复原版 WebSocket 分片场景下的 UTF-8 解码乱码

单静态二进制、无 JVM 依赖。各平台产物与校验清单 `SHA256SUMS.txt` 见
[Releases 页面](https://github.com/luchatex/sekiro-open-go/releases)。

后续版本沿用语义化版本：缺陷修复升 patch（`v1.0.1`），新增特性升 minor
（`v1.1.0`）。
