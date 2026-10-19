# 部署到 CentOS（Linux 静态二进制）

本文对应 [Releases 页面](https://github.com/luchatex/sekiro-open-go/releases) 发布的产物
（详见 [../release/README.md](../release/README.md)），
目标机为 CentOS 7/8/Stream、Rocky、Alma 等（x86_64 与 aarch64 均有产物；
**静态链接、无 glibc 版本依赖、无需安装任何运行时**）。

## 1. 产物清单

CentOS 取 **Linux x86_64** 三个文件即可（aarch64 机器改用 `-arm64` 版本）：

| 文件 | 平台 | 说明 |
|---|---|---|
| `sekiro-linux-amd64` | Linux x86_64 | 服务端主程序 |
| `sekiro-linux-arm64` | Linux aarch64 | 服务端主程序 |
| `loadtest-linux-amd64` | Linux x86_64 | 压测/自检工具（provider + invoke 双模式） |

Windows / macOS 产物同名提供（`sekiro-windows-amd64.exe`、`sekiro-darwin-arm64`）。
各版本完整校验值随 Release 附件 `SHA256SUMS.txt` 下发，下载后与本机二进制置于同一目录校验：

```bash
sha256sum -c SHA256SUMS.txt
# sekiro-linux-amd64: OK
# sekiro-linux-arm64: OK
# loadtest-linux-amd64: OK
```

构建方式（可复现）：`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o bin/sekiro-linux-amd64 ./cmd/sekiro`，
在 WSL Ubuntu 24.04 + Go 1.27.2 下原生编译并通过 `go test -race ./...` 与端到端冒烟。

## 2. 传输到目标机

```bash
# 从本机传到 CentOS（替换为用户与地址）
scp release/sekiro-linux-amd64 release/loadtest-linux-amd64 root@10.0.0.10:/opt/sekiro/
# 或先打包
tar czf sekiro-linux.tgz -C release sekiro-linux-amd64 loadtest-linux-amd64
```

## 3. 安装（systemd 托管）

```bash
# 3.1 目录与专用用户（不要用 root 跑）
useradd -r -s /sbin/nologin sekiro
mkdir -p /opt/sekiro /var/log/sekiro
mv sekiro-linux-amd64 /opt/sekiro/sekiro
chmod +x /opt/sekiro/sekiro          # DrvFs 拷贝会丢执行位，务必补
chown -R sekiro:sekiro /opt/sekiro /var/log/sekiro

# 3.2 systemd 单元
cat >/etc/systemd/system/sekiro.service <<'EOF'
[Unit]
Description=Sekiro Server (Go)
After=network.target

[Service]
Type=simple
User=sekiro
WorkingDirectory=/opt/sekiro
ExecStart=/opt/sekiro/sekiro -port 5612
Restart=on-failure
RestartSec=3
# 优雅关停：SIGTERM → 停止接入新连接、等待在途调用（最长 20s）后退出
KillSignal=SIGTERM
TimeoutStopSec=30
# 连接数较大时提高 fd 上限
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now sekiro
systemctl status sekiro
```

## 4. 配置方式（命令行 flag 或环境变量，二者等价）

| flag | 环境变量 | 默认 | 说明 |
|---|---|---|---|
| `-port 5612` | `SEKIRO_PORT` | `5612` | 监听端口（与原版一致） |
| `-debug` | `SEKIRO_LOCAL_DEBUG=true` | 关 | 调试级日志：额外打印完整调用参数（**含业务参数，生产慎用**） |
| `-log-level warn` | `SEKIRO_LOG_LEVEL=warn` | `info` | 日志级别 debug/info/warn/error。内部部署建议 warn：关掉逐笔调用 Info 日志（调用记录在 /op 监控页仍可查），日志量降到近零增长 |
| `-op-disable` | `SEKIRO_OP_DISABLE=true` | 关 | 关闭 `/op` 监控页与 `/op/api/stats` |
| `-op-password xxx` | `SEKIRO_OP_PASSWORD=xxx` | 空 | `/op` 监控路径访问密码：浏览器登录（12h 会话），连续输错 3 次锁定来源 IP 2 分钟。空 = 无鉴权；systemd 部署建议写进 `Environment=` 而非 ExecStart（避免 `ps` 可见明文密码） |

示例：`SEKIRO_PORT=5612 SEKIRO_LOG_LEVEL=warn /opt/sekiro/sekiro`。
默认（非 debug）只记 `group`/`action`，业务参数不落盘。

### 日志管理

- **推荐 systemd + journald**（第 3 节 unit 已用 journal 输出）：日志自动轮转，
  在 `/etc/systemd/journald.conf.d/sekiro.conf` 设 `SystemMaxUse=200M`、
  `MaxRetentionSec=14day` 限总量与保留期；查看用 `journalctl -u sekiro -f`。
  叠加 `-log-level warn` 后基本无需担心日志体积
- **若用 `nohup ... | tee app.log &`**：进程持有 fd，直接 mv 无效，需 logrotate
  `copytruncate`（`/etc/logrotate.d/sekiro`：`daily` + `maxsize 100M` +
  `rotate 7` + `compress`）；且 nohup 无自动拉起，建议尽早切 systemd

### 断开重连排查（WS provider）

出现大量 `duplicate client register` warn 时，看两个特征值即可定位死因：

| 特征 | 结论 | 处置 |
|---|---|---|
| `oldLifetime` 周期性 ≈ 30s，且伴随 `reason=read-idle` | 旧版二进制的读空闲误杀指纹 | 升级到基线 `12b933f` 之后的产物（心跳 pong 保活） |
| 周期性 ≈ 25s，伴随 `reason=pong-timeout` | 对端不回 pong（客户端库不回、或中间代理剥 ping） | 检查客户端所在网络链路；浏览器/标准 ws 库均自动回 pong |
| `oldLifetime` 无规律、伴随 `reason=read-error` | 页面刷新/客户端主动断开，属正常行为 | 无需处理 |

正常情况下每个连接的生命周期只有两条日志：`ws client registered`（INFO）
+ 断开时的一条死因日志（INFO）。

### nginx 前置（https/wss）与 WS 压缩

nginx 反代 wss 时 `proxy_http_version 1.1` 与 Upgrade/Connection 头是基本配置；
**`Sec-WebSocket-Extensions` 默认透传**，permessage-deflate 端到端协商成功。
验证方式（基线 `6c1756e`+）：

- `/op` 监控页「客户端节点」：通道徽标旁应为绿色 `deflate`；
  若显示黄色 `off`，说明压缩静默关闭（大文本带宽 ~10 倍放大）
- 注册日志：`ws client registered ... compression=deflate/off`

若为 `off` 而客户端确实声明了扩展，检查 nginx 是否有 `proxy_set_header`
把 `Sec-WebSocket-Extensions` 清空/覆盖的配置；浏览器与 Node 标准 ws 库
默认都会声明 permessage-deflate。

## 5. 防火墙与安全

```bash
# firewalld
firewall-cmd --permanent --add-port=5612/tcp && firewall-cmd --reload
# 或仅对内网来源开放
firewall-cmd --permanent --add-rich-rule='rule family="ipv4" source address="10.0.0.0/8" port port="5612" protocol="tcp" accept' && firewall-cmd --reload
```

- SELinux：二进制放在 `/opt/sekiro` 且只监听高位端口，Enforcing 下通常无需额外策略；
  若改为 80/443 等受管端口，需 `semanage port -a -t http_port_t -p tcp 5612`（按需）。
- **无鉴权**：与开源版一致，`/business/invoke` 无 token 校验。公网部署务必限制来源 IP，
  并加 `-op-disable` 关闭监控页；如需鉴权请在前面挂网关/反向代理。

## 6. 上线自检

```bash
# 6.1 服务在跑
curl -s http://127.0.0.1:5612/business/groupList
# {"data":[],"ok":true,"status":0}

# 6.2 自带压测工具：先起 3 个 provider，再打 2000 请求
./loadtest-linux-amd64 -mode provider -server 127.0.0.1:5612 -group smoke -clients 3 &
curl -s "http://127.0.0.1:5612/business/clientQueue?group=smoke"
# {"data":["load-0","load-1","load-2"],"ok":true,"status":0}
./loadtest-linux-amd64 -mode invoke -url http://127.0.0.1:5612 -group smoke -c 20 -n 2000

# 6.3 空池（未注册 provider）应快速失败
curl -s "http://127.0.0.1:5612/business/invoke?group=nobody&action=a"
# {"message":"pool queue empty","ok":false,"seq":0,"status":-1}

# 6.4 监控页（未加 -op-disable 时）
curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:5612/op   # 200
```

参考基线（WSL/Windows 本机实测，非目标机数据）：20 并发 2000 请求约 **3300 req/s**，
p50 5.4ms / p99 14.5ms，0 错误。

## 7. 运维要点

- **优雅关停**：`systemctl stop sekiro` 发 SIGTERM → 日志出现
  `shutdown signal received, draining connections` → 等待在途调用回包（上限 20s）→
  `sekiro server(go) stopped gracefully`。直接 `kill -9` 会打断在途的 15s 长轮询调用。
- **日志**：JSON 结构化输出到 stdout，`journalctl -u sekiro -f` 查看。
- **客户端接入**：端口 5612；WebSocket provider 注册
  `ws://<host>:5612/register?group=<g>&clientId=<id>`；HTTP 调用
  `/business/invoke?group=<g>&action=<a>&...`；二进制 invoker 走原生协议同端口复用。
- **fd 上限**：大规模连接时确认 `ulimit -n`（service 里已设 65535）。
- **回滚**：替换 `/opt/sekiro/sekiro` 二进制后 `systemctl restart sekiro` 即可，无状态、无外部依赖。

## 附：Windows 上跑同一份产物

```powershell
# 控制台直跑
.\sekiro-windows-amd64.exe -port 5612

# 注册为服务（推荐 NSSM；或用 sc.exe + wrapper）
nssm install sekiro "C:\opt\sekiro\sekiro-windows-amd64.exe" "-port 5612"
nssm set sekiro AppDirectory C:\opt\sekiro
nssm start sekiro
```

- 关停用 `nssm stop sekiro`（等价 SIGTERM 语义的 Ctrl+C），避免强杀打断在途 15s 长轮询调用。
- 客户端接入端口与 API 路径与 Linux 完全一致（5612、`/business/invoke`、`/register`）。
- 本机实测：10 并发 1000 请求 0 错误，约 3651 req/s，p50 2.0ms / p99 9.0ms。

## 8. 兼容性说明

- 静态链接（`CGO_ENABLED=0`），**不依赖目标机 glibc 版本**；内核 ≥ 3.2 即可
  （CentOS 7/8/9、Rocky、Alma 均满足；CentOS 6 的 2.6.32 不建议）。
- 与原 Java 版线上协议字节级兼容，客户端 SDK（Java/JS/Python）零改动接入。
- 时区：日志时间戳按本机时区；`/op` 的"天"序列按服务端本地时区聚合。
