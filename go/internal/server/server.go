// Package server 实现单端口多协议服务：
// 同一端口按首包字节探测分流（对齐 ProtocolDetector + SekiroMatcher/HttpMatcher）：
//   - sekiro 魔数("sekiro01") → 二进制协议通道（provider client / invoker）
//   - HTTP 请求行（方法+空格+/）→ 标准 http.Server（API + WebSocket 升级）
//   - 其他 → 回写 "sekiro unknown protocol" 并关闭
package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"sekiro-go/internal/compress"
	"sekiro-go/internal/config"
	"sekiro-go/internal/group"
	"sekiro-go/internal/httpapi"
	"sekiro-go/internal/native"
	"sekiro-go/internal/protocol"
)

var httpMethods = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "HEAD": true, "OPTIONS": true,
	"PATCH": true, "DELETE": true, "TRACE": true,
}

// defaultSniffTimeout 单端口多协议探测的读超时（等待首包 8~9 字节）。
//
// 未设限时，连上即不发数据的探测连接会永久阻塞在 br.Peek 上，占住 goroutine
// 与 fd（slowloris 面：仅靠并发连接数即可拖垮服务端，无需任何有效请求）。
//
// 存在 Server 实例字段而非包级变量：包级变量会被并发的 handleConn
// 协程读取，测试改写即触发 data race（-race 实证）。
const defaultSniffTimeout = 10 * time.Second

type Server struct {
	cfg     *config.Config
	mgr     *group.Manager
	comp    *compress.Manager
	log     *slog.Logger
	ln      net.Listener
	lnMu    sync.Mutex // 保护 ln / tlsLn：Run 写入与 Addr/TLSAddr/Shutdown/Close 读取跨协程
	httpSrv *http.Server
	memLn   *connListener
	tlsLn   net.Listener // 可选 TLS 监听（wss；配置 -tls-cert/-tls-key 时启用）

	// sniffTimeout 首包探测读超时（见 defaultSniffTimeout）；
	// 归属实例而非包级，避免测试改写与并发协程读取形成 data race。
	sniffTimeout time.Duration
}

func New(cfg *config.Config) (*Server, error) {
	log := slog.Default()
	s := &Server{
		cfg:          cfg,
		mgr:          group.NewManager(log),
		comp:         compress.NewManager(),
		log:          log,
		memLn:        newConnListener(),
		sniffTimeout: defaultSniffTimeout,
	}
	s.httpSrv = &http.Server{
		Handler:           httpapi.Handler(s.mgr, s.comp, log, !cfg.OpDisabled, cfg.OpPassword),
		ReadHeaderTimeout: 30 * time.Second,
		MaxHeaderBytes:    1 << 20,
		// 仅设空闲超时：把"连上不请求"的 keep-alive 连接收回来。
		// 刻意不设 WriteTimeout——invoke 是长轮询（默认 15s，可配置更长）
		// 且分段流按需续期，写超时会切断正常的大响应与流式输出。
		IdleTimeout: 120 * time.Second,
	}
	return s, nil
}

// Manager 暴露 group 管理器（测试与运维接口用）。
func (s *Server) Manager() *group.Manager { return s.mgr }

// Addr 实际监听地址（未监听或已关停时返回 nil）。
// 端口 0（系统分配端口）场景下用于取回真实端口。
func (s *Server) Addr() net.Addr {
	s.lnMu.Lock()
	defer s.lnMu.Unlock()
	if s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

func (s *Server) setListener(ln net.Listener) {
	s.lnMu.Lock()
	s.ln = ln
	s.lnMu.Unlock()
}

func (s *Server) listener() net.Listener {
	s.lnMu.Lock()
	defer s.lnMu.Unlock()
	return s.ln
}

// TLSAddr TLS 监听地址（未启用或已关停时返回 nil）；端口 0 场景取回真实端口。
func (s *Server) TLSAddr() net.Addr {
	s.lnMu.Lock()
	defer s.lnMu.Unlock()
	if s.tlsLn == nil {
		return nil
	}
	return s.tlsLn.Addr()
}

// setTLSListener/tlsListener 与明文监听同一把锁：tlsLn 由 Run 协程写入，
// 而 TLSAddr/Shutdown/Close 在其他协程读取，裸读写会形成 data race。
func (s *Server) setTLSListener(ln net.Listener) {
	s.lnMu.Lock()
	s.tlsLn = ln
	s.lnMu.Unlock()
}

func (s *Server) tlsListener() net.Listener {
	s.lnMu.Lock()
	defer s.lnMu.Unlock()
	return s.tlsLn
}

// shutdownGrace 优雅关停等待窗口：需覆盖默认 15s invoke 超时，
// 让在途的长轮询调用有机会正常回包。
const shutdownGrace = 20 * time.Second

// Run 阻塞服务；ctx 取消（或中断）时触发优雅关停并退出。
// 正常关停返回 nil；监听失败等真实错误原样返回。
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(s.cfg.Port))
	if err != nil {
		return err
	}
	s.setListener(ln)
	s.log.Info("start sekiro server(go)", "port", s.cfg.Port)

	go func() {
		if err := s.httpSrv.Serve(s.memLn); err != nil && err != http.ErrServerClosed {
			s.log.Error("http server exited", "err", err)
		}
	}()

	stopped := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			s.log.Info("shutdown signal received, draining connections", "grace", shutdownGrace)
			if err := s.Shutdown(); err != nil {
				s.log.Warn("graceful shutdown did not complete", "err", err)
			}
			close(stopped)
		case <-stopped:
		}
	}()

	// 可选 TLS 监听（wss）：配置 -tls-cert/-tls-key 时启用。
	// TLS 解密后的字节流走同一 handleConn 协议探测（sekiro 魔数 / HTTP），
	// native 二进制 provider 与 HTTP/WS 客户端均可在 wss 端口工作。
	// 用途：本机自签 wss（浏览器 https 页面直连 wss://127.0.0.1，不受 LNA 拦截）
	// 或生产前置无反代时直接暴露 wss。
	if s.cfg.TLSCert != "" && s.cfg.TLSCertKey != "" {
		cert, cerr := tls.LoadX509KeyPair(s.cfg.TLSCert, s.cfg.TLSCertKey)
		if cerr != nil {
			return cerr
		}
		// CurvePreferences：优先 X25519，其次 P-256——锁定现代曲线，
		// 排除 FIPS/groups 协商中较慢或较弱的选项（内部 wss 场景足够）
		tlsLn, terr := tls.Listen("tcp", ":"+strconv.Itoa(s.cfg.TLSPort),
			&tls.Config{
				Certificates:     []tls.Certificate{cert},
				MinVersion:       tls.VersionTLS12,
				CurvePreferences: []tls.CurveID{tls.X25519, tls.CurveP256},
			})
		if terr != nil {
			return terr
		}
		s.setTLSListener(tlsLn)
		s.log.Info("start sekiro server(go) TLS", "port", s.cfg.TLSPort, "cert", s.cfg.TLSCert)
		go func() {
			for {
				conn, err := tlsLn.Accept()
				if err != nil {
					if !errors.Is(err, net.ErrClosed) {
						s.log.Error("tls listener accept", "err", err)
					}
					return
				}
				go s.handleConn(conn)
			}
		}()
	}

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				<-stopped // 等关停流程收尾，正常退出
				return nil
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go s.handleConn(conn)
	}
}

