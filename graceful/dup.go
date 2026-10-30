//go:build unix

package graceful

import "syscall"

// dupCloexec 复制 fd（带 close-on-exec：别的子进程拿不到，exec 交给新进程时会单独放到 3、4… 号上）
func dupCloexec(fd int) (int, error) {
	r, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_DUPFD_CLOEXEC, 0)
	if errno != 0 {
		return -1, errno
	}
	return int(r), nil
}
