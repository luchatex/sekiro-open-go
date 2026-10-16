// loadtest 是 sekiro-go 的压测工具（M10）。
//
// provider 模式：启动 N 个原生 sekiro 协议客户端注册到服务端，
// 收到 S_TYPE_INVOKE 后立即返回固定长度的响应体。
//
//	loadtest -mode provider -server 127.0.0.1:5620 -group lg -clients 4 -resp-size 256
//
// invoke 模式：并发 HTTP 调用 /business/invoke，统计 QPS 与延迟分位数。
//
//	loadtest -mode invoke -url http://127.0.0.1:5620 -group lg -c 50 -n 20000
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"sekiro-go/internal/protocol"
)

func main() {
	mode := flag.String("mode", "invoke", "provider | invoke")
	server := flag.String("server", "127.0.0.1:5620", "provider 模式: sekiro 服务端 host:port")
	url := flag.String("url", "http://127.0.0.1:5620", "invoke 模式: 服务端 HTTP 基地址")
	group := flag.String("group", "loadtest", "注册/调用的 group")
	clients := flag.Int("clients", 4, "provider 模式: 客户端连接数")
	respSize := flag.Int("resp-size", 256, "provider 模式: 响应体字节数")
	conc := flag.Int("c", 50, "invoke 模式: 并发协程数")
	total := flag.Int("n", 20000, "invoke 模式: 总请求数")
	timeout := flag.Duration("timeout", 15*time.Second, "单请求超时")
	flag.Parse()

	switch *mode {
	case "provider":
		runProvider(*server, *group, *clients, *respSize)
	case "invoke":
		runInvoke(*url, *group, *conc, *total, *timeout)
	default:
		fmt.Fprintln(os.Stderr, "unknown mode:", *mode)
		os.Exit(2)
	}
}

// ---- provider：原生协议客户端 ----

func runProvider(server, group string, n, respSize int) {
	body := []byte(strings.Repeat("x", respSize))
	var served atomic.Int64
	done := make(chan struct{})

	for i := 0; i < n; i++ {
		id := fmt.Sprintf("load-%d", i)
		go func() {
			defer func() { done <- struct{}{} }()
			for attempt := 0; attempt < 3; attempt++ {
				if err := providerConn(server, group, id, body, &served); err == nil {
					return
				}
				time.Sleep(time.Second)
			}
			fmt.Fprintf(os.Stderr, "provider %s: give up\n", id)
		}()
	}

	<-done // 至少一个退出即结束
	fmt.Printf("provider connections: %d, served: %d\n", n, served.Load())
}

func providerConn(server, group, id string, body []byte, served *atomic.Int64) error {
	conn, err := net.DialTimeout("tcp", server, 3*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()

	reg := protocol.NewPacket(protocol.CTypeRegister)
	reg.AddHeader(protocol.HeaderSekiroGroup, group)
	reg.AddHeader(protocol.HeaderClientID, id)
	if err := protocol.WritePacket(conn, reg); err != nil {
		return err
	}

	br := bufio.NewReader(conn)
	for {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		p, err := protocol.ReadPacket(br)
		if err != nil {
			return err
		}
		switch p.Type {
		case protocol.TypeHeartbeat:
			continue
		case protocol.STypeInvoke:
			resp := protocol.NewPacket(protocol.CTypeInvokeResponse)
			resp.Seq = p.Seq
			resp.Data = body
			if err := protocol.WritePacket(conn, resp); err != nil {
				return err
			}
			served.Add(1)
		default:
			// 忽略其他服务端包
		}
	}
}

// logSample 记录前 5 条错误样本。
var sampleMu sync.Mutex
var sampleCount int

func logSample(msg string) {
	sampleMu.Lock()
	defer sampleMu.Unlock()
	if sampleCount < 5 {
		fmt.Println("ERR SAMPLE:", msg)
	}
	sampleCount++
}

// ---- invoke：HTTP 并发压测 ----

func runInvoke(base, group string, conc, total int, timeout time.Duration) {
	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			MaxIdleConnsPerHost: conc,
			MaxConnsPerHost:     0,
		},
	}

	var okCount, errCount atomic.Int64
	latencies := make([]time.Duration, 0, total)
	var mu sync.Mutex
	start := time.Now()
	var wg sync.WaitGroup

	perWorker := total / conc
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]time.Duration, 0, perWorker)
			for i := 0; i < perWorker; i++ {
				t0 := time.Now()
				req, _ := http.NewRequest(http.MethodGet,
					base+"/business/invoke?group="+group+"&action=echo", nil)
				req.Header.Set("Connection", "keep-alive")
				resp, err := client.Do(req)
				if err != nil {
					errCount.Add(1)
					logSample(err.Error())
					continue
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					errCount.Add(1)
					logSample(fmt.Sprintf("http %d", resp.StatusCode))
					continue
				}
				// HTTP 200 即计入成功（压测目标为服务端吞吐与延迟）
				okCount.Add(1)
				local = append(local, time.Since(t0))
			}
			mu.Lock()
			latencies = append(latencies, local...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	pct := func(p float64) time.Duration {
		if len(latencies) == 0 {
			return 0
		}
		idx := int(float64(len(latencies)-1) * p)
		return latencies[idx]
	}

	fmt.Printf("requests: %d (ok=%d err=%d) concurrency=%d\n",
		okCount.Load()+errCount.Load(), okCount.Load(), errCount.Load(), conc)
	fmt.Printf("elapsed: %v  throughput: %.0f req/s\n",
		elapsed.Round(time.Millisecond), float64(okCount.Load())/elapsed.Seconds())
	fmt.Printf("latency p50=%v p90=%v p95=%v p99=%v max=%v min=%v\n",
		pct(0.50).Round(time.Microsecond), pct(0.90).Round(time.Microsecond),
		pct(0.95).Round(time.Microsecond), pct(0.99).Round(time.Microsecond),
		pct(1).Round(time.Microsecond), pct(0).Round(time.Microsecond))
}
