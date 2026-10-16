import cn.iinti.sekiro3.business.api.SekiroClient;
import cn.iinti.sekiro3.business.api.interfaze.ActionHandler;
import cn.iinti.sekiro3.business.api.interfaze.HandlerRegistry;
import cn.iinti.sekiro3.business.api.interfaze.SekiroRequest;
import cn.iinti.sekiro3.business.api.interfaze.SekiroRequestInitializer;
import cn.iinti.sekiro3.business.api.interfaze.SekiroResponse;

import java.io.ByteArrayInputStream;
import java.nio.charset.StandardCharsets;
import java.util.concurrent.CountDownLatch;

/**
 * 真实 sekiro Java 客户端联调 demo（官方 SekiroClient API）。
 *
 * 用法：java DemoClient [host] [port] [clientId] [group]
 *
 * 说明：SekiroClient 4 参构造器参数顺序为 (sekiroGroup, clientId, host, port)
 * （经 javap 字节码核实：aload_1→sekiroGroup, aload_2→clientId）。
 * action 路由通过实现 ActionHandler 接口（action() 方法）注册。
 *
 * 提供的 action：
 *   hello    → success("你好,来自真实Java客户端: " + name)
 *   echo     → success(请求参数 JSON 原文)
 *   large    → success(100KB 中英混合文本，验证大载荷与协议压缩)
 *   segment  → 分 3 段流式返回（验证分段流 chunked）
 */
public class DemoClient {
    public static void main(String[] args) throws Exception {
        String host = args.length > 0 ? args[0] : "127.0.0.1";
        int port = args.length > 1 ? Integer.parseInt(args[1]) : 5613;
        String clientId = args.length > 2 ? args[2] : "demo-client-1";
        String group = args.length > 3 ? args[3] : "demoGroup";

        SekiroClient client = new SekiroClient(group, clientId, host, port);
        client.setupSekiroRequestInitializer(new SekiroRequestInitializer() {
            @Override
            public void onSekiroRequest(SekiroRequest request, HandlerRegistry registry) {
                registry.registerSekiroHandler(new ActionHandler() {
                    @Override
                    public String action() { return "hello"; }

                    @Override
                    public void handleRequest(SekiroRequest req, SekiroResponse resp) {
                        resp.success("你好,来自真实Java客户端: " + req.getString("name"));
                    }
                });
                registry.registerSekiroHandler(new ActionHandler() {
                    @Override
                    public String action() { return "echo"; }

                    @Override
                    public void handleRequest(SekiroRequest req, SekiroResponse resp) {
                        resp.success(req.getParamContent());
                    }
                });
                registry.registerSekiroHandler(new ActionHandler() {
                    @Override
                    public String action() { return "large"; }

                    @Override
                    public void handleRequest(SekiroRequest req, SekiroResponse resp) {
                        StringBuilder sb = new StringBuilder();
                        String unit = "中文ABC测试数据,";
                        int unitBytes = unit.getBytes(StandardCharsets.UTF_8).length;
                        for (int i = 0; i < 100 * 1024 / unitBytes; i++) {
                            sb.append(unit);
                        }
                        resp.success(sb.toString());
                    }
                });
                registry.registerSekiroHandler(new ActionHandler() {
                    @Override
                    public String action() { return "segment"; }

                    @Override
                    public void handleRequest(SekiroRequest req, SekiroResponse resp) {
                        // 分段流入口：send(InputStream)，SDK 内部按段发出
                        // SEGMENT_STREAM_FLAG 包（连续 send(Object) 会被 CAS 拒绝）
                        String body = "第一段-中文;\n第二段-ABC;\n第三段-混合123;\n";
                        resp.send(new ByteArrayInputStream(body.getBytes(StandardCharsets.UTF_8)));
                    }
                });
            }
        });
        client.start();
        System.out.println("[DemoClient] started clientId=" + clientId + " group=" + group
                + " -> " + host + ":" + port);
        new CountDownLatch(1).await(); // 常驻
    }
}
