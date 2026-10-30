# 更新日志

base-kit 的版本记录。格式参考 Keep a Changelog，版本号遵循语义化版本。

版本规则：MINOR 只增不改（新配置键带默认值、新种子、新接口）；数据库变更只增列不删列；
破坏性变更走 `/v2` 模块路径。

## [Unreleased]

## [1.6.0] - 2026-10-30

### 新增

- 配置 `database.seed`：启动时种子数据的写法。默认 `full`（空串同），与此前完全一样。
  - `insert`：只补缺。缺的角色、菜单（含按钮）、用户、部门、配置项照建；已有的一律不改。不清理软删行、不删遗留演示菜单、
    生产模式不删 `admin` / `jack`、不把菜单重新授给 super / admin 角色。
  - `off`：不写任何种子数据，只做 AutoMigrate（只加表、加列）。
  - 用在和别的系统共用 `sys_*` 表的库上（例如新老后台切换期间），免得每次启动按种子改写别人的菜单、把全部菜单
    授给 `admin` 角色、删掉叫 `admin` 的账号、物理删除软删的角色和用户。
  - `Options.Seed` 在三种模式下都会被调用（模板垫片要在里面拿到 `DB`）。kit 的种子助手跟随模式：
    `SyncSeedMenus` / `SyncSeedMenu` 在 `insert` 下只建缺的、在 `off` 下什么都不写，`RefreshRoleMenus`、
    `RemoveLegacySeedMenus` 只在 `full` 下生效。下游 Seed 里自己写的数据用 `store.CurrentSeedMode()` 判断。
  - 取值写错（不是 full / insert / off）时启动失败，不会悄悄按 full 跑。
- 操作日志扩展列：`middleware.RegisterOperationLogExtra(func(c *fiber.Ctx, cols map[string]any))` 在 handler
  执行完后调用，往 `cols` 里放「列名 → 值」（如业务备注、响应体），和基底字段在同一条 INSERT 里写进
  `sys_operation_logs`。列要先用扩展结构加到表上；字符串整理成合法 UTF-8 并拷贝，超过 64KB 截断；基底字段不能覆盖，
  非 snake_case 的列名忽略；扩展列写不进去时退回只写基底字段，日志不丢。没登记时写库路径不变。

### 修复

- 扩展结构（「给基底的表加列」）与基底模型同表时，基底的列不再丢失。此前 `Options.Models` 和基底模型放在同一次
  AutoMigrate 里，GORM 按表名去重、只迁移后一个结构：新库上 `sys_operation_logs` 只建出扩展列，操作日志全部写失败；
  老库上基底新加的列不再补。现在与基底同表的下游模型在基底模型之后单独迁移。
- `login_captcha` 配置项这一行不存在时按种子的默认值判断（生产开、开发关），不再一律视为关闭；存在但停用仍视为关闭。
  `full` 模式下这一行总会被种上，行为不变；`off` 模式下新建的 `sys_configs` 是空表，生产上验证码默认开启。

### 升级步骤

1. `cd server && go get github.com/leivorn/base-kit@v1.6.0`。默认 `full`，不改配置就与 1.5.1 行为一致，无需其他操作。
2. 库和别的系统共用时，在 `config.yaml` 的 `database` 段加 `seed: off`（或 `insert`）。`off` 只适合已经有超管账号的库：
   新库用 `off` 启动没有任何账号，进不去后台。
3. 以前为绕开同表扩展的迁移问题、在 Seed 里自己 AutoMigrate 扩展结构的，可以挪回 `Options.Models`。

## [1.5.1] - 2026-10-26

### 修复

- `SyncSeedMenu` / `SyncSeedMenus` 更新已存在的菜单时补上 Vben meta 字段：`active_icon`、`active_path`、
  `hide_in_menu`、`hide_in_breadcrumb`、`hide_in_tab`、`hide_children_in_menu`、`badge_type`、`badge`、
  `badge_variants`。此前这些字段只在首次建菜单时写入：下游先建了菜单，后来在 `Options.Seed` 里补上
  `HideInMenu: true` / `ActivePath` 把它改成独立详情页的隐藏菜单，已有的库不生效，侧栏照样显示。现在它们和
  path、title 等字段一样，每次启动按种子定义写回（改回 `false` / 空串也能写回）；`status` 仍不同步。

