//go:build unix

package graceful

import (
	"errors"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/valyala/fasthttp"
)

// Forward 包住服务的 fasthttp handler（name 是这个服务的监听名，同 Listen）。basekit.Serve 自动包上，不用自己挂。
//
// 交接以后，老进程不再接新连接，但已经连着的长连接上还会来新请求（nginx 的 upstream keepalive、浏览器直连时的 keep-alive）。
// 这些请求转给新进程处理：连同一个监听地址（现在只有新进程在接），请求原样（Host、原始路径、请求头、请求体）发过去，
// 响应原样带回并加 Connection: close，对方收完就关掉这条长连接。这样交接以后开始的请求都由新程序、新配置处理，
// 也不用服务端主动关空闲长连接（正好发在上面的 POST 会失败，nginx 不重试，就是 502）。
//
// 只是退出（systemctl stop，没有新进程）时照常处理，响应加 Connection: close。
// 包在 fasthttp 这一层（比 Fiber 的全部中间件都早）：转走的请求在老进程里不经过任何中间件，日志、操作日志都记在新进程。
func Forward(name string, next fasthttp.RequestHandler) fasthttp.RequestHandler {
	return std.forward(name, next)
}

func (s *state) forward(name string, next fasthttp.RequestHandler) fasthttp.RequestHandler {
	var once sync.Once
	var client *fasthttp.HostClient
	return func(ctx *fasthttp.RequestCtx) {
		if !s.handedOver.Load() {
			next(ctx)
			if s.isDraining() {
				ctx.SetConnectionClose()
			}
			return
		}
		once.Do(func() {
			s.mu.Lock()
			g := s.listeners[name]
			s.mu.Unlock()
			if g != nil {
				client = &fasthttp.HostClient{
					Addr:                     dialAddr(g.Addr()),
					DialDualStack:            true,
					DisablePathNormalizing:   true,
					NoDefaultUserAgentHeader: true,
					ReadTimeout:              5 * time.Minute, // 慢请求；nginx 的 proxy_read_timeout 一般不超过这个
					WriteTimeout:             time.Minute,
					MaxConns:                 1024,
				}
			}
		})
		if client == nil {
			next(ctx)
			return
		}
		req := fasthttp.AcquireRequest()
		resp := fasthttp.AcquireResponse()
		defer fasthttp.ReleaseRequest(req)
		defer fasthttp.ReleaseResponse(resp)
		ctx.Request.CopyTo(req)
		req.URI().DisablePathNormalizing = true
		if err := client.Do(req, resp); err != nil {
			if notConnected(err) {
				// 连不上新进程（请求没发出去；交接失败、本进程又恢复了接新连接）：自己处理
				log.Printf("graceful: 连不上新进程，自己处理 %s %s: %v", ctx.Method(), ctx.Path(), err)
				next(ctx)
				return
			}
			log.Printf("graceful: 转给新进程失败: %s %s: %v", ctx.Method(), ctx.Path(), err)
			ctx.ResetBody()
			ctx.SetStatusCode(fasthttp.StatusBadGateway)
			ctx.SetBodyString("bad gateway")
			ctx.SetConnectionClose()
			return
		}
		resp.CopyTo(&ctx.Response)
		ctx.SetConnectionClose()
	}
}

// dialAddr 是连监听地址用的地址：监听在所有地址上（0.0.0.0、::）时连本机。
func dialAddr(a net.Addr) string {
	if t, ok := a.(*net.TCPAddr); ok && (t.IP == nil || t.IP.IsUnspecified()) {
		return net.JoinHostPort("127.0.0.1", strconv.Itoa(t.Port))
	}
	return a.String()
}

// notConnected 报告请求是不是还没发出去（连接都没建起来），这时自己处理不会重复执行。
func notConnected(err error) bool {
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return true
	}
	return errors.Is(err, fasthttp.ErrDialTimeout) || errors.Is(err, fasthttp.ErrNoFreeConns) ||
		strings.Contains(err.Error(), "couldn't find DNS entries")
}
