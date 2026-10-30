//go:build !unix

package graceful

import (
	"errors"
	"net"
	"time"

	"github.com/valyala/fasthttp"
)

// 非 unix 平台不支持平滑重启：Supervised 恒为 false，basekit.Serve 走普通的 Listen；下面的函数只为让引用本包的代码能编译。

func Supervised() bool { return false }

func Inherited() bool { return false }

func Listen(name, network, addr string) (net.Listener, error) { return net.Listen(network, addr) }

func Forward(name string, next fasthttp.RequestHandler) fasthttp.RequestHandler { return next }

func Notify(state string) error { return nil }

func Ready() error { return nil }

func Upgrade(timeout time.Duration) error { return errors.New("平滑重启只支持 unix") }

var drain = make(chan struct{})

func BeginDrain() {}

func Draining() <-chan struct{} { return drain }

func IsDraining() bool { return false }

func OnLost(fn func()) {}
