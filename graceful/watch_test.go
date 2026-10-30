//go:build unix

package graceful

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

// 盯着新进程的判断：只有「还是主进程 + 异常退出」才把主进程要回来
func TestWatchDecides(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	crash := errors.New("signal: killed")
	cases := []struct {
		name      string
		msgs      []byte
		err       error
		exitFirst bool // 退出比它退出前写的消息先被看到
		reclaim   bool
	}{
		{"还是主进程，被 kill -9", nil, crash, false, true},
		{"正常退出（systemctl stop：收尾后退出码 0）", nil, nil, false, false},
		{"交给下一代以后崩了：主进程是下一代，不能抢", []byte{'H'}, crash, false, false},
		{"交给下一代、下一代崩了又要回来以后自己也崩了", []byte{'H', 'R'}, crash, false, true},
		{"写完 H 马上崩了、退出先被看到", []byte{'H'}, crash, true, false},
		{"管道先关了、之后崩了", nil, crash, true, true},
	}
	for _, c := range cases {
		s := newState()
		exited := make(chan int, 1)
		s.exit = func(code int) { exited <- code }
		msgs := make(chan byte, 4)
		exit := make(chan error, 1)
		done := make(chan struct{})
		go func() { s.watch(4242, msgs, exit, nopCloser{}); close(done) }()
		send := func() {
			for _, b := range c.msgs {
				msgs <- b
			}
		}
		if c.exitFirst {
			exit <- c.err
			time.Sleep(50 * time.Millisecond)
			send()
		} else {
			send()
			time.Sleep(50 * time.Millisecond)
			exit <- c.err
		}
		close(msgs)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatalf("%s：watch 没返回", c.name)
		}
		got := false
		select {
		case code := <-exited:
			got = code == 1
		default:
		}
		if got != c.reclaim {
			t.Errorf("%s：要回主进程 %v，应为 %v", c.name, got, c.reclaim)
		}
	}
}

// waitMainPID 等 systemd 收到 MAINPID=，返回 pid
func waitMainPID(t *testing.T, msgs <-chan string) int {
	t.Helper()
	for {
		select {
		case m := <-msgs:
			var pid int
			if _, err := fmt.Sscanf(m, "MAINPID=%d", &pid); err == nil {
				return pid
			}
		case <-time.After(10 * time.Second):
			t.Fatal("没收到 MAINPID")
		}
	}
}

// noMainPID 在 d 内没收到 MAINPID=pid
func noMainPID(t *testing.T, msgs <-chan string, pid int, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case m := <-msgs:
			if strings.Contains(m, fmt.Sprintf("MAINPID=%d\n", pid)) || m == fmt.Sprintf("MAINPID=%d", pid) {
				t.Fatalf("不该把主进程要回来：systemd 收到 %q", m)
			}
		case <-deadline:
			return
		}
	}
}

func upgradedState(t *testing.T, mode string) (*state, chan int, string) {
	t.Helper()
	s := newState()
	exited := make(chan int, 1)
	s.exit = func(code int) { exited <- code }
	ln, err := s.listen("api", "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "parent") })}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	t.Setenv("GRACEFUL_TEST_CHILD", mode)
	if err := s.upgrade(20 * time.Second); err != nil {
		t.Fatalf("交接失败: %v", err)
	}
	return s, exited, ln.Addr().String()
}

// 收尾期间新进程被 kill -9：老进程把主进程要回来（MAINPID=自己，等 systemd 处理完）、做完 OnLost、非 0 退出
func TestSuccessorCrashDuringDrainReclaims(t *testing.T) {
	msgs, closeNotify := fakeSystemd(t)
	defer closeNotify()
	s, exited, _ := upgradedState(t, "serve")
	hooked := make(chan struct{}, 1)
	s.onLost(func() { hooked <- struct{}{} })
	child := waitMainPID(t, msgs)
	s.beginDrain()
	if err := syscall.Kill(child, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-exited:
		if code != 1 {
			t.Fatalf("退出码 %d，应为非 0", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("新进程被杀以后老进程没把主进程要回来")
	}
	if got := waitMainPID(t, msgs); got != os.Getpid() {
		t.Fatalf("systemd 收到 MAINPID=%d，应为本进程 %d", got, os.Getpid())
	}
	select {
	case m := <-msgs:
		if m != "BARRIER=1" {
			t.Fatalf("要回主进程以后应当等 systemd 处理完（BARRIER=1），收到 %q", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("没发 BARRIER=1")
	}
	select {
	case <-hooked:
	default:
		t.Fatal("OnLost 登记的没执行（任务队列的心跳没删）")
	}
}

// 收尾期间新进程把服务交给下一代、然后自己崩了：主进程是下一代，老进程不动；下一代照常服务
func TestSuccessorHandsOverThenExitsNoReclaim(t *testing.T) {
	msgs, closeNotify := fakeSystemd(t)
	defer closeNotify()
	s, exited, addr := upgradedState(t, "relay")
	relay := waitMainPID(t, msgs)
	grand := waitMainPID(t, msgs)
	defer func() { _ = syscall.Kill(grand, syscall.SIGTERM) }()
	if grand == relay || grand == os.Getpid() {
		t.Fatalf("下一代 MAINPID=%d（中间一代 %d）", grand, relay)
	}
	s.beginDrain()
	noMainPID(t, msgs, os.Getpid(), 2*time.Second) // 中间一代交出去以后退出码 3 退出
	select {
	case <-exited:
		t.Fatal("主进程已经是下一代了，老进程不该退出")
	default:
	}
	resp, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatalf("下一代没在服务: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "child" {
		t.Fatalf("由 %q 处理", b)
	}
}

// 收尾期间新进程正常退出（systemctl stop：SIGTERM，收尾后退出码 0）：老进程不动
func TestSuccessorStopsCleanlyNoReclaim(t *testing.T) {
	msgs, closeNotify := fakeSystemd(t)
	defer closeNotify()
	s, exited, _ := upgradedState(t, "serve")
	child := waitMainPID(t, msgs)
	s.beginDrain()
	_ = syscall.Kill(child, syscall.SIGTERM)
	noMainPID(t, msgs, os.Getpid(), 2*time.Second)
	select {
	case <-exited:
		t.Fatal("新进程正常退出，老进程不该要回主进程")
	default:
	}
}

// BARRIER=1：systemd 处理完（关掉带过去的 fd）才返回；不处理时按超时返回错误
func TestNotifyBarrier(t *testing.T) {
	msgs, closeNotify := fakeSystemd(t)
	if err := notifyBarrier("MAINPID=1", 5*time.Second); err != nil {
		t.Fatalf("systemd 处理了也报错: %v", err)
	}
	if m := <-msgs; m != "MAINPID=1" {
		t.Fatalf("先收到 %q", m)
	}
	if m := <-msgs; m != "BARRIER=1" {
		t.Fatalf("再收到 %q", m)
	}
	closeNotify()
	// 收消息的不关 fd（像 systemd 卡住了）：超时
	dir, err := os.MkdirTemp("/tmp", "gfb")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := dir + "/n.sock"
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	t.Setenv("NOTIFY_SOCKET", path)
	if err := notifyBarrier("MAINPID=1", 300*time.Millisecond); err == nil || !strings.Contains(err.Error(), "没处理完") {
		t.Fatalf("没处理时应当超时，得到 %v", err)
	}
}
