//go:build unix

package graceful

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// 测试程序自己当「新进程」：GRACEFUL_TEST_CHILD=serve 时接手 api 监听、回答 child、Ready 后服务到收到 SIGTERM；
// =fail 时直接退出（模拟新版本配置错误起不来）；=notify-fails 时通知 systemd 失败（老进程已经暂停接新连接以后才失败）；
// =relay 时接手后马上再交给下一代（serve），然后自己崩溃；=no-handshake 时不握手、直接关管道（不支持平滑重启的旧版本）
func TestMain(m *testing.M) {
	switch os.Getenv("GRACEFUL_TEST_CHILD") {
	case "relay":
		// 中间一代：接手、服务，再把服务交给下一代（serve），交出去以后崩溃（之后它怎么退出都不归上一代管）
		ln, err := Listen("api", "tcp", "127.0.0.1:0")
		if err != nil {
			os.Exit(3)
		}
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "relay") })}
		go func() { _ = srv.Serve(ln) }()
		if err := Ready(); err != nil {
			os.Exit(4)
		}
		time.Sleep(200 * time.Millisecond)
		_ = os.Setenv("GRACEFUL_TEST_CHILD", "serve")
		if err := Upgrade(20 * time.Second); err != nil {
			fmt.Fprintln(os.Stderr, "relay upgrade:", err)
			os.Exit(5)
		}
		os.Exit(3)
	case "notify-fails":
		ln, err := Listen("api", "tcp", "127.0.0.1:0")
		if err != nil {
			os.Exit(3)
		}
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "child") })}
		go func() { _ = srv.Serve(ln) }()
		time.Sleep(200 * time.Millisecond) // 让老进程暂停的时间长一点，期间的请求要排在队列里等老进程恢复
		_ = os.Setenv("NOTIFY_SOCKET", "/nonexistent/notify.sock")
		if err := Ready(); err == nil {
			os.Exit(5)
		}
		os.Exit(4)
	case "serve":
		ln, err := Listen("api", "tcp", "127.0.0.1:0")
		if err != nil {
			fmt.Fprintln(os.Stderr, "child listen:", err)
			os.Exit(3)
		}
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "child") })}
		go func() { _ = srv.Serve(ln) }()
		if err := Ready(); err != nil {
			fmt.Fprintln(os.Stderr, "child ready:", err)
			os.Exit(4)
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
		defer stop()
		select {
		case <-ctx.Done():
		case <-time.After(30 * time.Second):
		}
		_ = srv.Close()
		os.Exit(0)
	case "fail":
		os.Exit(1)
	case "no-handshake":
		_ = std.ready.Close()
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// waitOpen 等打开的连接数降到 0，最多 max（basekit.Serve 里按 fasthttp 的 GetOpenConnectionsCount 等）
func waitOpen(open func() int32, max time.Duration) {
	deadline := time.Now().Add(max)
	for open() > 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
}

// fakeSystemd 收 sd_notify 的消息
func fakeSystemd(t *testing.T) (<-chan string, func()) {
	dir := t.TempDir()
	if len(dir) > 80 { // macOS 的 unix socket 路径上限约 104 字节，测试临时目录太长时换个短的
		var err error
		if dir, err = os.MkdirTemp("/tmp", "gfs"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
	}
	path := filepath.Join(dir, "notify.sock")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	msgs := make(chan string, 16)
	go func() {
		buf := make([]byte, 4096)
		oob := make([]byte, 256)
		for {
			n, oobn, _, _, err := conn.ReadMsgUnix(buf, oob)
			if err != nil {
				return
			}
			// BARRIER=1 带着一个管道的写端：和 systemd 一样，处理到这里就关掉它（发送方据此知道前面的消息都处理完了）
			if cms, err := syscall.ParseSocketControlMessage(oob[:oobn]); err == nil {
				for _, cm := range cms {
					if fds, err := syscall.ParseUnixRights(&cm); err == nil {
						for _, fd := range fds {
							_ = syscall.Close(fd)
						}
					}
				}
			}
			msgs <- string(buf[:n])
		}
	}()
	t.Setenv("NOTIFY_SOCKET", path)
	return msgs, func() { _ = conn.Close() }
}

func TestUpgradeHandsOverListenerWithoutErrors(t *testing.T) {
	msgs, closeNotify := fakeSystemd(t)
	defer closeNotify()
	s := newState()
	s.exit = func(int) { t.Error("新进程正常退出，不该要回主进程") } // 默认是 os.Exit，会把测试进程带走
	ln, err := s.listen("api", "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	var open atomic.Int32 // 和生产一样按打开的连接数等空闲（fasthttp 的 GetOpenConnectionsCount）
	parent := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "parent") }),
		ConnState: func(_ net.Conn, st http.ConnState) {
			switch st {
			case http.StateNew:
				open.Add(1)
			case http.StateClosed, http.StateHijacked:
				open.Add(-1)
			}
		}}
	go func() { _ = parent.Serve(ln) }()

	// 交接前后一直发请求（每次新连接），一个都不能失败
	var errs, fromParent, fromChild atomic.Int64
	var firstErr atomic.Value
	stop := make(chan struct{})
	var wg sync.WaitGroup
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	// 另有一个客户端一个接一个地发：同一时刻只有一个进程在接新连接，所以一旦新进程回答过，后面就不能再是老进程
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
			resp, err := client.Get("http://" + addr + "/")
			if err != nil {
				continue // 失败由上面的并发客户端统计
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			orderMu.Lock()
			order = append(order, string(b))
			orderMu.Unlock()
		}
	}()
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				resp, err := client.Get("http://" + addr + "/")
				if err != nil {
					errs.Add(1)
					firstErr.CompareAndSwap(nil, err.Error())
					continue
				}
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				switch string(b) {
				case "parent":
					fromParent.Add(1)
				case "child":
					fromChild.Add(1)
				}
			}
		}()
	}
	time.Sleep(300 * time.Millisecond)
	t.Setenv("GRACEFUL_TEST_CHILD", "serve")
	if err := s.upgrade(20 * time.Second); err != nil {
		t.Fatalf("交接失败: %v", err)
	}
	var childPID int
	select {
	case m := <-msgs:
		if !strings.Contains(m, "READY=1") || !strings.HasPrefix(m, "MAINPID=") {
			t.Fatalf("systemd 收到 %q", m)
		}
		_, _ = fmt.Sscanf(m, "MAINPID=%d", &childPID)
	case <-time.After(5 * time.Second):
		t.Fatal("新进程没通知 systemd")
	}
	defer func() {
		if p, err := os.FindProcess(childPID); err == nil {
			_ = p.Signal(syscall.SIGTERM)
		}
	}()
	if childPID == os.Getpid() || childPID == 0 {
		t.Fatalf("MAINPID = %d，应是新进程", childPID)
	}
	// 老进程不再接新连接；已经接下的连接处理完、关掉以后再 Shutdown（直接 Shutdown 会关掉刚接下还没读请求的连接）
	s.beginDrain()
	waitOpen(open.Load, 5*time.Second)
	if n := open.Load(); n != 0 {
		t.Errorf("WaitIdle 返回时还有 %d 个连接", n)
	}
	_ = parent.Shutdown(context.Background())
	time.Sleep(500 * time.Millisecond)
	close(stop)
	wg.Wait()
	if errs.Load() != 0 {
		t.Fatalf("交接期间 %d 个请求失败，第一个: %v", errs.Load(), firstErr.Load())
	}
	if fromParent.Load() == 0 || fromChild.Load() == 0 {
		t.Fatalf("老进程处理 %d 个、新进程处理 %d 个：应当两边都有", fromParent.Load(), fromChild.Load())
	}
	orderMu.Lock()
	seenChild := false
	for i, who := range order {
		if who == "child" {
			seenChild = true
		} else if seenChild {
			t.Fatalf("第 %d 个请求：新进程已经开始接新连接以后，老进程还接了新连接（%d 个请求）", i, len(order))
		}
	}
	orderMu.Unlock()
	resp, err := client.Get("http://" + addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "child" {
		t.Fatalf("交接后由 %q 处理", b)
	}
}

