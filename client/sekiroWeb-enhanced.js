/*
 * sekiroWeb-enhanced.js — sekiro-samples/demo-web/sekiroWeb.js 的增强版
 *
 * API 与官方版本完全兼容：new SekiroClient(wsURL) + registerAction(action, handler)
 * 针对 js-web 长期运行场景修复/增强：
 *
 * 1. [修复] 官方代码 onping 回调引用未定义变量 socket.pong()，服务端 Ping 到达时
 *    抛 ReferenceError（浏览器 WebSocket 自动回 Pong，无需应用层处理）——移除该回调
 * 2. [增强] 断线重连改为指数退避（1s 起步，2x 递增，上限 30s，加随机抖动），
 *    替代官方固定 2s；服务端长时间不可用时不会高频冲击
 * 3. [增强] onReconnect 事件钩子：new SekiroClient(url, { onReconnect: fn })
 * 4. [增强] 大字符串响应避免无谓的 JSON.parse：仅当字符串以 { 或 [ 开头时才尝试
 *    解析（官方对任意字符串响应先整段 parse，3MB 文本白白消耗一次 CPU 且有误判风险）
 * 5. [修复] sendSuccess 中 code 字段的 falsy 判断缺陷（code=0 会被重写逻辑误入
 *    status 分支）
 * 6. [增强] 连接错误诊断：
 *    - 注册地址解析：支持 { group, clientId } 自动拼接 /business/register，
 *      地址不完整且参数缺失时构造即抛错（原版会连到根路径握手失败无从排查）
 *    - URL 前缀校验（缺 ws://wss:// 直接抛出带示例的错误）
 *    - https 页面连非本机 ws:// 时给出 Mixed Content 警告（localhost 豁免不警告）
 *    - close code 人类可读翻译（1006=服务端不可达等），onClose/onError 钩子
 *    - isConnected() 状态查询；连接未建立时发送不再静默丢失，日志明示
 *
 * 其他行为（协议帧格式、__sekiro_seq__ 关联、压缩协商）与官方一致：
 * 服务端已启用 permessage-deflate，支持协商的浏览器（Chrome/Edge/Firefox）自动压缩。
 *
 * 使用文档见同目录 USAGE.md。
 */

function SekiroClient(wsURL, options) {
    if (!wsURL) {
        throw new Error('sekiro: wsURL can not be empty!! 用法：new SekiroClient("ws://127.0.0.1:5612/business/register?group=xx&clientId=xx") 或 new SekiroClient("ws://127.0.0.1:5612", { group: "xx", clientId: "yy" })');
    }
    if (!/^wss?:\/\//i.test(wsURL)) {
        throw new Error('sekiro: 无效的 wsURL "' + wsURL + '" —— 必须以 ws:// 或 wss:// 开头，例如 "ws://127.0.0.1:5612"');
    }
    this.handlers = {};
    this.socket = null;
    this.reconnectDelay = 1000;
    this.opts = options || {};
    // 注册地址解析：支持简写形式（options.group + options.clientId 自动拼接），
    // URL 已含 /register 或参数不全时给出明确错误（避免连到根路径握手失败）
    this.wsURL = this._resolveRegisterURL(wsURL, this.opts);
    this._checkMixedContent(this.wsURL);
    this.webSocketFactory = this.resolveWebSocketFactory();
    this.connect();
}

// _resolveRegisterURL 官方 API 要求完整注册地址（…/business/register?group=&clientId=）。
// 增强版兼容两种写法：
//   1. 完整注册 URL（含 /register）—— 原样使用
//   2. 基础地址 + options.group / options.clientId —— 自动拼接 /business/register
// 两者都不满足时构造即抛错（否则会连到根路径，握手失败 code=1006 且无从排查）。
SekiroClient.prototype._resolveRegisterURL = function (wsURL, opts) {
    if (/\/register(\?|$)/i.test(wsURL)) return wsURL;
    var group = opts.group, clientId = opts.clientId;
    if (group && clientId) {
        return wsURL.replace(/\/+$/, '') +
            '/business/register?group=' + encodeURIComponent(group) +
            '&clientId=' + encodeURIComponent(clientId);
    }
    throw new Error(
        'sekiro: wsURL "' + wsURL + '" 不是注册地址。两种写法二选一：\n' +
        '  1) 完整注册地址：new SekiroClient("' + wsURL + '/business/register?group=yourGroup&clientId=yourId")\n' +
        '  2) 基础地址 + 参数：new SekiroClient("' + wsURL + '", { group: "yourGroup", clientId: "yourId" })'
    );
};

function logToConsole(msg) {
    console.log(`${new Date().toISOString()} ${msg}`);
}

