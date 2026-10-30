# base-kit

[base 基底](https://github.com/leivorn/base) 的后端框架层：配置、JWT 鉴权、菜单权限码 RBAC、
数据层与整套系统管理模块（用户 / 角色 / 菜单 / 部门 / 配置 / 操作日志）。

项目从 base 模板派生，模板负责 `main.go`、业务代码和整个前端；框架层的 bug 修复和新功能
走 `go get github.com/leivorn/base-kit@latest`，不必再靠 git merge 一个个文件合。

> 代码历史：这些包原先在 [leivorn/base](https://github.com/leivorn/base) 的 `server/internal/`
> 下，`bf985b6` 之前的提交记录在那个仓库里。

## 用法

```go
package main

import (
    "github.com/leivorn/base-kit"

    "base/internal/router"
    "base/internal/store"
)

func main() {
    basekit.Run(basekit.Options{
        Models: store.ProjectModels(), // 下游模型，并入 AutoMigrate
        Seed:   store.ProjectSeed,     // 下游种子数据
        Routes: router.Setup,          // 下游业务路由
    })
}
```

`basekit.Options` 的全部扩展点：

| 字段 | 用途 |
| --- | --- |
| `ConfigPath` / `Config` | 配置文件路径（默认 `config.yaml`）；`Config` 非空时直接注入，测试用 |
| `Models` | 追加到 AutoMigrate 的下游模型 |
| `Seed` | 基底种子数据之后执行，种下游的菜单与业务数据 |
| `PreRoutes` | 在基底 `/admin` 路由**之前**注册；Fiber 先注册先匹配，用来覆盖基底某个接口 |
| `Routes` | 在基底路由之后注册业务路由 |
| `Swagger` | `enable_swagger` 为 true 时调用，由下游挂载 UI（kit 不依赖 swag） |
| `Fiber` | `fiber.New` 之前调整配置 |

只要数据层（定时任务、命令行工具）时用 `basekit.Bootstrap`；要拿到 `*fiber.App` 自己控制监听
（测试、自定义 Listener）时用 `basekit.NewApp`；自己组装的 app 要监听地址和平滑重启时交给 `basekit.Serve`
（见[监听地址与平滑重启](#监听地址与平滑重启)）。

用 `Bootstrap` 时也把 `Seed: store.ProjectSeed` 传上（种子是幂等的）：模板垫片 `internal/store` 里的
`store.DB` 是在 `ProjectSeed` 里赋值的，不传 Seed 它就是 nil。或者干脆在这类工具里直接用 kit 的 `store.DB`。

## 给基底的表加列

**只声明表名、主键和新列**，登记到 `Options.Models`：

```go
type UserExtension struct {
    ID    uint   `gorm:"primarykey"`
    Dept  string `gorm:"size:64;index"`
    Level int    `gorm:"default:0"`
}

func (UserExtension) TableName() string { return "sys_users" }
```

AutoMigrate 只增不删，kit 先建表、这个结构随后迁移同一张表，只会补上新列（与基底同表的下游模型在基底模型之后
单独迁移；1.6.0 之前放在同一次 AutoMigrate 里，新库会只建出扩展列）。

**不要嵌入 `adminmodel.User`。** 嵌入会把 `Roles` many2many 一起带过来，GORM 按新结构体名
派生外键，往共享的 `user_roles` 表里加一列 `<新结构名>_id`；通过嵌入结构写入时 `user_id` 是 NULL，
而 kit 的 handler 按 `user_id` 查角色——角色关联会静默失效。加 `gorm:"-"` 屏蔽 `Roles` 也挡不住那一列。
约束由 `store/embed_test.go` 锁定。

改字段之外的需求（比如给用户加一段结构化档案），更推荐旁表：`biz_user_profiles{user_id uniqueIndex, ...}`，
与 kit 零耦合。

## 种子数据的写法

`config.yaml` 的 `database.seed` 控制每次启动时种子数据怎么写：

| 取值 | 行为 |
| --- | --- |
| `full`（默认，留空同） | 建缺的、按种子定义写回已有的菜单、新菜单授给 super/admin 角色、清理软删行与遗留演示菜单、生产模式删 `admin`/`jack` |
| `insert` | 只补缺：缺的照建，已有的一律不改、不删、不重新授权 |
| `off` | 不写任何种子数据，只做 AutoMigrate（只加表、加列） |

库只给本项目用时用默认值。和别的系统共用 `sys_*` 表时（例如新老后台切换期间）用 `insert` 或 `off`，免得每次启动
改到别人的菜单、角色授权和账号。`off` 只适合已经有超管账号的库，新库用它启动没有任何账号。

`Options.Seed` 三种模式下都会被调用（模板垫片要在里面拿到 `DB`）。`SyncSeedMenus` / `SyncSeedMenu` /
`RefreshRoleMenus` / `RemoveLegacySeedMenus` 自己跟随模式；下游 Seed 里自己写的数据要判断一下：

```go
func projectSeed() {
    if store.CurrentSeedMode() == store.SeedOff { // kitstore "github.com/leivorn/base-kit/store"
        return
    }
    // ...
}
```

`off` 模式下 `sys_configs` 可能没有任何配置项：`login_captcha` 缺行时按生产开、开发关，`password_min_length` 缺行时取 8。

## 操作日志的扩展列

给 `sys_operation_logs` 加了列（见上一节）之后，用 `middleware.RegisterOperationLogExtra` 往同一条日志里写：

```go
type remarksKey struct{}

// handler 里：c.Locals(remarksKey{}, "订单迁移：订单 1，原用户 2 → 迁移用户 3")
middleware.RegisterOperationLogExtra(func(c *fiber.Ctx, cols map[string]any) {
    if remarks, ok := c.Locals(remarksKey{}).(string); ok {
        cols["remarks"] = remarks
    }
    cols["resp"] = truncate(string(c.Response().Body()), 2048) // 列宽自己控制
})
```

取值函数在 handler 执行完、日志入队前调用，只对记日志的写操作调用；handler 返回 error 时响应体还是空的。
扩展列和基底字段在同一条 INSERT 里写入；基底字段不能覆盖；扩展列写不进去（比如表上没有这一列）时退回只写基底字段。
登记在 `Routes` 里做（`app.Listen` 之前，不加锁）。

## 覆盖基底的接口

Fiber 先注册先匹配，在 `PreRoutes` 里注册同样的方法和路径即可。`PreRoutes` 排在 kit 的 `/admin`
中间件前面，命中后不会再经过它们，**鉴权、权限码、操作日志要自己挂**，否则这个接口不用登录就能调：

```go
basekit.Run(basekit.Options{
    PreRoutes: func(app *fiber.App) {
        // 盖掉 kit 的实现
        app.Put("/admin/system/user/:id",
            middleware.JWTAuth(), middleware.PermissionAuth(), middleware.OperationLog(), myUpdateUser)
    },
})
```

## 下游路由的中间件

kit 把 `JWTAuth`、`PermissionAuth`、`OperationLog` 挂在 `/admin` **前缀**上（见 `router.SetupAdmin`），
之后注册的 `/admin` 路由不挂也会鉴权、校验权限码、记操作日志（POST/PUT/DELETE）：

| 路由 | 这三个中间件 |
| --- | --- |
| `Routes` 里的 `/admin/*` | 已经挂好，**不要再挂** |
| `PreRoutes` 里的 `/admin/*` | 不经过，要自己挂（见上一节） |
| `/api` 等其他前缀 | 不经过，需要时自己挂 |

```go
Routes: func(app *fiber.App) {
    shop := app.Group("/admin/shop") // 不要写成 app.Group("/admin/shop", middleware.JWTAuth(), ...)
    shop.Get("/order/list", listOrders)
    shop.Put("/order/:id", updateOrder)
},
```

重复挂载时三个中间件同一请求只生效一次，不会多记日志，但也没有意义。

## 下游路由的权限码

```go
middleware.RegisterRoutePermissions(
    middleware.RoutePermission{Method: "GET", Path: "/admin/shop/order/list", Code: "Shop:Order:List"},
)
```

未登记的 `/admin` 路由对非 super 用户一律 403（默认拒绝）。只需登录不校验权限码的用
`middleware.RegisterAuthenticatedRoutes`。注册要在 `basekit.Run` 之前或 `Routes` 回调里完成。

## 后台任务

定时扫描、投递 worker 这类常驻 goroutine，在 `Routes` 里用 `basekit.Go` 起，**不要**
`go fn(context.Background())`：

```go
Routes: func(app *fiber.App) {
    basekit.Go(app, callback.NewWorker(log).Run) // 签名是 func(ctx context.Context) 的直接传
    basekit.Go(app, func(ctx context.Context) {
        sweepSessions(ctx, log)
    })
},
```

`app.Shutdown()` 时 ctx 取消，等任务返回后 Shutdown 才返回（最多等 10 秒），所以任务要在 ctx 取消后
尽快返回。只要一个随 app 关闭而取消的 ctx（交给自己管 goroutine 的组件）时用 `basekit.AppContext(app)`。

`context.Background()` 的问题在测试里暴露：`store.DB` 是包级变量，每次 `NewApp` 都换成新库，
前一个 app 起的任务不会退出，而是转去读写新 app 的库。一个测试包里 N 个测试各 `NewApp` 一次，
同一个 worker 就同时跑 N 份——下游真实案例：回调投递 worker 叠了 7 份，同一条回调发两遍，CI 时好时坏。
测试里记得 `t.Cleanup(func() { _ = app.Shutdown() })`，并排在关库之后注册（`t.Cleanup` 后注册的先执行）。

不是 systemd 以 `Type=notify` 启动时，`basekit.Run` 收到退出信号直接结束进程，这一点没变；以 `Type=notify` 启动时
优雅退出、平滑重启，见下一节。平滑重启时老进程收尾期间（最多 `DrainTimeout`）它的任务还在跑（ctx 到最后 Shutdown 才取消，
免得收尾中的请求用到的连接被任务的清理逻辑关掉），和新进程起的同一份任务并存；不能并发跑两份的任务（定时扫描、投递）
在循环里看 `graceful.IsDraining()`，开始收尾就不再取新活。

## 监听地址与平滑重启

**监听地址**：`server.host`，默认空串＝所有网卡（与以前的 `:port` 一样）。在 nginx 之后的生产服务填 `127.0.0.1`，
别的机器连不上。环境变量 `SERVER_HOST` 优先于配置（可以写在 systemd 单元里）。

**平滑重启**：`basekit.Run` / `basekit.Serve` 发现自己是 systemd 以 `Type=notify` 启动的（有 `NOTIFY_SOCKET`）时自动启用，
不用改代码；别的启动方式（`make dev`、`Type=simple` 的单元、非 unix）与以前完全一样。单元这样写：

```ini
[Service]
Type=notify
NotifyAccess=all
ExecStart=/opt/app/server/app
ExecReload=/bin/kill -HUP $MAINPID
# 必须有：平滑重启后的主进程不是 systemd 起的（MAINPID 报上来的），systemd（252 实测）stop 时对它发完 SIGTERM 不等，
# 立刻 SIGKILL，进行中的请求全断。自己发 SIGTERM、等它退出，再交给 systemd 收尾
ExecStop=/bin/sh -c 'kill -TERM $MAINPID 2>/dev/null; while kill -0 $MAINPID 2>/dev/null; do sleep 0.2; done'
Restart=always
# 收尾：ShutdownTimeout 20 秒 + 后台任务 10 秒，留余量
TimeoutStopSec=60
```

- **就绪**：初始化（读配置、连库、迁移、注册路由）完、开始接请求后报 `READY=1`，`systemctl start` 等到这时才返回。
- **`systemctl reload`**（SIGHUP）：同一个程序文件、同样的命令行参数起新进程，监听 socket 交给它（一直开着，不会有连接被拒）。
  新进程准备好后，老进程不再接新连接、新进程开始接，并报给 systemd 换主进程（`MAINPID`）；老进程长连接上再来的请求原样转给新进程
  （响应带 `Connection: close`），进行中的请求照常做完，连接都关掉后（最多 `DrainTimeout`）退出。
  所以 **reload 之后开始的请求一律由新程序、新配置处理**。
- **新进程起不来**（配置错、程序崩、`ReadyTimeout` 内没准备好）：杀掉它，老进程照常服务，日志里有「平滑重启失败」。
  `systemctl reload` 只是发信号、立刻返回，**不代表成功**：发布脚本要看 `systemctl show -p MainPID --value <服务>` 换没换、再探活。
- **老进程收尾期间新进程崩了**：新进程这时是老进程的子进程，systemd 发现不了；老进程把主进程要回来并退出，由 `Restart=` 重新拉起。
- **`systemctl stop`**（SIGTERM）：报 `STOPPING=1`，不再接新连接，进行中的请求做完（最多 `ShutdownTimeout`）后退出。
- **发布**：新程序文件写到旁边再 `mv` 过去（换 inode；不要原地覆盖正在运行的文件），然后 `systemctl reload`。
- **要 `systemctl restart` 的情况**（会断开进行中的请求）：单元文件改了（`User`、沙箱、`Environment` 这些是 systemd 起进程时设的，
  reload 起的新进程继承老进程的）、监听地址改了（继承的 socket 不会换，日志里有警告）、单元第一次从 `Type=simple` 换成 `Type=notify`、
  回退到 1.7.0 之前的版本（它不会报 READY，单元也要一起换回去）。
- **不跨进程的内存状态**：登录失败计数、拼图验证码（2 分钟有效）、权限和用户缓存。reload 后计数清零；reload 前刚拿到、
  reload 后才提交的验证码会校验失败，重新拖一次即可。

时限在 `basekit.ServeOptions` 里调（`Run` 用默认值）：

| 字段 | 默认 | 含义 |
| --- | --- | --- |
| `Name` | `http` | 监听名，交接时按名字对应 |
| `Addr` | `server.host:server.port` | 监听地址 |
| `ReadyTimeout` | 90 秒 | 等新进程准备好的上限，超时算失败 |
| `DrainTimeout` | 70 秒 | 交接后老进程等连接关掉的上限（nginx upstream `keepalive_timeout` 默认 60 秒） |
| `ShutdownTimeout` | 20 秒 | 最后关闭时等进行中请求的上限 |

**同一个程序里的第二个服务**（比如消息网关）用子命令起，自己的 `fiber.App` 交给 `Serve`，单独一个 systemd 单元
（`ExecStart=/opt/app/server/app gateway`）。reload 时按同样的参数起新进程，两个服务各自交接、互不影响：

```go
func main() {
    if len(os.Args) > 1 && os.Args[1] == "gateway" {
        app := gateway.NewApp() // 自己读配置、自己组装的 *fiber.App
        if err := basekit.Serve(app, basekit.ServeOptions{Addr: "127.0.0.1:18090"}); err != nil {
            log.Fatal(err)
        }
        return
    }
    basekit.Run(basekit.Options{ /* … */ })
}
```

**紧急强制停止 / 重启**（资金、数据类 bug，老代码一刻都不能再跑）：平滑重启和 `systemctl stop` / `restart` 都会等进行中的请求做完
（老代码继续执行），紧急时绕开它们，直接对整个 cgroup 发 SIGKILL（主进程、收尾中的老进程、正在执行的 ExecStop / ExecReload 一起）。
杀之前先让单元「起不来」，免得 `Restart=always` 又拉起来：单元里加一条断言，紧急停止时先写这个标记文件：

```ini
[Unit]
AssertPathExists=!/opt/app/emergency/app.stopped
```

```bash
# 紧急停止：写标记（之后 Restart=、手工 start、开机都起不来）→ 全部进程立刻 SIGKILL → 状态收成 inactive
mkdir -p /opt/app/emergency && date >/opt/app/emergency/app.stopped
systemctl kill --signal=SIGKILL app; systemctl stop app
# 恢复 / 紧急重启（用盘上现在的程序文件）：先照上面杀干净，再
rm /opt/app/emergency/app.stopped; systemctl start app
```

不用 `systemctl mask --runtime`：单元装在 `/etc/systemd/system` 时，它的优先级高于 `/run` 里的屏蔽，屏蔽不生效。
进行中的请求会被切断（对方看到连接中断 / 502），不在一个事务里的多步操作可能只做了一半。确认 cgroup 里一个进程都不剩
（`/sys/fs/cgroup$(systemctl show -p ControlGroup --value app)/cgroup.procs` 为空）再做后续。下游可以把这几步包成脚本放在服务器上，
发布脚本加一个强制模式（装完程序直接紧急重启）；bj96weixin-go 的 `deploy/emergency.sh` 是一个现成的例子（含测试构建里的长请求接口和真实 systemd 测试）。

一个进程只调一次 `Serve`。要在一个进程里开多个监听，直接用 `graceful` 包（`Listen` 每个监听取一个名字，全部 `Listen`
之后再 `Ready`，SIGHUP 时 `Upgrade`，见包文档）。

## 登录安全

后台「系统配置」security 分组里的两项现在真正生效（现查库，后台改完立即生效）：

| 配置键 | 作用 |
| --- | --- |
| `login_captcha` | `true` 时登录前必须完成拼图滑块验证：前端 `GET /admin/auth/captcha` 拿带缺口的背景图和拼图块，拖动后 `POST /admin/auth/captcha/verify` 提交横坐标，通过拿到一次性 `token`，登录时作为 `captchaToken` 带上。缺口位置只在服务端，拼图只能提交一次、拖错计入来源 IP 的失败次数。生产新库默认开，开发库默认关 |
| `password_min_length` | 设密码时的最小长度，只能在 8 的基础上调高 |

口令强度（建用户、管理员重置、本人改密）：至少 8 位、同时含字母和数字、不是常见弱口令、不含用户名。
生产模式下登录时也按这套规则（固定 8 位基线，不跟随上面的配置）再查一遍：存量弱口令账号哪怕密码对，
也按登录失败处理，由超管在后台重置。生产模式启动时还会删除残留的演示账号 `admin` / `jack`。

超管自己被拦在外面（弱口令、忘记密码）时，在服务目录下以服务的运行用户执行：

```bash
./server reset-password          # 默认 super；也可指定用户名
```

它读同一份 `config.yaml`，把密码重置为随机强口令并打印出来。

## 从模板迁移

基底 v2.0.0 之前，这些包在模板的 `server/internal/` 下。同步到 v2.0.0 后跑一次导入路径改写：

```bash
cd server
go run github.com/leivorn/base-kit/cmd/basekit-migrate ./...   # 版本跟随 go.mod 里钉的 kit
go mod tidy
```

`base/internal/router` 和 `base/internal/store` 不在改写范围内：模板 v2.0.0 之后
仍有这两个同名的本地包（路由挂载点，和转发 `DB` / `IsUniqueViolation` / `syncSeedMenu` 的数据层垫片），
所以 `store.DB`、`store.IsUniqueViolation` 这些写法一个字都不用改。
kit 里 store 的其余能力（`SyncSeedMenus`、`RemoveLegacySeedMenus` 等）需要时直接 import kit。

### 自己往基底目录里加过文件的情况

如果之前在 `internal/handler/admin/`、`internal/model/admin/` 这类目录里放过自己的文件
（真实下游里见过一个目录 15 个），同步之后基底只删了自己那份，你的文件会留在原地。
这时同一个 import 路径下有两个来源：本地包里你的函数，和 kit 包里基底的函数——
改写把引用方指向了 kit，你的函数就会 `undefined`，而错误信息看不出根因。

改写工具会在结束时把这些目录列出来。两种处理方式：

1. **把它们挪到自己的包**（推荐），比如 `internal/handler/biz/`，再改引用方的 import。
   以后基底再动这些目录也不会碰到你。
2. 保留原地，把引用方改成同时 import kit 包和本地包，各起一个别名。

顺带一提，如果你的文件用到了基底同包里的**未导出**标识符（比如测试里的 `newTestApp`），
那些标识符现在在 kit 里且不可见，只能自己补一份。

## 版本

语义化版本。MINOR 只增不改（新配置键带默认值、新种子、新接口）；数据库变更只增列不删列；
破坏性变更走 `/v2` 模块路径。变更记录见 [CHANGELOG.md](CHANGELOG.md)。
