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
    // 应用层心跳（半开连接检测）：
    //   浏览器发不了 WS Ping（无此 API），provider 又是纯静默端——链路被中间
    //   设备悄悄切断时，服务端 25s 内靠 Ping 判死，客户端却永远停在
    //   readyState=1，业务侧只看到设备"凭空消失"。这里用应用层心跳补上：
    //   注册时声明 hb=1，服务端回一帧 hello 后开始按周期发心跳；
    //   连续 3 个周期没有任何下行帧即判定半开，主动重连。
    //   老服务端不会回 hello，本连接自动降级为「不发心跳」，行为与旧版一致。
    this.hbEnabled = this.opts.heartbeat !== false;
    this.hbSupported = false;
    this.hbInterval = this.opts.heartbeatInterval || 15000;
    // 显式配置的间隔优先于服务端建议值（测试/特殊网络下需要更短的判活周期）
    this.hbIntervalFixed = !!this.opts.heartbeatInterval;
    this.lastRxAt = 0;
    this.hbTimer = null;
    this.hbWatchdog = null;
    this.hbHelloTimer = null;
    this.hbVisibilityHandler = null;
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
    var url = /\/register(\?|$)/i.test(wsURL) ? wsURL : null;
    if (!url) {
        var group = opts.group, clientId = opts.clientId;
        if (group && clientId) {
            url = wsURL.replace(/\/+$/, '') +
                '/business/register?group=' + encodeURIComponent(group) +
                '&clientId=' + encodeURIComponent(clientId);
        } else {
            throw new Error(
                'sekiro: wsURL "' + wsURL + '" 不是注册地址。两种写法二选一：\n' +
                '  1) 完整注册地址：new SekiroClient("' + wsURL + '/business/register?group=yourGroup&clientId=yourId")\n' +
                '  2) 基础地址 + 参数：new SekiroClient("' + wsURL + '", { group: "yourGroup", clientId: "yourId" })'
            );
        }
    }
    // 声明支持应用层心跳：服务端据此下发能力通告，老服务端会忽略该参数
    if (this.hbEnabled) {
        url += (url.indexOf('?') >= 0 ? '&' : '?') + 'hb=1';
    }
    return url;
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
    this._stopHeartbeat(); // 重连前先清掉上一轮的定时器，避免叠加
    try {
        this.socket = this.webSocketFactory(this.wsURL);
    } catch (e) {
        logToConsole('sekiro: create connection failed, reconnect: ' + e);
        this.scheduleReconnect();
        return;
    }

    // 用局部变量固化本连接：重连/强制重建后，旧 socket 迟到的 close 事件
    // 必须被丢弃，否则会触发二次重连（半开场景下 close 事件可能很久才到）。
    var sock = this.socket;

    // 浏览器 WebSocket 自动回 Pong，无需（也不应）应用层处理 Ping
    sock.onmessage(function (event) {
        if (_this.socket !== sock) return;
        _this._onFrame(event.data);
    });

    sock.onopen(function () {
        if (_this.socket !== sock) return;
        logToConsole('sekiro: connection opened');
        _this.reconnectDelay = 1000; // 连接成功后重置退避
        _this.lastRxAt = Date.now();
        if (_this.hbEnabled) {
            // 3s 内没收到能力通告 = 对端是旧版本服务端，本连接不发心跳，
            // 行为退回与官方版一致（不会触发 4400 被踢）
            _this.hbHelloTimer = setTimeout(function () {
                if (!_this.hbSupported && _this.isConnected()) {
                    logToConsole('sekiro: 服务端未下发心跳通告（旧版本），本次连接不启用应用层心跳');
                }
            }, 3000);
        }
        if (typeof _this.opts.onOpen === 'function') {
            try { _this.opts.onOpen(); } catch (e) { /* ignore */ }
        }
    });

    sock.onclose(function (event) {
        if (_this.socket !== sock) return; // 已被强制重建，旧连接的收尾事件忽略
        _this._stopHeartbeat();
        var reason = describeCloseEvent(event);
        var hint = (event && event.code === 1006) ? _this._loopbackHint() : '';
        logToConsole('sekiro: disconnected, ' + reason + ', schedule reconnect' + hint);
        if (typeof _this.opts.onClose === 'function') {
            try { _this.opts.onClose(event, reason + hint); } catch (e) { /* ignore */ }
        }
        _this.scheduleReconnect();
    });

    sock.onerror = function (event) {
        if (_this.socket !== sock) return;
        // 浏览器不暴露错误细节（安全策略），诊断信息以紧随其后的 close code 为准
        logToConsole('sekiro: connection error（细节见紧随其后的 close 日志，1006=服务端不可达，1015=TLS 证书问题）');
        if (typeof _this.opts.onError === 'function') {
            try { _this.opts.onError(event); } catch (e) { /* ignore */ }
        }
    };
};