// _checkMixedContent https 页面连非本机 ws:// 会被浏览器 Mixed Content 拦截，
// 提前给出可操作的警告。localhost/127.0.0.1/[::1] 属 Mixed Content 豁免
// （用 URL.hostname 精确解析，避免把带端口的 127.0.0.1:5612 误判为非本机）；
// 但注意公网 https 页面连本机还受浏览器 Private Network Access 策略约束，
// 若仍被拦见 USAGE.md「https 页面连本机 provider」一节。
SekiroClient.prototype._checkMixedContent = function (wsURL) {
    if (typeof location !== 'object' || location.protocol !== 'https:') return;
    if (!/^ws:\/\//i.test(wsURL)) return;
    var host = '';
    try { host = new URL(wsURL).hostname.toLowerCase(); } catch (e) { return; }
    var loopback = host === 'localhost' || host === '127.0.0.1' || host === '::1' || host === '[::1]';
    if (loopback) return;
    console.warn(
        'sekiro: [Mixed Content] 当前页面为 https，浏览器会拦截到 ' + host + ' 的 ws:// 连接。\n' +
        '  解决：改用 wss://（反代终结 TLS，见 deploy/README.md）'
    );
};

// describeCloseEvent 将 WebSocket close code 翻译为人类可读原因。
function describeCloseEvent(event) {    var code = event ? event.code : undefined;
    var map = {
        1000: '正常关闭（normal closure）',
        1001: '对端离开（页面跳转或服务端主动关闭）',
        1002: '协议错误（protocol error）',
        1006: '连接未建立即断开 —— 最常见：服务端未启动/端口错误/防火墙或代理拦截/网络不可达',
        1008: '被服务端策略拒绝（policy violation）',
        1009: '消息过大（message too big）',
        1011: '服务端内部错误（server error）',
        1015: 'TLS 握手失败 —— wss 证书不受信/过期，或使用了自签证书未导入信任库'
    };
    var text = 'code=' + code + (map[code] ? '（' + map[code] + '）' : '');
    if (event && event.reason) text += ' reason="' + event.reason + '"';
    return text;
}

// _loopbackHint https 页面连本机 ws 而 1006 时的附加诊断：
// Mixed Content 虽豁免 localhost，但 Chrome/Edge 的 Private Network Access
// （PNA）策略仍可能拦截公网页面 → 本机私有地址的请求。
SekiroClient.prototype._loopbackHint = function () {
    if (typeof location !== 'object' || location.protocol !== 'https:') return '';
    var host = '';
    try { host = new URL(this.wsURL).hostname.toLowerCase(); } catch (e) { return ''; }
    if (host !== 'localhost' && host !== '127.0.0.1' && host !== '::1') return '';
    return '\n  [PNA 提示] 公网 https 页面连本机 ws:// 可能被浏览器 Private Network Access 拦截。' +
        '\n    开发期：用 http://127.0.0.1 或本地 file:// 打开页面再测；' +
        '\n    或 chrome://flags 搜索 "private network" 临时关闭（仅开发用）；' +
        '\n    生产：走 wss:// 反代（deploy/README.md）';
};

SekiroClient.prototype.resolveWebSocketFactory = function () {
    // 浏览器取 window.WebSocket；Node 22+ 取原生 WebSocket（undici）。
    // 统一包一层 wrapper：官方调用风格为 onmessage(fn) 方法注入，
    // 而原生 WebSocket 的 onmessage 是属性——直接暴露裸对象在 Node 下会
    // "onmessage is not a function"（此前仅靠测试脚本注入 window shim 掩盖）。
    var Ctor = null;
    if (typeof window === 'object' && window.WebSocket) {
        Ctor = window.WebSocket;
    } else if (typeof WebSocket === 'function') {
        Ctor = WebSocket;
    }
    if (!Ctor) {
        throw new Error('the js environment do not support websocket');
    }
    return function (wsURL) {
        function WebSocketWrapper(wsURL) {
            this.mSocket = new Ctor(wsURL);
        }
        // 透传 readyState：连接状态判定与发送保护需要
        Object.defineProperty(WebSocketWrapper.prototype, 'readyState', {
            get: function () { return this.mSocket.readyState; }
        });
        WebSocketWrapper.prototype.close = function () {
            this.mSocket.close();
        };
        WebSocketWrapper.prototype.onmessage = function (fn) {
            this.mSocket.onmessage = fn;
        };
        WebSocketWrapper.prototype.onopen = function (fn) {
            this.mSocket.onopen = fn;
        };
        WebSocketWrapper.prototype.onclose = function (fn) {
            this.mSocket.onclose = fn;
        };
        WebSocketWrapper.prototype.send = function (message) {
            this.mSocket.send(message);
        };
        return new WebSocketWrapper(wsURL);
    };
};

SekiroClient.prototype.connect = function () {
    logToConsole('sekiro: begin of connect to wsURL: ' + this.wsURL);
    var _this = this;
    try {
        this.socket = this.webSocketFactory(this.wsURL);
    } catch (e) {
        logToConsole('sekiro: create connection failed, reconnect: ' + e);
        this.scheduleReconnect();
        return;
    }

    // 浏览器 WebSocket 自动回 Pong，无需（也不应）应用层处理 Ping
    this.socket.onmessage(function (event) {
        _this.handleSekiroRequest(event.data);
    });

    this.socket.onopen(function () {
        logToConsole('sekiro: connection opened');
        _this.reconnectDelay = 1000; // 连接成功后重置退避
        if (typeof _this.opts.onOpen === 'function') {
            try { _this.opts.onOpen(); } catch (e) { /* ignore */ }
        }
    });

    this.socket.onclose(function (event) {
        var reason = describeCloseEvent(event);
        var hint = (event && event.code === 1006) ? _this._loopbackHint() : '';
        logToConsole('sekiro: disconnected, ' + reason + ', schedule reconnect' + hint);
        if (typeof _this.opts.onClose === 'function') {
            try { _this.opts.onClose(event, reason + hint); } catch (e) { /* ignore */ }
        }
        _this.scheduleReconnect();
    });

    this.socket.onerror = function (event) {
        // 浏览器不暴露错误细节（安全策略），诊断信息以紧随其后的 close code 为准
        logToConsole('sekiro: connection error（细节见紧随其后的 close 日志，1006=服务端不可达，1015=TLS 证书问题）');
        if (typeof _this.opts.onError === 'function') {
            try { _this.opts.onError(event); } catch (e) { /* ignore */ }
        }
    };
};

// isConnected 当前连接是否可用（readyState === OPEN）。
SekiroClient.prototype.isConnected = function () {
    return !!this.socket && this.socket.readyState === 1;
};

// _sendReady 发送前检查；连接未建立时日志明示（不静默丢弃）。
SekiroClient.prototype._sendReady = function (what) {
    if (this.isConnected()) return true;
    logToConsole('sekiro: 消息未送达（连接未建立，将随重连恢复）: ' + what);
    return false;
};

SekiroClient.prototype.scheduleReconnect = function () {
    var _this = this;
    var delay = this.reconnectDelay + Math.floor(Math.random() * 500); // 抖动
    logToConsole('sekiro: reconnect after ' + delay + 'ms');
    if (typeof this.opts.onReconnect === 'function') {
        try { this.opts.onReconnect(delay); } catch (e) { /* ignore */ }
    }
    setTimeout(function () {
        _this.reconnectDelay = Math.min(_this.reconnectDelay * 2, 30000); // 指数退避，上限 30s
        _this.connect();
    }, delay);
};

SekiroClient.prototype.handleSekiroRequest = function (requestJson) {
    var request;
    try {
        request = JSON.parse(requestJson);
    } catch (e) {
        logToConsole('sekiro: invalid json request');
        return;
    }
    var seq = request['__sekiro_seq__'];

    if (!request['action']) {
        this.sendFailed(seq, 'need request param {action}');
        return;
    }
    var action = request['action'];
    if (!this.handlers[action]) {
        this.sendFailed(seq, 'no action handler: ' + action + ' defined');
        return;
    }

    var theHandler = this.handlers[action];
    var _this = this;
    try {
        theHandler(request, function (response) {
            try {
                _this.sendSuccess(seq, response);
            } catch (e) {
                _this.sendFailed(seq, 'e:' + e);
            }
        }, function (errorMessage) {
            _this.sendFailed(seq, errorMessage);
        });
    } catch (e) {
        logToConsole('error: ' + e);
        _this.sendFailed(seq, ':' + e);
    }
};

SekiroClient.prototype.sendSuccess = function (seq, response) {
    var responseJson;
    if (typeof response === 'string') {
        // 仅当形如 JSON 对象/数组时才尝试解析——
        // 避免对大文本响应（如 3MB）做一次无谓且昂贵的整段 parse
        var head = response.charAt(0);
        if (head === '{' || head === '[') {
            try {
                responseJson = JSON.parse(response);
            } catch (e) {
                responseJson = { data: response };
            }
        } else {
            responseJson = { data: response };
        }
    } else if (typeof response === 'object' && response !== null) {
        responseJson = response;
    } else {
        responseJson = { data: response };
    }

    if (Array.isArray(responseJson) || typeof responseJson === 'string') {
        responseJson = { data: responseJson };
    }

    if (responseJson['code'] !== undefined) {
        responseJson['code'] = 0;
    } else if (responseJson['status'] !== undefined) {
        responseJson['status'] = 0;
    } else {
        responseJson['status'] = 0;
    }
    responseJson['__sekiro_seq__'] = seq;
    var responseText = JSON.stringify(responseJson);
    if (this._sendReady('success seq=' + seq)) {
        this.socket.send(responseText);
    }
};

SekiroClient.prototype.sendFailed = function (seq, errorMessage) {
    if (typeof errorMessage !== 'string') {
        errorMessage = JSON.stringify(errorMessage);
    }
    var responseJson = {};
    responseJson['message'] = errorMessage;
    responseJson['status'] = -1;
    responseJson['__sekiro_seq__'] = seq;
    var responseText = JSON.stringify(responseJson);
    if (this._sendReady('fail seq=' + seq)) {
        this.socket.send(responseText);
    }
};

SekiroClient.prototype.registerAction = function (action, handler) {
    if (typeof action !== 'string') {
        throw new Error('an action must be string');
    }
    this.handlers[action] = handler;
    logToConsole('sekiro: register action: ' + action);
};
