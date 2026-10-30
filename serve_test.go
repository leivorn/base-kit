//go:build unix

package basekit

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/leivorn/base-kit/config"

	"github.com/gofiber/fiber/v2"
)

// 测试程序自己当被 systemd 管的服务：BASEKIT_SERVE_TEST=1 时读版本文件（内容是 fail 就退出，模拟新配置有错起不来），
// 在 BASEKIT_SERVE_TEST_ADDR 上用 Serve 服务，回答「版本|pid」；/slow 睡 1.5 秒再回答（交接时进行中的请求）。
func TestMain(m *testing.M) {
	if os.Getenv("BASEKIT_SERVE_TEST") == "1" {
		os.Exit(serveTestChild())
	}
	os.Exit(m.Run())
}

func serveTestChild() int {
	b, err := os.ReadFile(os.Getenv("BASEKIT_SERVE_TEST_VERSION"))
	if err != nil {
		return 3
	}
	version := strings.TrimSpace(string(b))
	if version == "fail" {
		fmt.Fprintln(os.Stderr, "serve test child: 配置有错，起不来")
		return 1
	}
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	reply := func(c *fiber.Ctx) error { return c.SendString(fmt.Sprintf("%s|%d", version, os.Getpid())) }
	app.Get("/slow", func(c *fiber.Ctx) error {
		time.Sleep(1500 * time.Millisecond)
		return reply(c)
	})
	app.All("/*", reply)
	err = Serve(app, ServeOptions{
		Addr:            os.Getenv("BASEKIT_SERVE_TEST_ADDR"),
		ReadyTimeout:    10 * time.Second,
		DrainTimeout:    5 * time.Second,
		ShutdownTimeout: 5 * time.Second,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "serve test child:", err)
		return 2
	}
	return 0
}

// notifySink 冒充 systemd 收 sd_notify：记下每个 MAINPID，BARRIER 带来的 fd 处理完就关
type notifySink struct {
	path string
	conn *net.UnixConn
	msgs chan string
}

func newNotifySink(t *testing.T) *notifySink {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "bks") // macOS 的 unix socket 路径上限约 104 字节，t.TempDir() 太长
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "notify.sock")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	s := &notifySink{path: path, conn: conn, msgs: make(chan string, 64)}
	go func() {
		buf := make([]byte, 4096)
		oob := make([]byte, 256)
		for {
			n, oobn, _, _, err := conn.ReadMsgUnix(buf, oob)
			if err != nil {
				return
			}
			if cms, err := syscall.ParseSocketControlMessage(oob[:oobn]); err == nil {
				for _, cm := range cms {
					if fds, err := syscall.ParseUnixRights(&cm); err == nil {
						for _, fd := range fds {
							_ = syscall.Close(fd)
						}
					}
				}
			}
			s.msgs <- string(buf[:n])
		}
	}()
	return s
}

// mainPID 等下一条带 MAINPID= 的消息（READY 也要在里面），返回 pid
func (s *notifySink) mainPID(t *testing.T, wait time.Duration) int {
	t.Helper()
	deadline := time.After(wait)
	for {
		select {
		case m := <-s.msgs:
			var pid int
			if _, err := fmt.Sscanf(m, "MAINPID=%d", &pid); err == nil && strings.Contains(m, "READY=1") {
				return pid
			}
		case <-deadline:
			t.Fatalf("%s 内 systemd 没收到新的 MAINPID / READY", wait)
			return 0
		}
	}
}