### 升级步骤

1. `cd server && go get github.com/leivorn/base-kit@v1.5.1`。
2. 升级后首次启动会按种子覆盖上面这些字段。在「菜单管理」里给种子菜单（kit 自带的仪表盘、系统管理各菜单，
   或下游 `Options.Seed` 里登记的菜单）手动设过隐藏、激活路径、徽章等的，会被改回种子定义的值（kit 自带菜单
   都是默认值）；要保留就写进种子定义。没手改过的无需操作。

## [1.5.0] - 2026-10-13

### 模块路径

- 模块路径由 `github.com/xsxs89757/base-kit` 改为 **`github.com/leivorn/base-kit`**:GitHub 账号 xsxs89757 改名为
  leivorn,旧用户名不再由本人持有,不能再作为模块来源。代码与 1.4.0 相同,只改了路径。
- 1.4.0 及更早版本仍可按旧路径从 Go 模块代理(proxy.golang.org)取到,已钉版本的项目不会立刻坏;但旧路径不会再有新版本。

#### 升级步骤

1. `cd server && go get github.com/leivorn/base-kit@v1.5.0`,再把代码里的 `github.com/xsxs89757/base-kit` 全部替换为
   `github.com/leivorn/base-kit`,`go mod tidy` 去掉旧路径。
2. 生成 Swagger 的 `--packagePrefix` 里的旧路径一并替换。

## [1.4.0] - 2026-10-08

### 安全

- 菜单的外链（`link`）与内嵌页（`iframeSrc`）地址只允许 `http(s)://` 链接或以 `/` 开头的站内路径，新增、
  修改时校验（400），并去掉首尾空白。此前任意值都能存：有菜单编辑权限的人填 `javascript:` 地址，超管点开
  菜单时脚本在后台域名下执行，能读走 localStorage 里的 token，等于借菜单提权到超管。库里已有的危险地址不再
  通过 `/admin/menu/all` 下发给前端（日志打 WARN），菜单管理列表仍原样展示，方便改掉。
- 删除、清空操作日志只允许超级管理员（内置超管或持 super 角色），持 `System:OperationLog:Delete` 的普通管理员
  返回 403：审计记录不应由被审计的人抹掉。日常清理用配置 `server.op_log_retention_days` 按保留期自动删除。

#### 升级步骤

1. 启动后看日志有没有 `has an unsafe link` / `has an unsafe iframeSrc` 的 WARN，有就到「菜单管理」把对应菜单的
   地址改成 `https://` 链接或站内路径（这些菜单的外链在改好前点不开）。
2. 原来靠普通管理员清理操作日志的，改为配置 `server.op_log_retention_days`（生产建议 90），或交给超管操作。

## [1.3.0] - 2026-10-06

### 安全

- 登录验证码真正生效，做成拼图滑块：系统配置 `login_captcha` 此前只种了配置项、没有任何代码读它。现在为
  `true` 时登录前必须把拼图块拖进缺口。新增公开接口 `GET /admin/auth/captcha`（返回 `enabled`，开启时另有
  带缺口的背景图、拼图块与其纵坐标）和 `POST /admin/auth/captcha/verify`（提交横坐标，通过返回 2 分钟有效的
  一次性 `token`），`POST /admin/auth/login` 新增 `captchaToken` 字段。缺口位置只存在服务端（vben 自带的滑块
  组件是纯前端校验，不能直接用）；拼图只能提交一次，拖错计入来源 IP 的登录失败次数（随机乱拖约 4% 能蒙中，
  不限次数就等于没有）。背景图由服务端随机绘制，不依赖图片素材。生产模式新建的库默认开启，已有库保留原值。
