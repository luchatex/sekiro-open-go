package server

import (
	"bufio"
	"net"
	"sync"
)

// connListener 把已接受的连接转交给 http.Server（单端口多路复用）。
//
// 并发模型：l.ch 永不 close（避免 Feed 锁外发送与 Close 并发时的
// send-on-closed-channel panic）；关停经独立 done 通道广播，
// Feed/Accept 均以 select 双通道等待，Close 幂等。
type connListener struct {
	ch   chan net.Conn
	done chan struct{}
	once sync.Once
}

func newConnListener() *connListener {
	return &connListener{ch: make(chan net.Conn, 64), done: make(chan struct{})}
}

func (l *connListener) Feed(c net.Conn) {
	select {
	case l.ch <- c:
		// 入队后二次确认：与 Close 的排空存在极小窗口
		// （排空先跑、随后本分支入队成功），补关一次避免 fd 悬挂
		select {
		case <-l.done:
			c.Close()
		default:
		}
	case <-l.done:
		c.Close()
	}
}

func (l *connListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *connListener) Close() error {
	l.once.Do(func() {
		close(l.done)
		// 排空队列：未及接受的连接直接关闭，避免 fd 悬挂到进程退出
		for {
			select {
			case c := <-l.ch:
				c.Close()
			default:
				return
			}
		}
	})
	return nil
}

func (l *connListener) Addr() net.Addr { return &net.TCPAddr{} }

// bufferedConn 将已缓冲的预读数据回灌给 http.Server。
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }
