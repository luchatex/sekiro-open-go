package httpapi

import (
	"compress/gzip"
	"compress/zlib"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sekiro-go/internal/compress"
	"sekiro-go/internal/group"
	"sekiro-go/internal/protocol"
)

func TestNegotiateEncoding(t *testing.T) {
	cases := []struct {
		accept string
		want   string
	}{
		{"", ""},
		{"identity", ""},
		{"*", ""}, // 通配符不参与协商（对齐 Netty）
		{"gzip", "gzip"},
		{"gzip;q=1.0", "gzip"},
		{"deflate, gzip", "gzip"}, // gzip 优先
		{"deflate", "deflate"},
		{"x-gzip", "gzip"},
		{"gzip;q=0", ""}, // q=0 排除
		{"deflate;q=0, gzip;q=0", ""},
		{"deflate;q=0.5, gzip;q=0", "deflate"},
		{"br, gzip", "gzip"}, // 不认识的编码忽略
		{"GZIP", "gzip"},     // 大小写不敏感
		{"gzip ; q=0.8", "gzip"},
	}
	for _, c := range cases {
		if got := negotiateEncoding(c.accept); got != c.want {
			t.Errorf("negotiateEncoding(%q) = %q, want %q", c.accept, got, c.want)
		}
	}
}

// newTestHandler 构造带空 group 管理器的完整 Handler（监控页开启）。
func newTestHandler() http.Handler {
	return Handler(group.NewManager(nil), compress.NewManager(), nil, true, "")
}

func TestGzipGroupList(t *testing.T) {
	srv := httptest.NewServer(newTestHandler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/business/groupList", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if enc := resp.Header.Get("Content-Encoding"); enc != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", enc)
	}
	if vary := resp.Header.Get("Vary"); vary != "Accept-Encoding" {
		t.Fatalf("Vary = %q, want Accept-Encoding", vary)
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		t.Fatalf("Content-Length should be removed when compressing, got %q", cl)
	}

	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"status":0`) {
		t.Fatalf("decompressed body = %s, want status:0 json", body)
	}
}

func TestDeflateGroupList(t *testing.T) {
	srv := httptest.NewServer(newTestHandler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/business/groupList", nil)
	req.Header.Set("Accept-Encoding", "deflate")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if enc := resp.Header.Get("Content-Encoding"); enc != "deflate" {
		t.Fatalf("Content-Encoding = %q, want deflate", enc)
	}
	zr, err := zlib.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("zlib.NewReader (Netty deflate 为 zlib wrapper): %v", err)
	}
	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"status":0`) {
		t.Fatalf("decompressed body = %s, want status:0 json", body)
	}
}

func TestNoCompressionWithoutAcceptEncoding(t *testing.T) {
	srv := httptest.NewServer(newTestHandler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/business/groupList")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if enc := resp.Header.Get("Content-Encoding"); enc != "" {
		t.Fatalf("Content-Encoding = %q, want empty (无 Accept-Encoding 不压缩)", enc)
	}
}

func TestNoCompressionQZero(t *testing.T) {
	srv := httptest.NewServer(newTestHandler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/business/groupList", nil)
	req.Header.Set("Accept-Encoding", "gzip;q=0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if enc := resp.Header.Get("Content-Encoding"); enc != "" {
		t.Fatalf("Content-Encoding = %q, want empty (q=0 排除)", enc)
	}
}

func TestNoCompressionOn404Path(t *testing.T) {
	// 404 路径对应原版反代通道（无 HttpContentCompressor），不压缩
	srv := httptest.NewServer(newTestHandler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/whatever", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if enc := resp.Header.Get("Content-Encoding"); enc != "" {
		t.Fatalf("Content-Encoding = %q, want empty (404 路径不压缩)", enc)
	}
}

// TestSegmentStreamWithGzip 分段流经压缩器：flush 透传 + 最终 body 完整可解压。
func TestSegmentStreamWithGzip(t *testing.T) {
	rec := httptest.NewRecorder()
	cw := newCompressWriter(rec, "gzip")
	sink := &httpSink{w: cw, closed: make(chan struct{})}

	sink.RespondPacket(mkSegmentPacket("第一段数据", true))
	sink.RespondPacket(mkSegmentPacket("第二段数据", true))
	sink.RespondPacket(mkSegmentPacket("", false)) // 终结包
	<-sink.Closed()
	cw.Close()

	if enc := rec.Header().Get("Content-Encoding"); enc != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", enc)
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(body); got != "第一段数据第二段数据" {
		t.Fatalf("segment body = %q, want %q", got, "第一段数据第二段数据")
	}
}

// mkSegmentPacket 构造分段流包（SEGMENT_STREAM_FLAG 头 + 文本 payload）。
func mkSegmentPacket(data string, flag bool) *protocol.Packet {
	h := protocol.NewHeaders()
	if flag {
		h.Set(protocol.SegmentStreamFlag, "true")
		h.Set(protocol.PayloadContentType, "text/plain")
	}
	return &protocol.Packet{Type: protocol.STypeInvokeResponse, Headers: h, Data: []byte(data)}
}
