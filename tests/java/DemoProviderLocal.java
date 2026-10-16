import cn.iinti.sekiro3.business.api.SekiroClient;
import cn.iinti.sekiro3.business.api.interfaze.ActionHandler;
import cn.iinti.sekiro3.business.api.interfaze.SekiroResponse;

/**
 * 参数化 provider：为指定 group 提供 testAction，用于 shadow 对拍。
 * 用法：java DemoProviderLocal <group> <clientId> <host> <port>
 */
public class DemoProviderLocal {
    public static void main(String[] args) throws InterruptedException {
        String group = args.length > 0 ? args[0] : "test-python";
        String clientId = args.length > 1 ? args[1] : "java-provider";
        String host = args.length > 2 ? args[2] : "127.0.0.1";
        int port = args.length > 3 ? Integer.parseInt(args[3]) : 5621;

        new SekiroClient(group, clientId, host, port)
                .setupSekiroRequestInitializer((req, registry) ->
                        registry.registerSekiroHandler(new ActionHandler() {
                            @Override
                            public String action() { return "testAction"; }

                            @Override
                            public void handleRequest(cn.iinti.sekiro3.business.api.interfaze.SekiroRequest request,
                                                      SekiroResponse response) {
                                response.success("ok-from-java-provider:" + request.getString("param"));
                            }
                        })
                ).start();
        System.out.println("[DemoProviderLocal] " + group + "/" + clientId + " -> " + host + ":" + port);
        Thread.sleep(Long.MAX_VALUE);
    }
}
