//go:build unix

package graceful

import (
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	// envFDs 是交给新进程的监听 socket 的名字，逗号分隔，fd 从 3 开始按这个顺序排
	envFDs = "BASEKIT_LISTEN_FDS"
	// envReady 是新进程通知老进程的管道 fd：准备好了写 'P'，开始服务了写 'N'；之后交给下一代写 'H'，要回主进程写 'R'
	envReady = "BASEKIT_READY_FD"
	// envGo 是老进程通知新进程「已经停止接新连接，开始吧」（写 'G'）的管道 fd
	envGo = "BASEKIT_GO_FD"
)

type state struct {
	mu        sync.Mutex
	inherited map[string]*os.File
	names     []string
	listeners map[string]*gate
	ready     *os.File // 新进程：写给老进程
	goPipe    *os.File // 新进程：老进程写过来
	child     bool
	upgrading bool
	// handedOver：已经让新进程开始接新连接，本进程长连接上来的新请求转给它（见 Forward）
	handedOver atomic.Bool
	// lostHooks / exit：新进程在本进程收尾期间意外退出时用（见 watch.go）；exit 测试里换掉
	lostHooks []func()
	exit      func(code int)

	drainOnce sync.Once
	drain     chan struct{}
}

var std = newState()

func newState() *state {
	s := &state{inherited: map[string]*os.File{}, listeners: map[string]*gate{}, drain: make(chan struct{}), exit: os.Exit}
	if names := os.Getenv(envFDs); names != "" {
		for i, name := range strings.Split(names, ",") {
			s.inherited[name] = os.NewFile(uintptr(3+i), name)
		}
	}
	if fd, err := strconv.Atoi(os.Getenv(envReady)); err == nil && fd > 2 {
		s.ready = os.NewFile(uintptr(fd), "ready")
		s.child = true
	}
	if fd, err := strconv.Atoi(os.Getenv(envGo)); err == nil && fd > 2 {
		s.goPipe = os.NewFile(uintptr(fd), "go")
	}
	// 只给自己用：本进程再往下起的子进程不该看到
	_ = os.Unsetenv(envFDs)
	_ = os.Unsetenv(envReady)
	_ = os.Unsetenv(envGo)
	return s
}

// Supervised 报告是不是由 systemd 以 Type=notify 启动的（有 NOTIFY_SOCKET），只有这样才能平滑重启。
func Supervised() bool { return os.Getenv("NOTIFY_SOCKET") != "" }

// Inherited 报告是不是从上一个进程接手的（平滑重启起来的新进程）。
func Inherited() bool { return std.child }

// Listen 返回名为 name 的监听 socket：从上一个进程继承了就用继承的，否则新监听 addr。Upgrade 时按名字交给下一个进程。
// 平滑重启起来的新进程里，监听先暂停（Accept 等着），Ready 时老进程停止接新连接以后才放行。
//
// 继承的 socket 地址和 addr 不一样（配置里改了监听地址）时照样用继承的、打一条警告：换地址要 systemctl restart。
func Listen(name, network, addr string) (net.Listener, error) { return std.listen(name, network, addr) }

func (s *state) listen(name, network, addr string) (net.Listener, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.listeners[name]; dup {
		return nil, fmt.Errorf("graceful: 监听 %s 重复注册", name)
	}
	var ln net.Listener
	var err error
	if f, ok := s.inherited[name]; ok {
		ln, err = net.FileListener(f)
		_ = f.Close() // FileListener 自己 dup 了一份
		delete(s.inherited, name)
		if err != nil {
			return nil, fmt.Errorf("graceful: 继承的 %s 用不了: %w", name, err)
		}
		if !sameAddr(ln.Addr(), network, addr) {
			log.Printf("graceful: 监听 %s 是从上一个进程接手的 %s，和现在配置的 %s 不一样；监听地址要 systemctl restart 才会换",
				name, ln.Addr(), addr)
		}
	} else if ln, err = net.Listen(network, addr); err != nil {
		return nil, err
	}
	g := newGate(ln, s.child)
	s.names = append(s.names, name)
	s.listeners[name] = g
	return g, nil
}

// sameAddr 报告监听着的地址 a 是不是配置的 addr（端口相同，主机相同或都是「所有地址」）。
func sameAddr(a net.Addr, network, addr string) bool {
	want, err := net.ResolveTCPAddr(network, addr)
	got, ok := a.(*net.TCPAddr)
	if err != nil || !ok {
		return true // 不是 TCP（或解析不了）就不比了
	}
	if got.Port != want.Port && want.Port != 0 {
		return false
	}
	if want.IP == nil || want.IP.IsUnspecified() {
		return got.IP == nil || got.IP.IsUnspecified()
	}
	return want.IP.Equal(got.IP)
}

