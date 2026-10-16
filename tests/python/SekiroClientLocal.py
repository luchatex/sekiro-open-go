import json
import sys
import time

sys.path.insert(0, "G:/tmp/sekiro-samples/demo-python")

from sekiro.SekiroClient import SekiroClient, SekiroHandler, SekiroResponse


class TestHandler(SekiroHandler):
    def handle(self, request: json, response: SekiroResponse):
        response.success("ok-python")


# sekiro-samples demo-python 本地化版本：连接本机 Go 版服务端 5620
if __name__ == '__main__':
    sekiro_client = SekiroClient("test-python", host="127.0.0.1", port=5620, client_id="python-samples")
    sekiro_client.register_action("testAction", TestHandler())
    sekiro_client.start()
    print("[samples-python] started, group=test-python clientId=python-samples -> 127.0.0.1:5620")
    time.sleep(3600)