// _restart 丢弃当前连接并立刻重连。
//
// 与 scheduleReconnect 的区别：用于「连接已被判定为假活/半开」的场景——
// 此时 socket.close() 的关闭握手也可能永远收不到对端回应（同样的半开），
// 不能等 onclose 再重连。这里先摘掉引用（旧连接的迟到事件因此被忽略），
// 再新开连接；新连接失败会走正常的退避流程。
SekiroClient.prototype._restart = function (why) {
    this._stopHeartbeat();
    var old = this.socket;
    this.socket = null;
    if (old) {
        try { old.close(); } catch (e) { /* ignore */ }
    }
    logToConsole('sekiro: 主动重建连接（' + why + '）');
    this.reconnectDelay = 1000;
    this.connect();
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

// ---- 应用层心跳（半开连接检测）----
//
// 背景：浏览器没有 WS Ping API，服务端每 20s 发的 Ping 只能证明「服务端还活着」，
// 客户端无从判断链路是否已断。链路被中间设备静默切断时，服务端 25s 内就判死并
// 把设备移出 group，客户端却永远停在 readyState=1 —— 业务侧看到「设备凭空消失，
// 直到有人刷新页面才回来」。
//
// 协议（向后兼容）：
//   1. 注册地址带 hb=1 → 新服务端立刻回一帧 {"__sekiro_hello__":1,"hb":10}
//   2. 客户端收到 hello 后每 hb 秒发 {"__sekiro_hb__":<ts>}，服务端原样回显
//   3. 连续 3 个周期没有任何下行帧 → 判定半开，丢弃旧连接并重建
//   4. 收不到 hello（旧服务端）= 本连接不发心跳，行为与官方版完全一致
//
// 间隔取 10s 而非更长：服务端判死是「写空闲 20s + 等 pong 10s」= 30s，
// 客户端 3×10s = 30s 与之对齐，避免出现「服务端已把设备移出 group、客户端
// 还以为自己在线」的空窗。

// _onFrame 收帧入口：先识别两帧控制消息，其余交给业务分发。
// 用 indexOf 判别而非 JSON.parse —— 业务响应可能是 3MB 大文本，不能为判别
// 多解析一次。
SekiroClient.prototype._onFrame = function (data) {
    this.lastRxAt = Date.now();
    if (typeof data === 'string') {
        if (data.indexOf('"__sekiro_hello__"') >= 0 && data.length < 200) {
            var hello;
            try { hello = JSON.parse(data); } catch (e) { hello = null; }
            if (hello && hello['__sekiro_hello__']) {
                this._startHeartbeat(hello['hb']);
                return;
            }
        }
        if (data.indexOf('"__sekiro_hb__"') >= 0 && data.length < 200) {
            return; // 心跳回执：lastRxAt 已刷新，无需业务处理
        }
    }
    this.handleSekiroRequest(data);
};

// _startHeartbeat 收到 hello 后启动心跳与看护。
SekiroClient.prototype._startHeartbeat = function (hintSeconds) {
    var _this = this;
    this._stopHeartbeat();
    if (!this.hbIntervalFixed && hintSeconds && hintSeconds >= 3 && hintSeconds <= 120) {
        this.hbInterval = hintSeconds * 1000;
    }
    this.hbSupported = true;
    logToConsole('sekiro: 应用层心跳已启用（间隔 ' + Math.round(this.hbInterval / 1000) + 's）');
    this.hbTimer = setInterval(function () { _this._sendHeartbeat(); }, this.hbInterval);

    // 看护：3 个周期没有任何下行帧 → 半开，丢弃旧连接并重建。
    // 页面隐藏（后台标签页）时不做判定：浏览器会节流后台页的定时器，误判会把
    // 健康连接掐掉。此时只把基准时间前移；重新可见时立刻补发一次心跳再开始计。
    var hidden = function () {
        return typeof document === 'object' && document.visibilityState === 'hidden';
    };
    this.hbWatchdog = setInterval(function () {
        if (!_this.isConnected()) return;
        if (hidden()) { _this.lastRxAt = Date.now(); return; }
        var idle = Date.now() - _this.lastRxAt;
        if (idle > _this.hbInterval * 3) {
            logToConsole('sekiro: 心跳失联 ' + Math.round(idle / 1000) +
                's（超过 3 个周期），判定连接已半开，主动重连');
            if (typeof _this.opts.onHeartbeatLost === 'function') {
                try { _this.opts.onHeartbeatLost(idle); } catch (e) { /* ignore */ }
            }
            _this._restart('心跳失联 ' + Math.round(idle / 1000) + 's');
        }
    }, Math.max(1000, Math.floor(this.hbInterval / 3)));

    // 从后台切回前台：后台期间可能已断（系统休眠/网络切换），立刻补一次心跳，
    // 并重置基准，给对端一个完整的 3 周期窗口再判死。
    if (typeof document === 'object' && typeof document.addEventListener === 'function') {
        this.hbVisibilityHandler = function () {
            if (document.visibilityState === 'hidden') return;
            _this.lastRxAt = Date.now();
            _this._sendHeartbeat();
        };
        document.addEventListener('visibilitychange', this.hbVisibilityHandler);
    }
};

// _sendHeartbeat 发一帧心跳（连接未建立时静默跳过，写失败由 close 事件接管）。
SekiroClient.prototype._sendHeartbeat = function () {
    if (!this.isConnected()) return;
    try {
        this.socket.send(JSON.stringify({ '__sekiro_hb__': Date.now() }));
    } catch (e) { /* ignore */ }
};

SekiroClient.prototype._stopHeartbeat = function () {
    if (this.hbHelloTimer) { clearTimeout(this.hbHelloTimer); this.hbHelloTimer = null; }
    if (this.hbTimer) { clearInterval(this.hbTimer); this.hbTimer = null; }
    if (this.hbWatchdog) { clearInterval(this.hbWatchdog); this.hbWatchdog = null; }
    if (this.hbVisibilityHandler && typeof document === 'object' &&
        typeof document.removeEventListener === 'function') {
        document.removeEventListener('visibilitychange', this.hbVisibilityHandler);
    }
    this.hbVisibilityHandler = null;
    this.hbSupported = false;
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
