import json
import sys
sys.path.insert(0, "G:/tmp/sekiro-samples/demo-python")
from sekiro.SekiroInvoker import SekiroInvoker

if __name__ == '__main__':
    sekiro_invoker = SekiroInvoker(api_token="", server_list=["127.0.0.1:5621"])
    print("begin request..")
    response = sekiro_invoker.request("test-python", "testAction", param="testparm")
    print("invoke response: " + str(json.dumps(response, ensure_ascii=False)))