// noMainPID 在 d 内没有新的 MAINPID
func (s *notifySink) noMainPID(t *testing.T, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case m := <-s.msgs:
			if strings.HasPrefix(m, "MAINPID=") {
				t.Fatalf("不该换主进程：systemd 收到 %q", m)
			}
		case <-deadline:
			return
		}
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// 端到端：systemd（Type=notify）下连着平滑重启，持续请求一个都不失败；重启以后开始的请求都由新版本处理；
// 进行中的慢请求照常做完；老长连接上的请求转给新进程；新版本起不来时什么都不变；SIGTERM 优雅退出。
func TestServeGracefulReloadUnderLoad(t *testing.T) {
	sink := newNotifySink(t)
	addr := freeAddr(t)
	versionFile := filepath.Join(t.TempDir(), "version")
	setVersion := func(v string) {
		if err := os.WriteFile(versionFile, []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	setVersion("v1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), "BASEKIT_SERVE_TEST=1", "BASEKIT_SERVE_TEST_ADDR="+addr,
		"BASEKIT_SERVE_TEST_VERSION="+versionFile, "NOTIFY_SOCKET="+sink.path)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	firstExited := make(chan error, 1)
	go func() { firstExited <- cmd.Wait() }()
	var pids []int
	t.Cleanup(func() {
		for _, pid := range pids {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	pid1 := sink.mainPID(t, 10*time.Second)
	pids = append(pids, pid1)
	if pid1 != cmd.Process.Pid {
		t.Fatalf("第一次启动 MAINPID=%d，应为 %d", pid1, cmd.Process.Pid)
	}

	get := func(c *http.Client, path string) (string, error) {
		resp, err := c.Get("http://" + addr + path)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			return "", err
		}
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, b)
		}
		return string(b), nil
	}
	version := func(body string) string { return strings.SplitN(body, "|", 2)[0] }

	// 持续请求：4 个并发（每次新连接，GET / POST 交替）只统计失败；1 个一个接一个地发，记下版本顺序
	fresh := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	var errs, total atomic.Int64
	var firstErr atomic.Value
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				var err error
				if (i+n)%2 == 0 {
					_, err = get(fresh, "/load")
				} else {
					var resp *http.Response
					if resp, err = fresh.Post("http://"+addr+"/post", "application/json", strings.NewReader(`{"a":1}`)); err == nil {
						_, _ = io.Copy(io.Discard, resp.Body)
						resp.Body.Close()
						if resp.StatusCode != http.StatusOK {
							err = fmt.Errorf("POST HTTP %d", resp.StatusCode)
						}
					}
				}
				total.Add(1)
				if err != nil {
					errs.Add(1)
					firstErr.CompareAndSwap(nil, err.Error())
				}
			}
		}(i)
	}
	var order []string
	var orderMu sync.Mutex
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if body, err := get(fresh, "/order"); err == nil {
				orderMu.Lock()
				order = append(order, version(body))
				orderMu.Unlock()
			}
		}
	}()
	time.Sleep(300 * time.Millisecond)

	// 交接前就连上的长连接，和一个交接时还在进行的慢请求
	keep, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer keep.Close()
	keepR := bufio.NewReader(keep)
	keepGet := func() (string, *http.Response) {
		t.Helper()
		_ = keep.SetDeadline(time.Now().Add(10 * time.Second))
		fmt.Fprintf(keep, "GET /keep HTTP/1.1\r\nHost: test\r\n\r\n")
		resp, err := http.ReadResponse(keepR, nil)
		if err != nil {
			t.Fatalf("长连接上的请求: %v", err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return string(b), resp
	}
	if body, _ := keepGet(); version(body) != "v1" {
		t.Fatalf("交接前长连接上 = %q", body)
	}
	slow := make(chan string, 1)
	go func() {
		body, err := get(&http.Client{Timeout: 10 * time.Second}, "/slow")
		if err != nil {
			body = "error: " + err.Error()
		}
		slow <- body
	}()
	time.Sleep(200 * time.Millisecond)

	// 1. 发布 v2：systemctl reload
	setVersion("v2")
	if err := syscall.Kill(pid1, syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	pid2 := sink.mainPID(t, 15*time.Second)
	pids = append(pids, pid2)
	if pid2 == pid1 {
		t.Fatal("平滑重启后 MAINPID 没变")
	}
	if body := <-slow; version(body) != "v1" {
		t.Fatalf("交接时进行中的慢请求 = %q，应由老进程做完", body)
	}
	body, resp := keepGet()
	if version(body) != "v2" {
		t.Fatalf("交接后老长连接上的请求 = %q，应转给新进程", body)
	}
	if !resp.Close {
		t.Fatal("转发的响应应当带 Connection: close")
	}
	select {
	case err := <-firstExited:
		if err != nil {
			t.Fatalf("老进程收尾后退出: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("老进程 10 秒内没退出（DrainTimeout 5 秒 + Shutdown）")
	}
	time.Sleep(300 * time.Millisecond)

	// 2. 新版本配置有错：reload 失败，主进程不变，照常服务 v2
	setVersion("fail")
	if err := syscall.Kill(pid2, syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	sink.noMainPID(t, 2*time.Second)
	if !alive(pid2) {
		t.Fatal("新版本起不来时当前进程不该退出")
	}
	if body, err := get(fresh, "/after-fail"); err != nil || version(body) != "v2" {
		t.Fatalf("reload 失败后 = %q, %v", body, err)
	}

	// 3. 修好以后再发布 v3
	setVersion("v3")
	if err := syscall.Kill(pid2, syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	pid3 := sink.mainPID(t, 15*time.Second)
	pids = append(pids, pid3)
	time.Sleep(500 * time.Millisecond)
	close(stop)
	wg.Wait()

	if n := errs.Load(); n != 0 {
		t.Fatalf("%d 个请求里失败 %d 个，第一个: %v", total.Load(), n, firstErr.Load())
	}
	if total.Load() < 100 {
		t.Fatalf("只发了 %d 个请求，压力不够", total.Load())
	}
	orderMu.Lock()
	rank := map[string]int{"v1": 1, "v2": 2, "v3": 3}
	last := 0
	for i, v := range order {
		if rank[v] < last {
			t.Fatalf("第 %d 个请求由 %s 处理：新版本开始接请求以后，老版本又接了新连接（顺序 %v）", i, v, order)
		}
		last = rank[v]
	}
	if last != 3 {
		t.Fatalf("最后由 %v 处理，应为 v3", order[len(order)-1])
	}
	orderMu.Unlock()
	t.Logf("连着 3 次 reload（1 次失败）期间 %d 个请求，0 失败", total.Load())

	// 4. systemctl stop：SIGTERM 给主进程，报 STOPPING=1 后退出
	deadline := time.Now().Add(15 * time.Second)
	for alive(pid2) && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if alive(pid2) {
		t.Fatal("交出去的 v2 进程收尾后没退出")
	}
	if err := syscall.Kill(pid3, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	stopping := false
	for !stopping {
		select {
		case m := <-sink.msgs:
			stopping = strings.Contains(m, "STOPPING=1")
		case <-time.After(5 * time.Second):
			t.Fatal("SIGTERM 后没报 STOPPING=1")
		}
	}
	deadline = time.Now().Add(10 * time.Second)
	for alive(pid3) && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if alive(pid3) {
		t.Fatal("SIGTERM 后 v3 进程没退出")
	}
}

// 不是 systemd（Type=notify）启动时 Serve 就是 app.Listen：地址默认取 server.host:server.port
func TestListenAddr(t *testing.T) {
	saved := config.C
	t.Cleanup(func() { config.C = saved })
	for _, c := range []struct {
		host string
		port int
		want string
	}{
		{"", 8080, ":8080"},
		{"127.0.0.1", 18080, "127.0.0.1:18080"},
		{"::1", 18080, "[::1]:18080"},
	} {
		config.C.Server.Host, config.C.Server.Port = c.host, c.port
		if got := ListenAddr(); got != c.want {
			t.Errorf("host %q port %d → %q，应为 %q", c.host, c.port, got, c.want)
		}
	}
}