func (s *state) pauseAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, g := range s.listeners {
		g.pause()
	}
}

func (s *state) resumeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, g := range s.listeners {
		g.resume()
	}
}

// Notify 给 systemd 发状态（sd_notify），没有 NOTIFY_SOCKET 时什么都不做。
func Notify(state string) error {
	sock := os.Getenv("NOTIFY_SOCKET")
	if sock == "" {
		return nil
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: sock, Net: "unixgram"})
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write([]byte(state))
	return err
}

// Ready 在服务的初始化都做完、开始 Serve 以后调用（要在本进程全部 Listen 之后）。平滑重启起来的新进程：告诉老进程准备好了，
// 等它停止接新连接，再把自己报成 systemd 的主进程、放行监听，最后告诉老进程可以退了。第一次启动：报 READY=1。
// 先报 systemd 后通知老进程：顺序反了的话，老进程退出时 systemd 还以为它是主进程，会当成服务挂了去重启。
func Ready() error { return std.readyUp(60 * time.Second) }

func (s *state) readyUp(wait time.Duration) error {
	if s.child {
		s.handshake(wait)
	}
	if err := Notify(fmt.Sprintf("MAINPID=%d\nREADY=1", os.Getpid())); err != nil {
		s.closePipes() // 不写就关：老进程当成交接失败，恢复接新连接、杀掉本进程
		return fmt.Errorf("graceful: 通知 systemd 失败: %w", err)
	}
	s.resumeAll()
	// 写不进去不算失败：本进程已经是主进程、在服务了。
	// 管道留着：以后本进程再交接给下一代（'H'）或者把主进程要回来（'R'）时告诉上一代，见 watch.go
	s.tellParent('N')
	if s.goPipe != nil {
		_ = s.goPipe.Close()
		s.goPipe = nil
	}
	return nil
}

// handshake 告诉老进程准备好了，等它回「开始」。老进程不在了（管道断了）或者 wait 内没回，也照样开始服务。
func (s *state) handshake(wait time.Duration) {
	if _, err := s.ready.Write([]byte{'P'}); err != nil || s.goPipe == nil {
		return
	}
	got := make(chan struct{})
	go func() {
		b := make([]byte, 1)
		_, _ = s.goPipe.Read(b)
		close(got)
	}()
	select {
	case <-got:
	case <-time.After(wait):
	}
}

func (s *state) closePipes() {
	for _, f := range []**os.File{&s.ready, &s.goPipe} {
		if *f != nil {
			_ = (*f).Close()
			*f = nil
		}
	}
}

// Upgrade 起一个新进程接手全部监听 socket，等它 Ready（最多 timeout）。返回 nil 后调用方应 BeginDrain 并在处理完手上的请求后退出；
// 返回错误时新进程已经被杀掉（或自己退出了），当前进程照常服务。
func Upgrade(timeout time.Duration) error { return std.upgrade(timeout) }