func TestUpgradeChildFailsKeepsServing(t *testing.T) {
	_, closeNotify := fakeSystemd(t)
	defer closeNotify()
	s := newState()
	ln, err := s.listen("api", "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "parent") })}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()
	t.Setenv("GRACEFUL_TEST_CHILD", "fail")
	begin := time.Now()
	if err := s.upgrade(20 * time.Second); err == nil {
		t.Fatal("新进程起不来也算交接成功了")
	}
	if time.Since(begin) > 10*time.Second {
		t.Errorf("新进程退出后应当马上判失败，用了 %s", time.Since(begin))
	}
	if s.isDraining() {
		t.Fatal("交接失败后不该进入退出")
	}
	resp, err := http.Get("http://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatalf("交接失败后老进程应当照常服务: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "parent" {
		t.Fatalf("由 %q 处理", b)
	}
}

func TestUpgradeNeedsSystemdNotify(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	s := newState()
	if _, err := s.listen("api", "tcp", "127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	if err := s.upgrade(time.Second); err == nil || !strings.Contains(err.Error(), "Type=notify") {
		t.Fatalf("没有 NOTIFY_SOCKET 时应当拒绝交接: %v", err)
	}
}

// 新进程在老进程暂停接新连接以后才失败（通知 systemd 失败）：老进程恢复接新连接，暂停期间来的请求排队等着，不丢
func TestUpgradeChildFailsAfterPauseResumes(t *testing.T) {
	_, closeNotify := fakeSystemd(t)
	defer closeNotify()
	s := newState()
	ln, err := s.listen("api", "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "parent") })}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()
	var errs, served atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				resp, err := client.Get("http://" + addr + "/")
				if err != nil {
					errs.Add(1)
					continue
				}
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if string(b) != "parent" {
					t.Errorf("由 %q 处理（新进程没通知 systemd，不该开始服务）", b)
				}
				served.Add(1)
			}
		}()
	}
	t.Setenv("GRACEFUL_TEST_CHILD", "notify-fails")
	if err := s.upgrade(20 * time.Second); err == nil {
		t.Fatal("新进程通知 systemd 失败也算交接成功了")
	}
	before := served.Load()
	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
	if errs.Load() != 0 || served.Load() == before {
		t.Fatalf("交接失败后：%d 个请求失败，恢复后处理了 %d 个", errs.Load(), served.Load()-before)
	}
	if s.isDraining() {
		t.Fatal("交接失败后不该进入退出")
	}
}

