# WSS / TLS 部署指南

sekiro-go 服务端保持明文单端口（协议探测架构，见 go/README.md），
TLS 一律由反代/隧道层终结，服务端代码零改动。

```
浏览器(https) ──wss://域名──▶ [TLS 终结层] ──ws/http──▶ sekiro-go(:5612)
```

## 方案选型

| 方案 | 证书 | 适用场景 | 配置文件 |
|---|---|---|---|
| Caddy 反代 | 自动申请续期（Let's Encrypt） | 公网生产，最省事 | `Caddyfile` |
| Nginx 反代 | 自备（免费 DV 或 Let's Encrypt） | 已有 Nginx | `nginx-sekiro.conf` |
| Cloudflare Tunnel | **无需证书、无需公网 IP** | 家庭/内网服务端上公网 | 见下 |
| `ws://127.0.0.1` 豁免 | **无需证书** | https 业务页 + 本机 provider | 无需配置 |

> ⚠️ 自签证书 + 浏览器「继续前往」对 **wss 无效**（页面级例外不覆盖
> WebSocket）。内网自签请用 [mkcert](https://github.com/FiloSottile/mkcert)
> 将本地 CA 装入系统信任库。

## Caddy（推荐）

```bash
# Caddyfile 已就绪，域名解析到本机后：
caddy run --config deploy/Caddyfile
```

自动 HTTPS、自动续期、WebSocket 升级透明支持。已包含 `/op` 监控页的
Basic Auth 示例（默认注释，按需开启）——v1.0.0 起也可改用服务端内置的
`-op-password`（带防爆破锁定），nginx Basic Auth 与其取其一即可。

## Nginx

```nginx
include /etc/nginx/conf.d/nginx-sekiro.conf;
# 证书路径按实际调整；/op 段含 IP 白名单与 Basic Auth 示例（默认注释）
```

配置包含两段必须保留的内容：

1. **upstream 连接池**：nginx → sekiro 复用 HTTP/1.1 长连接，减少后端侧
   `TIME_WAIT` 与短连接握手开销。

```nginx
upstream sekiro_backend {
    server 127.0.0.1:5612;
    keepalive 64;              # 每个 worker 的空闲连接数，不是全局总数
    keepalive_requests 1000;   # 单连接最多复用多少次
    keepalive_timeout 60s;     # 空闲连接在池中的保留时间
}
```

2. **按 Upgrade 头条件设置 Connection**：普通 HTTP 必须发送空的
   `Connection` 头，keepalive 才会生效；WebSocket 握手仍需要
   `Connection: upgrade`。不能对所有请求固定写 `Connection "upgrade"`。

```nginx
map $http_upgrade $connection_upgrade {
    default upgrade;
    ''      '';
}

proxy_http_version 1.1;
proxy_set_header Upgrade $http_upgrade;
proxy_set_header Connection $connection_upgrade;
```

关键点：

- `proxy_pass` 必须指向 upstream 名称（`http://sekiro_backend`）；直接写
  `http://127.0.0.1:5612` 会绕过连接池。
- `/op` 是普通 HTTP，使用 `proxy_http_version 1.1` +
  `proxy_set_header Connection ""`，不需要 Upgrade 头。
- `keepalive` 按 worker 分别计算；4 个 worker 配 64，理论最多保留
  256 条空闲连接。按 QPS 和后端连接数在 32–128 间调整即可。
- `/op` 登录锁定只在直连来源为回环/私网/链路本地地址时信任
  `X-Real-IP`/`X-Forwarded-For`；本配置的 nginx 与 sekiro 同机或同私网，
  并覆写 `X-Real-IP $remote_addr`，可正确按真实客户端 IP 限流。
- `proxy_socket_keepalive on` 启用 TCP keepalive，用来发现被防火墙或
  后端静默丢弃的死连接，与 HTTP 连接池不是同一概念。
- 这里减少的是 **nginx → sekiro** 的 `TIME_WAIT`。如果 `TIME_WAIT`
  大量出现在客户端 → nginx，需要让 HTTP 调用方使用连接池，并检查
  客户端是否发送了 `Connection: close`。
- 修改后先执行 `nginx -t`，再 `nginx -s reload`。

核对连接是否真正复用：

```bash
# 观察 nginx worker 与 5612 之间的 ESTABLISHED 数
ss -tanp '( sport = :5612 )' | grep -c ESTAB

# TIME_WAIT 应随连接池生效而下降，而不是立刻归零
ss -tan state time-wait '( dport = :5612 )' | wc -l
```

## Docker 资源限制与日志轮转

仓库根目录的 `docker-compose.yml` 已配置：

```yaml
mem_limit: "${SEKIRO_MEM_LIMIT:-4g}"
memswap_limit: "${SEKIRO_MEM_LIMIT:-4g}"
environment:
  GOMEMLIMIT: "${GOMEMLIMIT:-3GiB}"
  GOGC: "${GOGC:-100}"
logging:
  driver: json-file
  options:
    max-size: "50m"
    max-file: "3"
```

可在根目录 `.env` 覆盖：

```dotenv
SEKIRO_MEM_LIMIT=4g
GOMEMLIMIT=3GiB
GOGC=100
```

参数含义和取舍：

| 配置 | 作用 | 建议 |
|---|---|---|
| `mem_limit=4g` | cgroup 硬限制，超过会被 OOM kill，保护宿主机 | 按宿主机余量调整；这是保险，不是内存优化 |
| `memswap_limit=4g` | 与 memory 相等，禁止 swap 掩盖内存问题 | 保持相等，避免换页拖慢整个服务 |
| `GOMEMLIMIT=3GiB` | Go GC 的**软目标**，接近时更积极回收 | 取容器限制的 75%–90%，给栈、runtime、socket 缓冲留余量 |
| `GOGC=100` | Go 默认 GC 频率 | 先保持 100；没有泄漏且 CPU 富余时才试 75，50 会增加显著 GC CPU |
| `max-size=50m` + `max-file=3` | json-file 单文件与份数上限 | 总上限约 150MB，比 `100m×3` 更保守 |

如果不用 compose，必须显式传入等价参数：

```bash
docker run -d --name sekiro \
  --memory=4g --memory-swap=4g \
  --log-driver=json-file --log-opt max-size=50m --log-opt max-file=3 \
  -e GOMEMLIMIT=3GiB -e GOGC=100 \
  -p 5612:5612 --restart unless-stopped \
  ghcr.io/luchatex/sekiro-open-go:1.0.1
```

注意事项：

- `GOMEMLIMIT` 不是硬限制，不会阻止分配，也不能修复泄漏。live heap
  超过限制时，进程仍会继续增长直到被 cgroup OOM kill。
- 不要把 `GOMEMLIMIT` 设得远低于实际活跃堆，否则会持续高频 GC、增加延迟和 CPU。
- `GOGC=50` 会让 GC 更频繁，典型代价是 CPU 上升；不要和“修复泄漏”混为一谈。
- 建议先部署修复版并重启，观察 24 小时实际 RSS，再决定是否把限制从 4GB 收紧。
- 容器退出码 `137` 通常表示被 OOM kill：

```bash
# compose 容器名为 sekiro-go；docker run 示例中为 sekiro，按实际名称替换
docker inspect -f '{{.State.OOMKilled}} {{.State.ExitCode}}' sekiro
docker stats sekiro
```

- 若使用宿主机 systemd 而非 Docker：在 unit 中设置
  `MemoryMax=4G`、`MemorySwapMax=0`，并设置
  `Environment=GOMEMLIMIT=3GiB`、`Environment=GOGC=100`；
  日志由 journald 的 `SystemMaxUse` 限制。

## Cloudflare Tunnel（零证书、零公网 IP）

```bash
# 快速试用（临时域名）
cloudflared tunnel --url http://127.0.0.1:5612
# → https://xxx.trycloudflare.com  页面里 wss://xxx.trycloudflare.com

# 正式使用：命名隧道 + 自己的域名（域名托管在 Cloudflare）
cloudflared tunnel create sekiro
cloudflared tunnel route dns sekiro sekiro.example.com
cloudflared tunnel run sekiro
```

- TLS 终结在 CF 边缘，源站只有出站连接
- 免费套餐支持 WebSocket；空闲超时 100s < sekiro 心跳间隔，不受影响
- 大陆访问速度一般；国内替代：腾讯云 EdgeOne / CLB（同样支持 WS 透传）

## 本机 wss + 自签证书（✅ 实测可行，https 页面直连本机 provider）

**Playwright 实测结论**：浏览器 LNA（Local Network Access）只拦**明文
`ws://`**，对加密目标 `wss://127.0.0.1` **不拦截**（实测错误码为
SSL 层而非 LNA 层）。因此本地起 wss + 自签证书（CA 导入系统信任库）
即可让 https 业务页面直连本机 provider，无需公网域名与真实证书。

### 步骤（Windows，5 分钟）

```bash
# 1. 生成自签 CA + 服务端证书（SAN 含 127.0.0.1/localhost）
openssl req -x509 -newkey rsa:2048 -keyout ca.key -out ca.crt -days 825 -nodes -subj "/CN=sekiro-local-dev-ca"
openssl req -newkey rsa:2048 -keyout server.key -out server.csr -nodes -subj "/CN=127.0.0.1"
printf "subjectAltName=IP:127.0.0.1,IP:::1,DNS:localhost" > san.cnf
openssl x509 -req -in server.csr -CA ca.crt -CAkey ca.key -CAcreateserial -out server.crt -days 825 -extfile san.cnf

# 2. CA 导入当前用户信任库（免管理员；或用 mkcert 一键替代本两步）
certutil -user -addstore Root ca.crt

# 3. 服务端启用 TLS 监听（与明文端口并存）
sekiro --port 5612 --tls-cert server.crt --tls-key server.key --tls-port 5613

# 4. https 业务页面直连
#    new SekiroClient('wss://127.0.0.1:5613', { group: 'demoGroup', clientId: 'web-1' })
```

- TLS 端口与明文端口同构：TLS 解密后走同一协议探测，native 二进制
  provider 也可走 wss 端口
- 自签 CA 只导入本机信任库；分发给他人设备需对方同样导入（或直接用
  mkcert：`mkcert -install && mkcert 127.0.0.1 localhost`）
- 集成测试：`internal/server/tls_test.go`（TLS 下 HTTP API + WS 注册）

### 顺带修复：跨源 WS 握手 403

实验中发现 coder/websocket 默认拒绝一切跨源 Origin（回 403），导致
https 页面注册 provider 必然失败——已按 Java 原版语义放行
（`OriginPatterns: ["*"]`，sekiro provider 本就设计为从任意业务页面注册）。

## 客户端接入（唯一需要的业务改动）

```js
// 生产（Caddy/Nginx/CF Tunnel 任一方案）
new SekiroClient("demoGroup", "my-client-1", "wss://sekiro.example.com", 443);

// 本机 provider（localhost 豁免）
new SekiroClient("demoGroup", "my-client-1", "ws://127.0.0.1", 5612);
```

HTTP API 同理：`https://sekiro.example.com/business/invoke?...`。

## 其他注意

- WSS 下 permessage-deflate 压缩照常协商生效（TLS 隧道内）
- `/op` 监控页鉴权二选一：v1.0.0+ 服务端内置 `-op-password`（登录会话 +
  连续 3 次失败锁 IP 2 分钟），或沿用本目录 nginx 的 Basic Auth/IP 白名单段；
  二者取其一即可，都不启用时页面无鉴权
- 服务端不实现原生 TLS 的原因：单端口按首包字节探测协议，TLS 握手
  首字节 0x16 会被判未知协议拒绝；原生 TLS 需要独立监听模式且要求
  所有 native provider 支持 TLS，收益不成比例
