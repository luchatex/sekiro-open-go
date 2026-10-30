package httpapi

import (
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sekiro-go/internal/protocol"
)

type blockingResponseWriter struct {
	header  http.Header
	entered chan struct{}
	release chan struct{}
	writes  atomic.Int32
	once    sync.Once
}

func (w *blockingResponseWriter) Header() http.Header { return w.header }
func (w *blockingResponseWriter) WriteHeader(int)     {}

func (w *blockingResponseWriter) Write(b []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	w.writes.Add(1)
	return len(b), nil
}

func TestHTTPAbortWaitInflightWriteAndSealsSink(t *testing.T) {
	w := &blockingResponseWriter{
		header:  make(http.Header),
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	sink := &httpSink{w: w, closed: make(chan struct{})}
	wrote := make(chan struct{})
	go func() {
		p := protocol.NewPacket(protocol.STypeInvokeResponse)
		p.Headers = protocol.NewHeaders()
		p.Data = []byte("partial")
		sink.RespondPacket(p)
		close(wrote)
	}()

	select {
	case <-w.entered:
	case <-time.After(time.Second):
		t.Fatal("响应未进入阻塞写")
	}

	canceled := make(chan struct{})
	go func() {
		sink.cancel()
		close(canceled)
	}()
	select {
	case <-canceled:
		t.Fatal("cancel 在途写完成前返回，gzip Close 可能与 Write 并发")
	case <-time.After(100 * time.Millisecond):
	}

	close(w.release)
	for name, ch := range map[string]<-chan struct{}{"write": wrote, "cancel": canceled} {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatalf("%s 未退出", name)
		}
	}

	before := w.writes.Load()
	sink.RespondCommon(-1, "late", "", 1)
	if got := w.writes.Load(); got != before {
		t.Fatalf("封口后仍发生 %d 次响应写入", got-before)
	}
}
