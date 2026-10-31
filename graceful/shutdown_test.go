//go:build unix

package graceful

import (
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
)

// basekit.Serve 退出的顺序是 BeginDrain →（平滑重启交接后）等连接关完（GetOpenConnectionsCount）→ app.ShutdownWithTimeout，
// 进行中的请求（请求体还在上传）都要做完。以前 BeginDrain 先关了监听：Shutdown 里 fasthttp 再关一次报 net.ErrClosed、
// 不等连接直接返回，进程一退请求就断了（systemctl stop 也一样）；GetOpenConnectionsCount 在 Serve 返回以后少算一个，
// 收尾时剩最后一个进行中的请求就当成没有连接了。
func TestShutdownWaitsForInFlightRequests(t *testing.T) {
	for _, handedOver := range []bool{false, true} {
		name := "直接退出"
		if handedOver {
			name = "交接以后收尾"
		}
		t.Run(name, func(t *testing.T) {
			s := newState()
			ln, err := s.listen("api", "tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			app := fiber.New(fiber.Config{DisableStartupMessage: true})
			app.Post("/", func(c *fiber.Ctx) error { return c.SendString(strconv.Itoa(len(c.Body()))) })
			served := make(chan error, 1)
			go func() { served <- app.Listener(ln) }()

			// 请求体分两半发：前一半发出去以后开始退出，后一半等 Shutdown 开始以后再发
			pr, pw := io.Pipe()
			req, _ := http.NewRequest(http.MethodPost, "http://"+ln.Addr().String()+"/", pr)
			req.ContentLength = 10
			type result struct {
				body string
				err  error
			}
			got := make(chan result, 1)
			go func() {
				client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
				resp, err := client.Do(req)
				if err != nil {
					got <- result{err: err}
					return
				}
				b, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				got <- result{string(b), err}
			}()
			if _, err := pw.Write([]byte("12345")); err != nil {
				t.Fatal(err)
			}
			for deadline := time.Now().Add(5 * time.Second); app.Server().GetOpenConnectionsCount() != 1; time.Sleep(10 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatalf("请求没连上：%d 个连接", app.Server().GetOpenConnectionsCount())
				}
			}

			if handedOver {
				s.pauseAll() // Upgrade 交接时做的
			}
			s.beginDrain()
			if handedOver {
				// 同 basekit 的 waitIdle：连接数降到 0 才往下走，最多 1 秒
				start := time.Now()
				for deadline := start.Add(time.Second); app.Server().GetOpenConnectionsCount() > 0 && time.Now().Before(deadline); {
					time.Sleep(20 * time.Millisecond)
				}
				if d := time.Since(start); d < 900*time.Millisecond {
					t.Errorf("还有一个进行中的请求，连接数 %v 后就成了 0", d)
				}
			}
			go func() {
				time.Sleep(300 * time.Millisecond)
				_, _ = pw.Write([]byte("67890"))
				_ = pw.Close()
			}()
			if err := app.ShutdownWithTimeout(5 * time.Second); err != nil {
				t.Errorf("Shutdown 没等进行中的请求：%v", err)
			}
			if r := <-got; r.err != nil || r.body != "10" {
				t.Fatalf("进行中的请求：%q %v", r.body, r.err)
			}
			if err := <-served; err != nil {
				t.Errorf("Serve 返回 %v", err)
			}
		})
	}
}
