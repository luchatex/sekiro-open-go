import json
import sys

sys.path.insert(0, "G:/tmp/sekiro-samples/demo-python")

# 补丁：官方 samples SekiroInvoker 的 _decode_sekiro_fast_json 把 SEKIRO_CLIENT_ID
# 头值裸拼进 JSON（{"clientId":x} 非法），对 Java 官方服务端同样报
# JSONDecodeError（已实测对拍）。此处为头值补 JSON 引号，验证服务端数据本体。
import sekiro.SekiroInvoker as _SI

_orig_decode = _SI._decode_sekiro_fast_json


def _decode_fixed(data, client_id):
    if client_id and client_id != "null" and not client_id.startswith('"'):
        client_id = '"' + client_id + '"'
    return _orig_decode(data, client_id)


_SI._decode_sekiro_fast_json = _decode_fixed

from sekiro.SekiroInvoker import SekiroInvoker

if __name__ == '__main__':
    port = sys.argv[1] if len(sys.argv) > 1 else "5620"
    inv = SekiroInvoker(api_token="", server_list=["127.0.0.1:" + port])
    resp = inv.request("test-python", "testAction", param="patched")
    print("INVOKER_RESPONSE[" + port + "]: " + json.dumps(resp, ensure_ascii=False))
    import os
    os._exit(0)  # tornado 线程非守护，直接退出
