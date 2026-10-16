# sekiro-samples 三语言客户端兼容性验证

使用官方示例仓库 https://github.com/yint-tech/sekiro-samples 验证
Go 版服务端（`tests` 本地脚本连接 127.0.0.1:5620）。

## 验证结果（2026-10-12）

| 客户端 | 来源 | 连接目标 | invoke 结果 |
|---|---|---|---|
| Java（官方 SDK Demo1） | demo-java | Go :5620 | ✅ `data={"age":10,"name":"test","request":"hello-java"}` |
| Go（官方 demo-go client） | demo-go | Go :5620 | ✅ `data="ok-go"` |
| Python SekiroClient | demo-python | Go :5620 | ✅ `data="ok-python"` |
| Python SekiroInvoker（二进制通道） | demo-python | Go :5620 | ⚠️ 原版报 JSONDecodeError（见下），**补丁后 ✅** `data="ok-python"` |

对照组（原版 Java 服务端 :5621）：Python SekiroInvoker 原版**同样报
JSONDecodeError**——sekiro-samples 自身缺陷，非 Go 版兼容性问题。

## 已知 samples 缺陷

`SekiroInvoker._decode_sekiro_fast_json` 将 `SEKIRO_CLIENT_ID` 响应头值**裸拼**进
JSON（`{"clientId":python-samples,...}` 非法），而 Java/Go 服务端均注入裸 clientId。
`SekiroInvokerPatched.py` 为头值补 JSON 引号后两端均正常，且 Go/Java 返回数据一致。

## 本地验证脚本

（Java demo 在 `java/`，Python 在 `python/`，大载荷 JS 在 `tests/js/`）

- `java/Demo1Local.java`：demo-java Demo1 本地化（group=test-java）
- `python/SekiroClientLocal.py`：demo-python client 本地化（group=test-python，需 tornado）
- `python/SekiroInvokerLocal.py` / `python/SekiroInvokerPatched.py`：demo-python invoker 本地化（含补丁版）
- `java/DemoProviderLocal.java`：参数化 provider（shadow 对拍用）
- Go demo-go：在 samples 仓库内 `sekio/local_main.go + cmd/samples/main.go`
  （handleRequest 未导出，实现须置于 sekio 包内），`go build ./cmd/samples`

## 复现

```bash
# 1. 启动 Go 服务端（在仓库 go/ 目录执行）
go build -o server.exe ./cmd/sekiro && ./server.exe --port 5620
# 2. 启动任一 samples 客户端（见上）
# 3. HTTP 调用
curl "http://127.0.0.1:5620/business/invoke?group=test-java&action=testAction&param=x"
```

## demo-web（JS/WebSocket）3MB 中文大载荷测试（2026-10-12）

`ws_big_payload.js`：加载官方 `sekiroWeb.js`（仅加 window shim 适配 Node 22 原生
WebSocket，官方代码零改动），provider 经 WS 通道响应 3MB 中文（1,048,576 汉字 =
3,145,728 UTF-8 字节），HTTP invoke 往返校验。

| 服务端 | 结果 |
|---|---|
| **Go 版 :5620** | **10/10 PASS**：U+FFFD=0，SHA256 与原始载荷逐字节一致，每轮约 100ms |
| Java 版 :5621（对照） | 10/10 PASS（单帧发送不触发其分帧缺陷；Java 版乱码仅出现在 WS 分帧场景——D12 实验实证 36/64 切分点乱码，Go 版 E2E 已用 75/75 全切分点证明根治） |

```bash
node tests/js/ws_big_payload.js 5620   # Go 版
node tests/js/ws_big_payload.js 5621   # Java 对照
```

## WS 分帧乱码：Java 版本地补丁验证（2026-10-12，D12 延伸）

`ws_fragment_test.js`：原生 TCP 手工构造 WebSocket 帧，把 3MB 中文 provider 响应按
8 个字节切分点切成 Text(FIN=0)+Continuation(FIN=1) 两帧（官方客户端无法控制分帧，
故需手工帧）。Java 侧修补（ServiceWsClient 字节级聚合，PATCH-01）随原仓库参考副本移除，此处保留当时验证结论。

| 服务端（:5621） | 结果 |
|---|---|
| Java 原版（未修补） | **2/16 PASS**：切分点落在汉字内部时 U+FFFD=2~3 |
| Java 修补版（LOCAL-PATCHES PATCH-01） | **16/16 PASS**：U+FFFD=0，SHA256 一致 |
| Java 修补版 + 官方 sekiroWeb.js 3MB（ws_big_payload.js） | 10/10 PASS（不分帧路径无回归） |

```bash
node tests/js/ws_fragment_test.js 5621   # Java（修补版）
node tests/js/ws_fragment_test.js 5620   # Go 版（预期 16/16）
```

## 原版 fastjson 序列化行为实证（2026-10-17，响应格式对齐依据）

用 tests/lib 私有 jar（sekiro-business-api.jar 内置 shaded fastjson）+ JDK 21
驱动原版代码实证（探针源码见本节末），产出对齐 `orderedjson` 包的黄金断言
（go/internal/orderedjson/fastjson_align_test.go）：

| 行为 | 原版 fastjson 实证输出 |
|---|---|
| HTML 字符 `<` `>` `&` | **不转义**（Go `json.Marshal` 默认转义 `\u003c`，爬虫 HTML 大文本必受影响） |
| 斜杠 `/`、单引号 `'` | 不转义 |
| 中文 | raw UTF-8 原样输出 |
| 键序（toJSONString 默认） | **递归字典序**（SortField 为 DEFAULT_GENERATE_FEATURE，嵌套对象/数组内对象均排序，FjProbe3 实证） |
| 控制字符 | `\b \f \n \r \t` 短形式；其余 `<0x20` 及 `0x7F` → `\u00XX` 十六进制大写 |
| 数字字面量 | 保留原文（`1.50` 输出 `1.50`） |
| fast-json 分支（quickJsonDecode） | 固定序 `{"clientId":..,"message":..,"status":<int>,"data":<原样嵌入>}`，data 为 provider 侧序列化结果不再重组 |

据此对齐的 Go 侧路径：once-call 响应（MarshalJSONSorted）、fast-json 信封
clientId/message 转义、invoke 参数下发（native+WS，MarshalJSONSorted，
`__sekiro_seq__` 排序后居首）。

```java
// /tmp/fjtest/FjProbe3.java（节选）——javac -cp sekiro-business-api.jar
Object o = JSON.parse("{\"zz\":{\"m2\":1,\"a1\":\"<b>&x</b>\",\"kk\":[{\"y\":2,\"b\":3}]},\"aa\":1.50,\"mM\":\"http://e.com/p?q=1&z=2\"}");
jo.putAll((JSONObject) o); jo.put("clientId", "dev-1");
// → {"aa":1.50,"clientId":"dev-1","mM":"http://e.com/p?q=1&z=2","zz":{"a1":"<b>&x</b>","kk":[{"b":3,"y":2}],"m2":1}}
```
