import json
import sys

sys.path.insert(0, "G:/tmp/sekiro-samples/demo-python")

from sekiro.SekiroInvoker import SekiroInvoker

# sekiro-samples demo-python Invoker 本地化版本：
# 二进制 invoker 通道（I_TYPE_CONNECT/I_TYPE_INVOKE）直连本机 Go 版服务端 5620
if __name__ == '__main__':
    sekiro_invoker = SekiroInvoker(api_token="", server_list=["127.0.0.1:5620"])
    print("begin request..")
    response = sekiro_invoker.request("test-python", "testAction", param="testparm")
    print("invoke response: " + str(json.dumps(response, ensure_ascii=False)))
