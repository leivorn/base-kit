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
（测试、自定义 Listener）时用 `basekit.NewApp`。

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

`basekit.Run` 收到退出信号时直接结束进程，这一点没变；生产上要优雅退出，自己接管信号并调 `app.ShutdownWithTimeout`。

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
