# 真实 sekiro Java 客户端联调

`DemoClient.java` 使用**官方 SekiroClient API**（私有 jar `sekiro-business-api-1.0.jar`，
与生产客户端同一实现）对 Go 版服务端做端到端联调。

## 准备

jar 放入 `lib/`（不入库，见 .gitignore）：

```
sekiro-business-api.jar   # 官方客户端 API + 协议实现（nexus.iinti.cn）
sekiro-netty.jar / sekiro-netty-core.jar   # 重打包 Netty
snappy-java.jar / jctools.jar              # 传递依赖
```

## 运行

目录布局：Java demo 在 `java/`，Python 脚本在 `python/`，官方 jar 在 `lib/`（不入库，见 .gitignore）。
以下命令均在 `tests/` 目录执行。

```bash
# 1. 编译
javac -encoding UTF-8 -cp "lib/sekiro-business-api.jar;lib/sekiro-netty.jar;lib/sekiro-netty-core.jar" -d classes java/DemoClient.java

# 2. 启动 Go 服务端（在仓库 go/ 目录构建）
cd ../../go && go build -o ../tests/server.exe ./cmd/sekiro && cd ../tests
./server.exe --port 5620

# 3. 启动真实客户端（常驻）
java -Dfile.encoding=UTF-8 -cp "classes;lib/*" DemoClient 127.0.0.1 5620 demo-client-1 demoGroup

# 4. 通过 HTTP API 调用
curl "http://127.0.0.1:5620/business/invoke?group=demoGroup&action=hello&name=sekiro"
```

## 提供的 action

| action | 返回 | 验证点 |
|---|---|---|
| hello | `你好,来自真实Java客户端: {name}` | 中文 UTF-8 全链路（粘包乱码根治验证） |
| echo | 请求参数 JSON 原文 | fast-json 信封、参数透传 |
| large | 100KB 中英混合文本 | 大载荷 + 协议帧层压缩协商（gzip/snappy） |
| segment | 三段流式文本 | 分段流（SDK `send(InputStream)` 内部发 SEGMENT_STREAM_FLAG 包） |

## 官方 API 关键语义（javap 字节码核实）

- `SekiroClient` 4 参构造器参数顺序是 **(sekiroGroup, clientId, host, port)**
  （`aload_1→sekiroGroup, aload_2→clientId`），与直觉相反。
- action 路由：实现 `ActionHandler` 接口（`action()` 方法），或 `@Action("x")` 注解；
  纯 `RequestHandler` 注册会抛 `can not resolve handler action`。
- `SekiroResponse.send(Object)` 为一次性语义（CAS 锁），连续调用被拒；
  **分段流入口是 `send(InputStream)` / `send(byte[])`**，SDK 内部按段发出
  `SEGMENT_STREAM_FLAG=true` 包。

## 已验证结论（2026-10-12）

- 注册/心跳/离线：真实客户端注册成功，长连接心跳稳定（数分钟无断连无重连）
- HTTP invoke 全矩阵通过，中文无乱码
- 100KB 载荷经协议帧压缩协商后完整往返（102452 字节）
- 分段流经真实 SDK InputStream 路径完整到达 HTTP chunked 出口
- 失败路径 status 为有符号 int32（-1），message/data null 语义正确

联调过程中发现并修复 Go 版两个真实缺陷（均有回归测试）：

1. `Record.deliver` 正常响应后不终结 Record：超时定时器空转 15s 后二次写响应，
   gzip 场景会把未压缩数据写进已关闭的压缩流（`internal/group/record_finish_test.go`）。
2. fast-json 信封 status 按 uint32 解码：Java `int` 负值（如 -2）显示为 4294967294
   （`internal/httpapi/http.go` 按 int32 有符号解码）。