// Shutdown 优雅关停：停止接入新连接，等待在途 HTTP 请求结束（含 15s
// invoke 长轮询），超时后由 http.Server 强关剩余连接。
func (s *Server) Shutdown() error {
	if ln := s.listener(); ln != nil {
		ln.Close()
	}
	if tlsLn := s.tlsListener(); tlsLn != nil {
		tlsLn.Close()
	}
	s.memLn.Close() // 停止分发已接入连接（排空队列、关掉未服务的 fd）
	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	err := s.httpSrv.Shutdown(ctx)
	s.mgr.Close()
	return err
}

// Close 立即关停（测试用，不等待在途请求）。
func (s *Server) Close() {
	s.httpSrv.Close()
	s.memLn.Close()
	if ln := s.listener(); ln != nil {
		ln.Close()
	}
	if tlsLn := s.tlsListener(); tlsLn != nil {
		tlsLn.Close()
	}
	s.mgr.Close()
}

func (s *Server) handleConn(conn net.Conn) {
	// 连接所有权：binary → native.ServeChannel 内部关闭；
	// HTTP → 移交 http.Server；其余路径在本函数内关闭。
	br := bufio.NewReaderSize(conn, 64<<10)

	// 探测阶段限时（slowloris 防护）；分流后立即清零：
	// binary 由 readLoop 按读空闲自管，HTTP 交给 http.Server 的
	// ReadHeaderTimeout/ReadTimeout，避免长轮询被调用超时误杀。
	if err := conn.SetReadDeadline(time.Now().Add(s.sniffTimeout)); err != nil {
		s.log.Debug("set sniff deadline failed", "err", err)
		conn.Close()
		return
	}

	head, err := br.Peek(8)
	if err != nil {
		conn.Close()
		return
	}
	if binary.BigEndian.Uint64(head) == protocol.Magic {
		_ = conn.SetReadDeadline(time.Time{})
		native.ServeChannel(conn, br, s.mgr, s.comp, s.log)
		return
	}
	if proto, need := classifyHTTP(head); proto {
		if need {
			more, err := br.Peek(9)
			if err != nil || len(more) < 9 {
				conn.Close()
				return
			}
		}
		_ = conn.SetReadDeadline(time.Time{})
		s.memLn.Feed(&bufferedConn{Conn: conn, r: br})
		return
	}
	// 对齐 Java UN_SUPPORT_PROTOCOL_MSG
	conn.Write([]byte("sekiro unknown protocol"))
	conn.Close()
}

// classifyHTTP 对齐 HttpMatcher.match：
// 前 8 字节内出现空格、方法合法、空格后是 '/' → HTTP。
// 返回 need=true 表示需要更多字节确认 URI 首字符。
func classifyHTTP(head []byte) (isHTTP bool, needMore bool) {
	if len(head) < 8 {
		return false, false // PENDING：字节不足（由 Peek 保证不会走到）
	}
	i := bytes.IndexByte(head, ' ')
	if i < 0 {
		return false, false // MISMATCH
	}
	if i+1 >= len(head) {
		return false, true // 需要更多字节判断 '/'
	}
	method := string(bytes.ToUpper(head[:i]))
	if !httpMethods[method] {
		return false, false
	}
	if head[i+1] != '/' {
		return false, false
	}
	return true, false
}