- 口令强度策略：建用户、管理员重置、本人改密统一要求至少 8 位（系统配置 `password_min_length` 此前同样
  没人读，现在可在 8 的基础上调高）、同时含字母和数字、不在常见弱口令表里、不含用户名，不合格返回 400。
- 生产模式拒绝弱口令登录：密码正确但不满足上面的规则（按固定 8 位基线）的账号按登录失败处理（403，与密码
  错误同一提示），服务端日志打 WARN。
- 生产模式启动时删除演示账号 `admin` / `jack`（不论密码是否改过）及其角色关联，线上只保留内置超管。
- 新增 `<server> reset-password [用户名]` 子命令（`basekit.Run` 处理）：把账号（默认 `super`）重置为随机
  强口令并打印，用于超管忘记密码或因弱口令被拦在登录页外。
- 生产首建超管的随机密码保证同时含字母和数字（此前约 5% 的概率是纯字母）。

#### 升级步骤

1. 生产库里的 `admin` / `jack` 会在升级后首次启动时被删除。还在用这两个账号的，先在后台另建账号并分配角色。
2. 生产环境里口令不达标的账号升级后登录不进去：让超管在后台「用户管理」里重置成合格密码。超管自己不达标时，
   在服务目录执行 `./server reset-password` 拿新密码。
3. 想开登录验证码的存量库，在后台「系统配置」把 `login_captcha` 改成 `true`。前端要先同步带拼图验证的
   基底登录页（基底 CHANGELOG 里「base-kit v1.3.0」那一节），旧登录页不会弹拼图，开了就登录不了。
   只升 kit、没同步前端的项目新建生产库时，`login_captcha` 默认是 `true`，要先在库里改成 `false`。

### 修复

- 种子配置项补上配置名称，已有库里名称为空的启动时回填：后台编辑配置要求名称必填，此前种子配置在后台根本改不了
  （保存报「ConfigName不能为空」，这条校验消息也补成了中文「配置名称不能为空」）。

- 种子角色判重改为包含软删记录，只在真正建成功时打 `role created` 日志：此前删掉 admin/user 角色后，
  每次启动都会打一行误导性的 created（实际被唯一索引挡下）；没有唯一索引的库则会真的复活。

## [1.2.0] - 2026-10-02

### 安全

- 生产模式（`server.mode: production`）首次建库时，内置超管 `super` 改用 20 位随机初始密码，只在启动日志里
  打印一次；不再用 `123456` 建号。
- 生产模式拒绝用默认种子密码 `123456` 登录：存量库里沿用默认密码的账号，哈希对得上也按登录失败处理，
  服务端日志打 WARN。升级前先把这类账号的密码改掉，否则会被挡在门外。
- 后台登录加失败锁定：窗口 15 分钟内，同一用户名+来源失败 5 次、同一来源失败 20 次、同一用户名失败 30 次，
  任一维度达到阈值即锁 15 分钟，返回 429。来源 IP 只在直连方是本机反代时才读 `X-Real-IP`。
- 种子账号判重改为包含软删记录：管理员删掉的 admin/jack 不会在下次启动时按默认密码复活。

## [1.1.0] - 2026-09-15

### 新增

- `basekit.Go(app, fn)`：起一个随 app 关闭而停止的后台任务。`app.Shutdown()` 时 ctx 取消，等任务返回
  （最多 10 秒）后 Shutdown 才返回；`basekit.AppContext(app)` 只取这个 ctx。用法见 README「后台任务」。
  下游在 `Routes` 里用 `go fn(context.Background())` 起常驻 goroutine 时，测试里每次 `NewApp` 都会叠一份：
  `store.DB` 是包级变量，前一个 app 的任务不会退出，而是转去读写新 app 的库。下游真实案例：一个测试包里
  7 个测试各 `NewApp` 一次，回调投递 worker 同时跑 7 份，同一条回调发两遍，CI 时好时坏。

### 修复

