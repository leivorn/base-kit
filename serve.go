package basekit

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/leivorn/base-kit/config"
	"github.com/leivorn/base-kit/graceful"

	"github.com/gofiber/fiber/v2"
)

// ServeOptions 调整 Serve 的监听地址和平滑重启的时限。零值可用。
type ServeOptions struct {
	// Name 监听名：平滑重启时按名字把 socket 交给新进程。默认 "http"。
	Name string
	// Addr 监听地址，默认 server.host:server.port（host 为空时监听所有网卡，与以前的 ":port" 一样）。
	// 同一个程序里起的第二个服务（子命令，另一个 systemd 单元）填自己的地址。
	Addr string
	// ReadyTimeout 平滑重启时等新进程准备好（读配置、连库、迁移、注册路由）的上限，默认 90 秒。
	// 超时或新进程中途退出：杀掉新进程，老进程照常服务。
	ReadyTimeout time.Duration
	// DrainTimeout 交接以后老进程等自己的连接都关掉的上限，默认 70 秒（nginx upstream keepalive_timeout 默认 60 秒）。
	// 这期间老连接上来的新请求转给新进程（graceful.Forward），进行中的请求照常做完。
	DrainTimeout time.Duration
	// ShutdownTimeout 最后关闭（app.ShutdownWithTimeout）等进行中请求的上限，默认 20 秒；之后 basekit.Go 的任务
	// 还有最多 10 秒返回。systemd 的 TimeoutStopSec 要比两者之和长。
	ShutdownTimeout time.Duration
}

func (o *ServeOptions) defaults() {
	if o.Name == "" {
		o.Name = "http"
	}
	if o.Addr == "" {
		o.Addr = ListenAddr()
	}
	if o.ReadyTimeout <= 0 {
		o.ReadyTimeout = 90 * time.Second
	}
	if o.DrainTimeout <= 0 {
		o.DrainTimeout = 70 * time.Second
	}
	if o.ShutdownTimeout <= 0 {
		o.ShutdownTimeout = 20 * time.Second
	}
}

// ListenAddr 是配置里的监听地址：server.host:server.port，host 为空时是 ":port"（所有网卡）。
func ListenAddr() string {
	return net.JoinHostPort(config.C.Server.Host, strconv.Itoa(config.C.Server.Port))
}

// Serve 在 opts.Addr 上服务 app，直到进程该退出。
//
// 由 systemd 以 Type=notify 启动时（graceful.Supervised）：
//   - 准备好后报 READY=1；
//   - SIGHUP（systemctl reload）平滑重启：同样的程序文件和命令行参数起新进程接手监听 socket，新进程准备好后本进程
//     不再接新连接，老连接上的新请求转给新进程，进行中的请求做完、连接都关掉（最多 DrainTimeout）后关闭退出；
//     新进程起不来时本进程照常服务，日志里有「平滑重启失败」；
//   - SIGTERM / SIGINT（systemctl stop）：不再接新连接，进行中的请求做完（最多 ShutdownTimeout）后退出。
//
// 否则（开发环境、Type=simple 的单元、非 unix）与以前的 app.Listen 完全一样：不接管信号，收到 SIGTERM 进程直接结束。
//
// 正常退出返回 nil，监听失败或服务中途出错返回错误。一个进程只调一次 Serve。
func Serve(app *fiber.App, opts ServeOptions) error {
	opts.defaults()
	if !graceful.Supervised() {
		log.Printf("Server starting on %s", opts.Addr)
		return app.Listen(opts.Addr)
	}
	return serveSupervised(app, opts)
}

func serveSupervised(app *fiber.App, opts ServeOptions) error {
	ln, err := graceful.Listen(opts.Name, "tcp", opts.Addr)
	if err != nil {
		return fmt.Errorf("监听 %s: %w", opts.Addr, err)
	}
	// fasthttp 这一层包上转发：交接以后老连接上的请求不经过任何中间件，直接转给新进程
	app.Server().Handler = graceful.Forward(opts.Name, app.Server().Handler)

	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sigs)
	errc := make(chan error, 1)
	if graceful.Inherited() {
		log.Printf("Server 接手 %s（平滑重启，pid %d）", opts.Addr, os.Getpid())
	} else {
		log.Printf("Server starting on %s（systemd 平滑重启已启用）", opts.Addr)
	}
	go func() { errc <- app.Listener(ln) }()
	if err := graceful.Ready(); err != nil {
		return err
	}
	for {
		select {
		case err := <-errc:
			if err == nil {
				err = fmt.Errorf("监听 %s 意外关闭", opts.Addr)
			}
			return err
		case sig := <-sigs:
			if sig == syscall.SIGHUP {
				log.Printf("平滑重启：启动新进程接手…")
				if err := graceful.Upgrade(opts.ReadyTimeout); err != nil {
					log.Printf("平滑重启失败，继续由当前进程服务：%v", err)
					continue
				}
				log.Printf("平滑重启：新进程已接手，本进程处理完手上的请求后退出（最多 %s）", opts.DrainTimeout)
				graceful.BeginDrain()
				waitIdle(app, opts.DrainTimeout, sigs)
			} else {
				log.Printf("收到退出信号（%v），处理完手上的请求后退出（最多 %s）", sig, opts.ShutdownTimeout)
				_ = graceful.Notify("STOPPING=1")
				graceful.BeginDrain()
			}
			// 监听由 Shutdown 关（BeginDrain 不关），它等进行中的请求做完（最多 ShutdownTimeout）
			if err := app.ShutdownWithTimeout(opts.ShutdownTimeout); err != nil {
				log.Printf("关闭: %v", err)
			}
			log.Printf("已退出（pid %d）", os.Getpid())
			return nil
		}
	}
}

// waitIdle 等 app 打开的连接都关掉，最多 max；期间收到 SIGTERM / SIGINT（systemctl stop）就不再等。
// 不主动关空闲长连接：自己关的话，对方正好拿那条连接发请求时会失败（POST 不重试）；老连接上的请求已经转给新进程，
// 响应带 Connection: close，对方收完自己关。
func waitIdle(app *fiber.App, max time.Duration, sigs <-chan os.Signal) {
	deadline := time.NewTimer(max)
	defer deadline.Stop()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for app.Server().GetOpenConnectionsCount() > 0 {
		select {
		case <-tick.C:
		case <-deadline.C:
			log.Printf("平滑重启：%s 内还有 %d 个连接没关，直接关闭", max, app.Server().GetOpenConnectionsCount())
			return
		case sig := <-sigs:
			if sig != syscall.SIGHUP { // 已经交出去了，再来的 reload 是给新进程的
				return
			}
		}
	}
}
