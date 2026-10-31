//go:build unix

package graceful

import (
	"errors"
	"net"
	"os"
	"sync"
	"time"
)

// gate 包在监听外面：暂停时不再 Accept（新连接留在内核的队列里，不会被拒），放行后接着接。
// 交接时老进程先暂停、新进程再放行，同一时刻只有一个进程在接新连接：老进程接的最后一个连接一定早于新进程接的第一个。
type gate struct {
	net.Listener
	mu     sync.Mutex
	open   chan struct{} // 关闭了表示放行；暂停时换成一个没关闭的
	paused bool
	closed bool
}

func newGate(ln net.Listener, paused bool) *gate {
	g := &gate{Listener: ln, open: make(chan struct{}), paused: paused}
	if !paused {
		close(g.open)
	}
	return g
}

func (g *gate) Accept() (net.Conn, error) {
	for {
		g.mu.Lock()
		open := g.open
		g.mu.Unlock()
		<-open
		c, err := g.Listener.Accept()
		if err != nil && errors.Is(err, os.ErrDeadlineExceeded) {
			// 只有 pause 会设超时：打断了正在等的 Accept，回去等放行（已经又放行了就接着接）。
			// 不能把这个超时交给 fasthttp：它会当成临时错误睡 1 秒
			continue
		}
		return c, err
	}
}

// pause 停止接新连接：正在 Accept 里等着的立刻返回（设一个已经过去的超时），回去等放行。
func (g *gate) pause() {
	g.mu.Lock()
	if g.paused || g.closed {
		g.mu.Unlock()
		return
	}
	g.paused = true
	g.open = make(chan struct{})
	g.mu.Unlock()
	if d, ok := g.Listener.(interface{ SetDeadline(time.Time) error }); ok {
		_ = d.SetDeadline(time.Now())
	}
}

// resume 放行。
func (g *gate) resume() {
	if d, ok := g.Listener.(interface{ SetDeadline(time.Time) error }); ok {
		_ = d.SetDeadline(time.Time{})
	}
	g.mu.Lock()
	if g.paused {
		g.paused = false
		close(g.open)
	}
	g.mu.Unlock()
}

// Close 关掉监听；暂停中等着的 Accept 放出来，拿到「监听已关闭」的错误返回。
// 重复关返回 nil：fasthttp 的 Shutdown 关监听出错时直接返回，不等进行中的请求。
func (g *gate) Close() error {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return nil
	}
	g.closed = true
	if g.paused {
		g.paused = false
		close(g.open)
	}
	g.mu.Unlock()
	return g.Listener.Close()
}