- 同一进程多次 `NewApp`（测试）时，`go test -race` 偶发报数据竞争：操作日志的清理 goroutine 读
  `config.C`，下一次 `Bootstrap` 同时在重写它。现在启动时读好保留天数再传给 goroutine，行为不变。

## [1.0.4] - 2026-09-14

### 修复

- 下游在 `/admin` 下重复挂 `JWTAuth` / `PermissionAuth` / `OperationLog` 时，每个写操作记两条操作日志，
  鉴权和权限码也各跑两遍。`SetupAdmin` 把这三个中间件挂在 `/admin` 前缀上，`Routes` 里注册的 `/admin` 路由
  本来就经过它们，而模板 `router/project.go` 的注释让下游「参考 admin.go 中 protected 分组的中间件挂法」再挂一遍；
  `PreRoutes` 的分组上挂了、请求又落到后面 kit 或 `Routes` 的路由，也是两遍。现在三个中间件同一请求只生效一次，
  已经重复挂载的下游升级后不改代码也只记一条。`PermissionAuth` 按 method + path 记放行，
  内部重定向（`c.Path(...)` 后 `RestartRouting`）到别的路由照样重新判定。
- 操作日志在 MySQL 上记不下文件上传。multipart 请求体是二进制，utf8mb4 列在严格模式下拒绝整条 INSERT
  （`Error 1366 Incorrect string value: '\x89PNG...'`），writer 只打一行日志，这条记录就没了；SQLite 上一切正常，
  所以本地看不出来。现在上传请求改记表单摘要：普通字段照录（敏感字段照样脱敏，JSON 字符串字段里的也脱敏），
  文件只记文件名、大小和类型，例如 `{"category":"头像","file":{"filename":"a.png","size":60016,"contentType":"image/png"}}`。
- 同一根因的另外几种丢日志：请求体按字节截到 2KB，会切开中文等多字节字符（超过 2KB 的中文表单大概率整条丢失）；
  path / User-Agent 超出列宽触发 `Error 1406`。现在按字符边界截断，所有列写库前统一替换非法 UTF-8、去掉 NUL、
  按列宽截断。PostgreSQL 上是同样的问题（`SQLSTATE 22021` / `22001`），一并修好。
- 操作日志条目直接持有 Fiber 返回的字符串，它们指向会被后续请求复用的缓冲区，而日志是后台 writer 异步写库的，
  写库时可能已经变成别的请求的内容：并发时 method / path / User-Agent 会记错（实测 `DELETE /system/user/1`
  被记成 `POSTTE /system/role/2`）。现在入队前拷贝。

### 文档

- README「覆盖基底的接口」的示例漏挂中间件：`PreRoutes` 排在 kit 的 `/admin` 中间件前面，照抄出来的接口
  不用登录就能调。示例补上三个中间件；新增「下游路由的中间件」一节，说明哪些路由已经挂好、哪些要自己挂。
  `SetupAdmin`、`Options.PreRoutes` / `Routes` 和三个中间件的注释同步写明。

## [1.0.3] - 2026-09-02

### 变更

- 生产模式（`server.mode: production`）下 5xx 响应不再回显内部错误原文，对外统一返回
  `Internal Server Error`，原文进日志。recover 中间件把 panic 也转成这类错误，之前 DB 错误、文件路径
  会原样出现在响应体里。开发模式行为不变，4xx（404/405 等）不受影响。

### 修复

- `basekit-migrate` 的推荐用法改为在 `server/` 下不带 `@latest` 运行，版本跟随 go.mod 里钉的 kit。
  `@latest` 在 GOPROXY 有延迟的几分钟里会拿到旧版——而 v1.0.1 正好带着「改写 `base/internal/store`」那个 bug。
- `basekit-migrate` 结束时提示 import 分组未重排（kit 路径留在原来 `base/...` 那一组），用 `goimports -w .` 归位。

### 文档

