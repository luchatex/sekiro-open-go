// sekiroInvoker.js — 调用方参考实现（浏览器 / Node 22+ 通用）
//
// 针对慢页面（单次调用 >100s）的四个必要约束：
//   1. 参数名用正确的 invoke_timeout（毫秒）
//   2. 调用方 HTTP 超时覆盖任务总时长（默认 120s）
//   3. 每个 clientId 同时最多 1 个在途请求，防止长任务堆积
//   4. 重试最多 2 次、退避 + 抖动；带幂等键与熔断
//
// 浏览器：<script src="sekiroInvoker.js"></script> → new SekiroInvoker(...)
// Node：  const SekiroInvoker = require('./sekiroInvoker.js')
(function (root) {
    'use strict';

    var RESERVED = { group: 1, action: 1, bind_client: 1, invoke_timeout: 1, '__invoke_trace_id': 1 };

    function InvokerError(message, status, retryable) {
        var e = new Error(message);
        e.name = 'SekiroInvokerError';
        e.status = status === undefined ? -1 : status;
        e.retryable = !!retryable;
        return e;
    }

    function stableStringify(value) {
        if (value === null || typeof value !== 'object') return JSON.stringify(value);
        if (Array.isArray(value)) return '[' + value.map(stableStringify).join(',') + ']';
        var keys = Object.keys(value).sort();
        return '{' + keys.map(function (k) {
            return JSON.stringify(k) + ':' + stableStringify(value[k]);
        }).join(',') + '}';
    }

    function sleep(ms) {
        return new Promise(function (r) { setTimeout(r, ms); });
    }

    function SekiroInvoker(baseURL, options) {
        if (!baseURL || !/^https?:\/\//i.test(baseURL)) {
            throw new Error('SekiroInvoker: baseURL 必须是 http:// 或 https:// 开头');
        }
        this.baseURL = baseURL.replace(/\/+$/, '');
        var o = options || {};
        this.timeoutMs = o.timeoutMs || 120000;
        this.serverTimeoutMs = o.serverTimeoutMs || 100000;
        this.maxRetries = o.maxRetries === undefined ? 2 : o.maxRetries;
        this.retryBaseMs = o.retryBaseMs || 2000;
        this.breakerWindowMs = o.breakerWindowMs || 300000;
        this.breakerFailures = o.breakerFailures || 3;
        this.breakerCooldownMs = o.breakerCooldownMs || 300000;
        this.idempotencyTTL = o.idempotencyTTL || 300000;
        this.fetchImpl = o.fetch || (typeof fetch !== 'undefined' ? fetch : null);
        if (typeof this.fetchImpl !== 'function') {
            throw new Error('SekiroInvoker: 当前环境没有 fetch，请通过 options.fetch 注入');
        }
        this.chains = new Map();
        this.breakers = new Map();
        this.cache = new Map();
    }

    SekiroInvoker.prototype.invoke = function (group, action, params, options) {
        var self = this;
        var o = options || {};
        var bindClient = o.bindClient || '';
        var idempotencyKey = o.idempotencyKey ||
            group + '|' + action + '|' + bindClient + '|' + stableStringify(params || {});
        var slot = bindClient || '__pool__';

        var cached = this._cacheGet(idempotencyKey);
        if (cached) return Promise.resolve(cached);

        var breaker = this._checkBreaker(slot);
        if (breaker) return Promise.reject(breaker);

        // 每 clientId 单飞：后续调用排队等待，不在同一设备上叠加长任务。
        var prev = this.chains.get(slot) || Promise.resolve();
        var run = prev.catch(function () {}).then(function () {
            return self._invokeWithRetry(group, action, params || {}, {
                bindClient: bindClient,
                idempotencyKey: idempotencyKey,
                timeoutMs: o.timeoutMs || self.timeoutMs,
                signal: o.signal
            });
        });
        this.chains.set(slot, run.catch(function () {}));
        return run.then(function (res) {
            self._cacheSet(idempotencyKey, res);
            self._recordSuccess(slot);
            return res;
        }, function (err) {
            self._recordFailure(slot);
            throw err;
        });
    };

    SekiroInvoker.prototype._invokeWithRetry = function (group, action, params, o) {
        var self = this;
        var attempt = 0;
        function next() {
            return self._invokeOnce(group, action, params, o).catch(function (err) {
                if (!err.retryable || attempt >= self.maxRetries) throw err;
                attempt++;
                var base = self.retryBaseMs * Math.pow(2, attempt - 1);
                var delay = Math.round(base * (0.7 + Math.random() * 0.6));
                return sleep(delay).then(next);
            });
        }
        return next();
    };

    SekiroInvoker.prototype._invokeOnce = function (group, action, params, o) {
        var url = this.baseURL + '/business-demo/invoke?' + this._query(group, action, params, o);
        var ctrl = new AbortController();
        var timer = setTimeout(function () { ctrl.abort(); }, o.timeoutMs);
        if (o.signal) {
            if (o.signal.aborted) ctrl.abort();
            else o.signal.addEventListener('abort', function () { ctrl.abort(); }, { once: true });
        }
        var self = this;
        return this.fetchImpl(url, {
            method: 'GET',
            signal: ctrl.signal,
            headers: { Accept: 'application/json' }
        }).then(function (res) {
            if (!res.ok) throw InvokerError('http status ' + res.status, res.status, res.status >= 500);
            return res.json();
        }).then(function (body) {
            if (body && body.status === 0) return body;
            var message = (body && body.message) || 'unknown error';
            var retryable = /busy|timeout|upstream lose connection/i.test(message);
            throw InvokerError(message, body && body.status, retryable);
        }).catch(function (err) {
            if (err && err.name === 'SekiroInvokerError') throw err;
            var retryable = !o.signal || !o.signal.aborted;
            throw InvokerError('network error: ' + (err && err.message), -1, retryable);
        }).finally(function () {
            clearTimeout(timer);
            void self;
        });
    };

    SekiroInvoker.prototype._query = function (group, action, params, o) {
        var q = new URLSearchParams();
        q.set('group', group);
        q.set('action', action);
        if (o.bindClient) q.set('bind_client', o.bindClient);
        q.set('invoke_timeout', String(this.serverTimeoutMs));
        q.set('__invoke_trace_id', o.idempotencyKey);
        Object.keys(params).forEach(function (k) {
            if (RESERVED[k]) return;
            var v = params[k];
            if (v !== undefined && v !== null) q.set(k, String(v));
        });
        return q.toString();
    };

    SekiroInvoker.prototype._cacheGet = function (key) {
        var hit = this.cache.get(key);
        if (!hit) return null;
        if (Date.now() - hit.at > this.idempotencyTTL) {
            this.cache.delete(key);
            return null;
        }
        return hit.value;
    };

    SekiroInvoker.prototype._cacheSet = function (key, value) {
        this.cache.set(key, { at: Date.now(), value: value });
    };

    SekiroInvoker.prototype._checkBreaker = function (slot) {
        var b = this.breakers.get(slot);
        if (!b || !b.openUntil || Date.now() >= b.openUntil) return null;
        return InvokerError('circuit open for ' + slot + ' until ' + new Date(b.openUntil).toISOString(), -1, false);
    };

    SekiroInvoker.prototype._recordSuccess = function (slot) {
        this.breakers.delete(slot);
    };

    SekiroInvoker.prototype._recordFailure = function (slot) {
        var now = Date.now();
        var windowMs = this.breakerWindowMs;
        var b = this.breakers.get(slot) || { times: [] };
        b.times = b.times.filter(function (t) { return now - t < windowMs; });
        b.times.push(now);
        if (b.times.length >= this.breakerFailures) {
            b.openUntil = now + this.breakerCooldownMs;
            b.times = [];
        }
        this.breakers.set(slot, b);
    };

    root.SekiroInvoker = SekiroInvoker;
    if (typeof module !== 'undefined' && module.exports) module.exports = SekiroInvoker;
})(typeof globalThis !== 'undefined' ? globalThis : this);
