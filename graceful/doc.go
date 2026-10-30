// Package graceful 是平滑重启：systemctl reload（ExecReload 发 SIGHUP）时，当前进程把监听的 socket 交给一个新进程
// （同一个程序文件、同样的命令行参数；程序文件换过就是新版本，config.yaml 重新读），新进程准备好以后通知 systemd 换主进程
// （MAINPID），当前进程不再接新连接，处理完手上的请求再退出。整个过程 socket 一直开着，nginx 不会碰到连接被拒（502）。
//
// 交接是三步握手（两根管道）：新进程准备好（监听先暂停）→ 老进程停止 Accept、回「开始」→ 新进程通知 systemd、开始 Accept、
// 回「好了」。同一时刻只有一个进程在接新连接，中间新到的连接在内核队列里等几毫秒。
// 交接以后老进程已经建立的长连接上再来的请求，由 Forward 原样转给新进程（响应带 Connection: close），
// 所以交接以后开始的请求一律由新程序、新配置处理。
//
// 新进程起不来（配置错、程序有问题、ReadyTimeout 内没准备好）时老进程照常服务，Upgrade 返回错误。
// 老进程收尾期间新进程意外退出时，老进程把主进程要回来并退出，由 systemd 按 Restart= 重新拉起（见 watch.go）。
//
// 要 systemd 的 Type=notify、NotifyAccess=all、ExecReload=/bin/kill -HUP $MAINPID；不是这样启动的（没有 NOTIFY_SOCKET）
// 不做交接，Supervised 返回 false。一般不直接用本包：basekit.Run / basekit.Serve 在 Supervised 时自动走这一套。
//
// 只支持 unix（Linux、macOS）；其他平台上 Supervised 恒为 false，其余函数是空操作。
package graceful
