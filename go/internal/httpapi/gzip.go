package httpapi

// HTTP 响应内容压缩，对齐 Java 版 HttpFirstPacketHandler 中挂载的
// Netty HttpContentCompressor：
//   - 仅 sekiro HTTP API 路径生效（WS 升级与 404 代理路径不经压缩器）
//   - 按请求 Accept-Encoding 协商：gzip（含 x-gzip）优先，其次 deflate
//     （Netty DeflateEncoder 为 zlib wrapper 格式）；q=0 排除
//   - 压缩后删除 Content-Length（Netty HttpContentEncoder 行为，转 chunked）
//   - 无最小长度阈值（无参构造默认压缩一切响应体）
//   - 压缩级别取默认值（Netty JdkZlibEncoder/GzipEncoder 亦为默认级别）
//
// 内容压缩不要求与 Java 版字节一致（客户端按 Content-Encoding 解压即可），
// 仅要求协商语义与响应头一致。

import (
	"compress/gzip"
	"compress/zlib"
	"io"
	"net/http"
	"strconv"
	"strings"
)

const (
	headerAcceptEncoding  = "Accept-Encoding"
	headerContentEncoding = "Content-Encoding"
	headerVary            = "Vary"
)

// negotiateEncoding 对齐 Netty HttpContentCompressor.determineEncoding：
// 解析 Accept-Encoding 各分段（含 q 参数），q>0 时 gzip 优先、deflate 次之。
// 返回 "" 表示不压缩。
func negotiateEncoding(accept string) string {
	gzQ, dcQ := -1.0, -1.0
	for _, part := range strings.Split(accept, ",") {
		token, q := parseEncodingPart(part)
		switch token {
		case "gzip", "x-gzip":
			if gzQ < 0 {
				gzQ = q
			}
		case "deflate":
			if dcQ < 0 {
				dcQ = q
			}
		}
	}
	switch {
	case gzQ > 0:
		return "gzip"
	case dcQ > 0:
		return "deflate"
	}
	return ""
}

// parseEncodingPart 解析单个分段："gzip;q=0.5" → ("gzip", 0.5)。
// 无 q 参数视为 q=1；非法 q 忽略保持 1；通配符 * 不参与协商（对齐 Netty）。
func parseEncodingPart(part string) (string, float64) {
	part = strings.TrimSpace(part)
	if part == "" {
		return "", -1
	}
	segs := strings.Split(part, ";")
	token := strings.ToLower(strings.TrimSpace(segs[0]))
	q := 1.0
	for _, s := range segs[1:] {
		s = strings.TrimSpace(s)
		if len(s) >= 2 && (s[0] == 'q' || s[0] == 'Q') && s[1] == '=' {
			if v, err := strconv.ParseFloat(s[2:], 64); err == nil {
				q = v
			}
		}
	}
	return token, q
}

// compressWriter 包装 http.ResponseWriter，按协商结果压缩响应体。
// enc == "" 时透传。实现 http.Flusher 以支撑分段流 flush 透传。
type compressWriter struct {
	http.ResponseWriter
	enc   string
	gz    io.WriteCloser
	wrote bool
}

// Unwrap 暴露底层 ResponseWriter。
//
// 必需：http.NewResponseController 依赖此接口逐层穿透包装器，
// 否则 SetReadDeadline 到不了真正的连接——body 读取超时保护会失效。
func (cw *compressWriter) Unwrap() http.ResponseWriter { return cw.ResponseWriter }

func newCompressWriter(w http.ResponseWriter, enc string) *compressWriter {
	return &compressWriter{ResponseWriter: w, enc: enc}
}

func (cw *compressWriter) WriteHeader(code int) {
	if cw.wrote {
		cw.ResponseWriter.WriteHeader(code)
		return
	}
	cw.wrote = true
	if cw.enc != "" {
		h := cw.ResponseWriter.Header()
		// Netty HttpContentEncoder：压缩后响应为流式，
		// 删除定长声明并转 chunked（显式声明防止 net/http 补算定长）
		h.Del("Content-Length")
		h.Set("Transfer-Encoding", "chunked")
		h.Set(headerContentEncoding, cw.enc)
		h.Add(headerVary, headerAcceptEncoding)
		switch cw.enc {
		case "gzip":
			cw.gz = gzip.NewWriter(cw.ResponseWriter)
		case "deflate":
			// Netty DeflateEncoder 使用 zlib wrapper（header=true）
			cw.gz = zlib.NewWriter(cw.ResponseWriter)
		}
	}
	cw.ResponseWriter.WriteHeader(code)
}

func (cw *compressWriter) Write(b []byte) (int, error) {
	if !cw.wrote {
		cw.WriteHeader(http.StatusOK)
	}
	if cw.gz != nil {
		return cw.gz.Write(b)
	}
	return cw.ResponseWriter.Write(b)
}

// Flush 透传：先刷压缩器缓冲，再刷底层连接（分段流逐段可见）。
func (cw *compressWriter) Flush() {
	if cw.gz != nil {
		if f, ok := cw.gz.(interface{ Flush() error }); ok {
			_ = f.Flush()
		}
	}
	if f, ok := cw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Close 结束压缩流（写 trailer/校验和）。必须在响应终结时调用。
func (cw *compressWriter) Close() {
	if cw.gz != nil {
		_ = cw.gz.Close()
		cw.gz = nil
	}
}
