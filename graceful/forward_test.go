//go:build unix

package graceful

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
)

// 交接以后，老进程长连接上来的新请求转给新进程：Host、原始路径、请求体、X-Forwarded-For 原样，响应带 Connection: close；
// 交接撤销后（新进程没起来）老进程自己处理
func TestForwardRequestsOnOldConnections(t *testing.T) {
	s := newState()
	ln, err := s.listen("api", "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	echo := func(who string) fiber.Handler {
		return func(c *fiber.Ctx) error {
			return c.SendString(fmt.Sprintf("%s|%s|%s|%s|%s", who, c.Hostname(), string(c.Request().Header.RequestURI()), c.Body(), c.Get("X-Forwarded-For")))
		}
	}
	old := fiber.New(fiber.Config{DisableStartupMessage: true})
	old.All("/*", echo("old"))
	old.Server().Handler = s.forward("api", old.Server().Handler)
	go func() { _ = old.Listener(ln) }()
	defer old.Shutdown()

	// 交接前就连上老进程的长连接（像 nginx 的 upstream keepalive）
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	send := func(body string) (*http.Response, string) {
		t.Helper()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		fmt.Fprintf(conn, "POST /s/a%%2Fb//c?x=1 HTTP/1.1\r\nHost: preview.example\r\nX-Forwarded-For: 203.0.113.5\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(b)
	}
	if _, b := send("one"); b != "old|preview.example|/s/a%2Fb//c?x=1|one|203.0.113.5" {
		t.Fatalf("交接前 = %q", b)
	}

	// 「新进程」：同一个 socket 的另一份 fd 上服务（交接时就是这样），老进程暂停接新连接、开始转发
	f, err := dupListener(s.listeners["api"])
	if err != nil {
		t.Fatal(err)
	}
	nln, err := net.FileListener(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	nw := fiber.New(fiber.Config{DisableStartupMessage: true})
	nw.All("/*", echo("new"))
	go func() { _ = nw.Listener(nln) }()
	defer nw.Shutdown()
	s.pauseAll()
	s.handedOver.Store(true)

	resp, b := send("two")
	if want := "new|preview.example|/s/a%2Fb//c?x=1|two|203.0.113.5"; b != want {
		t.Fatalf("交接后老连接上的请求 = %q，应为 %q", b, want)
	}
	if !resp.Close {
		t.Fatal("转发的响应应当带 Connection: close")
	}

	// 交接撤销：老进程恢复接新连接、自己处理（新进程那边关掉）
	_ = nw.Shutdown()
	s.handedOver.Store(false)
	s.resumeAll()
	r, err := http.Get("http://" + addr + "/x")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if !strings.HasPrefix(string(got), "old|") {
		t.Fatalf("撤销交接后 = %q", got)
	}
}

func TestDialAddr(t *testing.T) {
	for in, want := range map[string]string{"[::]:8080": "127.0.0.1:8080", "0.0.0.0:8080": "127.0.0.1:8080", "127.0.0.1:8080": "127.0.0.1:8080"} {
		a, _ := net.ResolveTCPAddr("tcp", in)
		if got := dialAddr(a); got != want {
			t.Errorf("%s → %s，应为 %s", in, got, want)
		}
	}
}
