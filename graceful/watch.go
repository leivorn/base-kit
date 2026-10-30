//go:build unix

package graceful

import (
	"fmt"
	"io"
	"log"
	"os"
	"syscall"
	"time"
)

// 交接以后老进程还要收尾一阵（等长连接、等进行中的请求）。这段时间新进程是老进程的子进程，不是 systemd 的：
// 新进程崩了，退出信号由老进程收走（cmd.Wait），systemd 发现不了（journal 里那条 "Supervising process … which is not
// our child"；public-service 在服务器上实测过：老进程退出前一直显示 active、不拉起，监听 socket 只剩新进程那份，全部 502）。
// 所以老进程盯着新进程：新进程还是主进程、却异常退出时，老进程把主进程要回来（MAINPID=自己）、马上非 0 退出，
// systemd 收到主进程退出，按 Restart=always 重新拉起。老进程收尾结束退出以后新进程过继给 PID 1，systemd 自己就能发现。
//
// 判断依据（不能把主进程从更新的进程那里抢走）：
//   - 新进程把服务再交给下一代以后，通过同一根管道（BASEKIT_READY_FD）写 'H'；之后它退出（不管怎么退出）都不归本进程管：
//     主进程是下一代，下一代是它的子进程，由它盯着，它退出后下一代过继给 PID 1、由 systemd 盯着。
//   - 新进程自己又把主进程要回来（它的下一代崩了）时写 'R'，本进程重新把它当主进程盯着（连着几次交接、几代都还在收尾时，
//     一级一级往上要，直到 systemd 的子进程）。
//   - 退出码 0 是正常停服务（systemctl stop：SIGTERM，收尾后正常退出），不要回来。
//   - 只有「还是主进程 + 非 0 退出或被信号杀掉」才算意外退出。

// OnLost 登记新进程意外退出、本进程要回主进程后、退出前要做的事（比如删掉任务队列的心跳，让重新拉起的进程马上重跑本进程
// 手上的任务）。每个最多等 2 秒。
func OnLost(fn func()) { std.onLost(fn) }

func (s *state) onLost(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lostHooks = append(s.lostHooks, fn)
}

// watch 在交接成功以后盯着新进程，直到它退出。pipe 是新进程写消息的管道，读完关掉。
func (s *state) watch(pid int, msgs <-chan byte, exited <-chan error, pipe io.Closer) {
	defer pipe.Close()
	isMain := true
	apply := func(b byte) {
		switch b {
		case 'H':
			isMain = false
		case 'R':
			isMain = true
		}
	}
	for {
		select {
		case b, ok := <-msgs:
			if !ok {
				msgs = nil // 管道关了：之后只看退出
				continue
			}
			apply(b)
		case err := <-exited:
			// 退出可能比它退出前写的消息先看到：进程退出以后写端就关了，先把剩下的读完再判断
			timeout := time.After(time.Second)
		drain:
			for msgs != nil {
				select {
				case b, ok := <-msgs:
					if !ok {
						break drain
					}
					apply(b)
				case <-timeout:
					break drain
				}
			}
			if isMain && err != nil {
				s.reclaim(pid, err)
			}
			return
		}
	}
}

// reclaim 新进程意外退出：把主进程要回来、告诉上一代（它还在收尾的话）、做完 OnLost 登记的事，非 0 退出。
func (s *state) reclaim(pid int, why error) {
	log.Printf("平滑重启：新进程（pid %d）在本进程收尾期间意外退出（%v）；它不是 systemd 的子进程，systemd 发现不了。"+
		"本进程把主进程要回来、马上退出，由 systemd 按 Restart= 重新拉起", pid, why)
	if err := notifyBarrier(fmt.Sprintf("MAINPID=%d", os.Getpid()), 5*time.Second); err != nil {
		log.Printf("graceful: 把主进程要回来（MAINPID）失败: %v", err)
	}
	s.tellParent('R')
	s.mu.Lock()
	hooks := append([]func(){}, s.lostHooks...)
	s.mu.Unlock()
	for _, h := range hooks {
		done := make(chan struct{})
		go func() {
			defer close(done)
			h()
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}
	s.exit(1)
}

// tellParent 告诉上一代（交接给本进程的那个，还在收尾时）：'N' 开始服务了，'H' 交给下一代了，'R' 又要回来了。
// 上一代已经退出时写不进去，以后不再写。
func (s *state) tellParent(b byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ready == nil {
		return
	}
	if _, err := s.ready.Write([]byte{b}); err != nil {
		_ = s.ready.Close()
		s.ready = nil
	}
}

// notifyBarrier 给 systemd 发 state，再发 BARRIER=1（带一个管道的写端，systemd 处理完之前的消息就关掉它）等它处理完。
// 本进程发完 MAINPID 马上要退出：systemd 得先认下本进程是主进程，收到本进程退出时才会按 Restart= 拉起。
// 用没 connect 的 socket + sendmsg（和 libsystemd 的 sd_notify 一样）：Go 的 UnixConn 连上以后不让带 fd 发（WriteMsgUnix 报错）。
func notifyBarrier(state string, timeout time.Duration) error {
	sock := os.Getenv("NOTIFY_SOCKET")
	if sock == "" {
		return nil
	}
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	syscall.CloseOnExec(fd)
	to := &syscall.SockaddrUnix{Name: sock}
	if err := syscall.Sendmsg(fd, []byte(state), nil, to, 0); err != nil {
		return err
	}
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	defer r.Close()
	err = syscall.Sendmsg(fd, []byte("BARRIER=1"), syscall.UnixRights(int(w.Fd())), to, 0)
	_ = w.Close()
	if err != nil {
		return err
	}
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, r)
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("systemd %s 内没处理完（BARRIER）", timeout)
	}
}