- `go get -u` 改为 `go get ...@latest`：`-u` 会把 kit 的依赖一并升到最新 minor，与模板钉死版本的初衷相反。
- `Bootstrap` 不传 `Seed` 时模板垫片里的 `store.DB` 为 nil，README 点明。

## [1.0.2] - 2026-09-02

### 修复

- `basekit-migrate` 不再改写 `base/internal/store`。模板 v2.0.0 之后仍有一个同名的本地包
  （数据层挂载点的垫片，转发 `DB` / `IsUniqueViolation` 并给 `project.go` 提供 `syncSeedMenu`），
  改写会让 `main.go` 里的 `store.ProjectModels` / `store.ProjectSeed` 变成 undefined。
  顺带的好处：下游写 `store.DB`、`store.IsUniqueViolation` 的地方一个字都不用改。
  拿脚手架建的下游走完整升级流程时发现的。

## [1.0.1] - 2026-09-02

### 新增

- `basekit-migrate` 结束时检查 kit 接管的目录里是否还留着下游自己的 `.go` 文件，
  列出来并说明怎么处理。拿真实下游试跑时发现的：一个项目在 6 个基底目录里放了 48 个自己的文件，
  改写后引用方指向 kit，那些函数全部 `undefined`，报错信息看不出根因。
- `cmd/basekit-migrate` 补测试：映射表命中与不命中（`base/internal/router` 和下游自建包必须原样保留）、
  残留目录提示（空目录和未改写的包不报）。

## [1.0.0] - 2026-09-02

首个稳定版。API 与 v0.1.0 一致，配套 [base](https://github.com/leivorn/base) 模板 v2.0.0
（模板的 `server/go.mod` 从此钉这个版本）。

从这个版本起遵守上面的版本规则：MINOR 只增不改，数据库变更只增列，破坏性变更走 `/v2`。
框架层的 bug 修复和新功能不必等模板发版，下游 `cd server && go get github.com/leivorn/base-kit@latest` 即可。

### 验证

- 模板 `server/main_test.go` 冒烟测试（启动 → 登录 → 用户信息 → 菜单 → Swagger）通过。
- 用 `--parseDependencyLevel 3 --packagePrefix base,github.com/leivorn/base-kit` 重新生成的
  `swagger.json` 与抽库前逐字节一致。

## [0.1.0] - 2026-09-02

从 [leivorn/base](https://github.com/leivorn/base) 的 `server/internal/` 抽出框架层与系统管理模块，
首个可用版本（模板尚未切换过来，等 v1.0.0 一起发布）。

### 新增

- 根包 `basekit`：`Options` / `Bootstrap` / `NewApp` / `Run`，取代模板里手写的 `main.go` 装配逻辑。
  下游扩展点：`Models`、`Seed`、`PreRoutes`（覆盖基底接口）、`Routes`、`Swagger`、`Fiber`。
- `store.Init(Options) error`：不再 `log.Fatal`，由调用方决定进程存亡；模型与种子数据通过参数传入，
  取代模板时代靠同包文件 `project.go` 的编译期挂载。
- 导出种子助手 `SyncSeedMenus` / `SyncSeedMenu` / `RefreshRoleMenus` / `RemoveLegacySeedMenus` 和 `MenuDef`。
- `config.Path()` / `config.LoadExtra(dst)`：下游把自己的配置段读进自己的结构体。
- `cmd/basekit-migrate`：go/ast 导入路径改写工具，下游从模板迁移时跑一次。
- `store/embed_test.go` 锁定「给基底表加列」的扩展方式，`store/mysql_test.go` 在真实 MySQL 上验证迁移与种子。

### 已知约束

- 给 `sys_users` 等基底表加列时，扩展结构**只能声明表名、主键和新列**，不能嵌入 `adminmodel.User`：
  嵌入会把 `Roles` many2many 带过来，往共享的 `user_roles` 表加一列 `<结构名>_id`，
  通过嵌入结构写入时 `user_id` 为 NULL，kit 的 handler 按 `user_id` 查角色会静默查不到。详见 README。
