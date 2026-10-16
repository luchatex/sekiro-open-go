import cn.iinti.sekiro3.business.api.SekiroClient;
import cn.iinti.sekiro3.business.api.interfaze.ActionHandler;
import cn.iinti.sekiro3.business.api.interfaze.SekiroRequest;
import cn.iinti.sekiro3.business.api.interfaze.SekiroResponse;

/**
 * sekiro-samples/demo-java Demo1 的本地化版本：
 * 连接本机 Go 版服务端 5620，验证官方 Java 客户端 samples 调用。
 * 构造器参数顺序 (sekiroGroup, clientId, host, port)。
 */
public class Demo1Local {
    public static class TestResponse {
        public String name;
        private Integer age;
        private String request;

        public Integer getAge() { return age; }
        public void setAge(Integer age) { this.age = age; }
        public String getRequest() { return request; }
        public void setRequest(String request) { this.request = request; }
        public String getName() { return name; }
        public void setName(String name) { this.name = name; }
    }

    public static void main(String[] args) throws InterruptedException {
        new SekiroClient("test-java", "java-samples", "127.0.0.1", 5620)
                .setupSekiroRequestInitializer((sekiroRequest, handlerRegistry) ->
                        handlerRegistry.registerSekiroHandler(new ActionHandler() {
                            @Override
                            public String action() { return "testAction"; }

                            @Override
                            public void handleRequest(SekiroRequest sekiroRequest, SekiroResponse sekiroResponse) {
                                TestResponse testResult = new TestResponse();
                                testResult.setAge(10);
                                testResult.setName("test");
                                testResult.setRequest(sekiroRequest.getString("param"));
                                sekiroResponse.success(testResult);
                            }
                        })
                ).start();
        System.out.println("[Demo1Local] started, group=test-java clientId=java-samples -> 127.0.0.1:5620");
        Thread.sleep(Long.MAX_VALUE);
    }
}