func (s *state) upgrade(timeout time.Duration) error {
	if !Supervised() {
		return errors.New("不是 systemd（Type=notify）启动的，没法换主进程：请用 systemctl restart")
	}
	s.mu.Lock()
	if s.upgrading {
		s.mu.Unlock()
		return errors.New("已经在交接了")
	}
	if s.isDraining() {
		s.mu.Unlock()
		return errors.New("当前进程已经在退出")
	}
	s.upgrading = true
	names := append([]string(nil), s.names...)
	files := make([]*os.File, 0, len(names)+1)
	var err error
	for _, name := range names {
		var f *os.File
		if f, err = dupListener(s.listeners[name]); err != nil {
			break
		}
		files = append(files, f)
	}
	s.mu.Unlock()
	defer func() {
		for _, f := range files {
			_ = f.Close()
		}
		s.mu.Lock()
		s.upgrading = false
		s.mu.Unlock()
	}()
	if err != nil {
		return fmt.Errorf("复制监听 socket: %w", err)
	}

	readyR, readyW, err := os.Pipe()
	if err != nil {
		return err
	}
	// 交接成功以后 readyR 交给 watch 接着读（新进程之后还会写 'H' / 'R'），失败时关掉
	handed := false
	defer func() {
		if !handed {
			_ = readyR.Close()
		}
	}()
	goR, goW, err := os.Pipe()
	if err != nil {
		_ = readyW.Close()
		return err
	}
	defer goW.Close()
	exe := os.Args[0]
	if !strings.HasPrefix(exe, "/") {
		if exe, err = os.Executable(); err != nil {
			_ = readyW.Close()
			_ = goR.Close()
			return err
		}
	}
	// 同样的命令行参数：同一个程序里起的不同服务（子命令）各自交接给自己的新进程
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Env = append(os.Environ(), envFDs+"="+strings.Join(names, ","),
		envReady+"="+strconv.Itoa(3+len(files)), envGo+"="+strconv.Itoa(4+len(files)))
	cmd.ExtraFiles = append(files, readyW, goR)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err = cmd.Start()
	_ = readyW.Close()
	_ = goR.Close()
	if err != nil {
		return fmt.Errorf("启动新进程: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	msgs := make(chan byte, 2)
	go func() {
		defer close(msgs)
		b := make([]byte, 1)
		for {
			if _, err := readyR.Read(b); err != nil {
				return
			}
			msgs <- b[0]
		}
	}()
	pid := cmd.Process.Pid
	fail := func(why error) error {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
		}
		return why
	}
	// 1. 新进程初始化完（连库、迁移、注册路由），监听还暂停着
	select {
	case b, ok := <-msgs:
		if !ok || b != 'P' {
			// 管道关了多半是新进程退出了（配置错、起不来）：等一下退出状态，报得准一点
			select {
			case err := <-exited:
				return fmt.Errorf("新进程（pid %d）退出了: %v，看上面它的日志", pid, err)
			case <-time.After(time.Second):
			}
			return fail(fmt.Errorf("新进程（pid %d）没准备好就关了管道（不支持平滑重启的旧版本？），看上面它的日志", pid))
		}
	case err := <-exited:
		return fmt.Errorf("新进程（pid %d）退出了: %v，看上面它的日志", pid, err)
	case <-time.After(timeout):
		return fail(fmt.Errorf("新进程（pid %d）%s 内没准备好", pid, timeout))
	}
	// 2. 本进程停止接新连接（新连接在内核队列里等），让新进程开始接
	// 从这里起本进程长连接上来的新请求都转给新进程（排在监听队列里，新进程开始接就处理）；交接失败时转过去的还在
	// 队列里，本进程恢复接新连接以后自己处理
	s.pauseAll()
	s.handedOver.Store(true)
	undo := func() {
		s.handedOver.Store(false)
		s.resumeAll()
	}
	if _, err := goW.Write([]byte{'G'}); err != nil {
		undo()
		return fail(fmt.Errorf("通知新进程（pid %d）开始失败: %w", pid, err))
	}
	// 3. 新进程已经报给 systemd、开始接新连接
	select {
	case b, ok := <-msgs:
		if ok && b == 'N' {
			handed = true
			s.tellParent('H')
			go s.watch(pid, msgs, exited, readyR)
			return nil
		}
		undo()
		return fail(fmt.Errorf("新进程（pid %d）没能开始服务（通知 systemd 失败？），看上面它的日志", pid))
	case err := <-exited:
		undo()
		return fmt.Errorf("新进程（pid %d）开始服务前退出了: %v，看上面它的日志", pid, err)
	case <-time.After(30 * time.Second):
		undo()
		return fail(fmt.Errorf("新进程（pid %d）30 秒内没开始服务", pid))
	}
}

// dupListener 复制监听 socket 的 fd 给新进程。不用 TCPListener.File()：它返回的 os.File 在 exec 传 fd（Fd()）时
// 会把 socket 改成阻塞模式，这个标志是新旧两个 fd 共享的，老进程的 accept 会卡住。直接 dup 再 os.NewFile 就不会改。
func dupListener(g *gate) (*os.File, error) {
	sc, ok := g.Listener.(syscall.Conn)
	if !ok {
		return nil, fmt.Errorf("%T 不支持取 fd", g.Listener)
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return nil, err
	}
	var dup int
	var derr error
	if err := raw.Control(func(fd uintptr) { dup, derr = dupCloexec(int(fd)) }); err != nil {
		return nil, err
	}
	if derr != nil {
		return nil, derr
	}
	return os.NewFile(uintptr(dup), "listener"), nil
}

// BeginDrain 开始退出：之后的响应带 Connection: close，Draining() 关闭。可以重复调用。
// 监听不在这里关，由 app.ShutdownWithTimeout 关（交接后已经暂停、不接新连接了；没有新进程时 Shutdown 一关新连接就被拒）：
// 提前关掉的话，Shutdown 里 fasthttp 再关一次报 net.ErrClosed、不等进行中的请求直接返回；fasthttp 的 Serve 也会先返回，
// GetOpenConnectionsCount 少算一个，交接后收尾时剩最后一个进行中的请求就当成没有连接了。
func BeginDrain() { std.beginDrain() }

func (s *state) beginDrain() {
	s.drainOnce.Do(func() { close(s.drain) })
}

// Draining 在开始退出（交接成功或收到退出信号）时关闭。不能和新进程并行的后台任务据此停止取新活。
func Draining() <-chan struct{} { return std.drain }

// IsDraining 报告是否已经开始退出。
func IsDraining() bool { return std.isDraining() }

func (s *state) isDraining() bool {
	select {
	case <-s.drain:
		return true
	default:
		return false
	}
}