// 暂停：正在等的 Accept 让出来，新连接留在队列里；恢复后接着接；暂停中关闭也能退出
func TestGatePauseResume(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := newGate(raw, false)
	accepted := make(chan net.Conn, 4)
	acceptErr := make(chan error, 1)
	go func() {
		for {
			c, err := g.Accept()
			if err != nil {
				acceptErr <- err
				return
			}
			accepted <- c
		}
	}()
	time.Sleep(50 * time.Millisecond) // 让 Accept 先等着
	g.pause()
	c1, err := net.Dial("tcp", raw.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	select {
	case <-accepted:
		t.Fatal("暂停中接了新连接")
	case <-time.After(200 * time.Millisecond):
	}
	g.resume()
	select {
	case c := <-accepted:
		c.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("恢复后没接排队的连接")
	}
	g.pause()
	_ = g.Close()
	select {
	case err := <-acceptErr:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("关闭后 Accept 返回 %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("暂停中关闭，Accept 没返回")
	}
}

// 新进程不握手就关了管道（不支持平滑重启的旧版本程序）：交接失败、新进程被杀，老进程照常服务
func TestUpgradeChildWithoutHandshakeFails(t *testing.T) {
	_, closeNotify := fakeSystemd(t)
	defer closeNotify()
	s := newState()
	ln, err := s.listen("api", "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "parent") })}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()
	t.Setenv("GRACEFUL_TEST_CHILD", "no-handshake")
	begin := time.Now()
	if err := s.upgrade(20 * time.Second); err == nil {
		t.Fatal("新进程不握手也算交接成功了")
	}
	if time.Since(begin) > 15*time.Second {
		t.Errorf("管道一关就该判失败，用了 %s", time.Since(begin))
	}
	resp, err := http.Get("http://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatalf("交接失败后老进程应当照常服务: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "parent" {
		t.Fatalf("由 %q 处理", b)
	}
}

// 继承的监听地址和配置不一样时只警告；sameAddr 的判断
func TestSameAddr(t *testing.T) {
	cases := []struct {
		got, want string
		same      bool
	}{
		{"127.0.0.1:18080", "127.0.0.1:18080", true},
		{"[::]:18080", ":18080", true},
		{"0.0.0.0:18080", ":18080", true},
		{"[::]:18080", "127.0.0.1:18080", false},
		{"127.0.0.1:18080", ":18080", false},
		{"127.0.0.1:18080", "127.0.0.1:18081", false},
	}
	for _, c := range cases {
		a, err := net.ResolveTCPAddr("tcp", c.got)
		if err != nil {
			t.Fatal(err)
		}
		if got := sameAddr(a, "tcp", c.want); got != c.same {
			t.Errorf("监听 %s、配置 %s：sameAddr = %v，应为 %v", c.got, c.want, got, c.same)
		}
	}
}
