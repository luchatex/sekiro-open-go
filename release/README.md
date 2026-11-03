# 发布产物

**产物已由 GitHub Actions 自动发布**：推送 `v*` 标签即触发全平台构建
（全量测试 → 冒烟自检 → Release），二进制与 `SHA256SUMS.txt` 直接到
[Releases 页面](https://github.com/luchatex/sekiro-open-go/releases)下载，
不再本地手工出包。

```bash
git tag v1.0.4 && git push origin v1.0.4   # 自动构建并发布
```

本地构建（调试/自用，非发布途径）：

```bash
cd go
export CGO_ENABLED=0
GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o sekiro ./cmd/sekiro
# GOOS/GOARCH 按需切换：linux/arm64、windows/amd64、darwin/arm64
```

部署与自检方法见 [deploy/](../deploy/) 与 [go/README.md](../go/README.md)。

## 历史版本

各版本变更明细（含镜像标签与修复清单）统一维护在
[CHANGELOG.md](../CHANGELOG.md)，此处不再重复，避免两处记录漂移。

版本号沿用语义化版本：缺陷修复升 patch（`v1.0.4`），新增特性升 minor（`v1.1.0`）。
