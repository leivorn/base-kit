package basekit

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/leivorn/base-kit/store"

	"github.com/gofiber/fiber/v2"
)

// Shutdown 要等任务返回之后才返回：测试的 t.Cleanup 紧接着就关库，
// 任务还在跑的话会撞上已关闭的库，或者转去读写下一个测试的库。
func TestGoWaitsForTaskOnShutdown(t *testing.T) {
	app := fiber.New()
	var exited atomic.Bool
	Go(app, func(ctx context.Context) {
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond) // 收尾要花时间，Shutdown 得等它
		exited.Store(true)
	})

	if err := app.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if !exited.Load() {
		t.Fatal("Shutdown 返回时后台任务还没退出")
	}
}

// 每个 app 各管各的：关掉一个不能取消另一个的任务。
func TestAppContextPerApp(t *testing.T) {
	a, b := fiber.New(), fiber.New()
	ctxA, ctxB := AppContext(a), AppContext(b)
	if AppContext(a) != ctxA {
		t.Fatal("同一个 app 两次取到的 ctx 不是同一个")
	}

	_ = a.Shutdown()
	if ctxA.Err() == nil {
		t.Error("a 关闭后它的 ctx 没有取消")
	}
	if ctxB.Err() != nil {
		t.Error("关闭 a 把 b 的 ctx 也取消了")
	}

	_ = b.Shutdown()
	if ctxB.Err() == nil {
		t.Error("b 关闭后它的 ctx 没有取消")
	}
}

// 不看 ctx 的任务不能让 Shutdown 卡死，等到上限就放弃。
func TestShutdownWaitIsBounded(t *testing.T) {
	old := shutdownWait
	shutdownWait = 50 * time.Millisecond
	t.Cleanup(func() { shutdownWait = old })

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	app := fiber.New()
	Go(app, func(context.Context) { <-release })

	start := time.Now()
	_ = app.Shutdown()
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("Shutdown 等了 %s，应该在 %s 左右放弃", d, shutdownWait)
	}
}

// 下游的真实用法：Routes 里起 worker，测试里一个进程先后 NewApp 好几次。
// 前一个 app 关闭后它的 worker 必须已经退出，不能和下一个 app 的叠在一起跑。
func TestGoAcrossNewApp(t *testing.T) {
	var running atomic.Int32
	started := make(chan struct{}, 1)

	for i := 1; i <= 3; i++ {
		app, err := NewApp(Options{
			Config: testConfig(t, "development"),
			Routes: func(app *fiber.App) {
				Go(app, func(ctx context.Context) {
					running.Add(1)
					defer running.Add(-1)
					started <- struct{}{}
					<-ctx.Done()
				})
			},
		})
		if err != nil {
			t.Fatalf("第 %d 个 NewApp: %v", i, err)
		}

		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatalf("第 %d 个 app 的 worker 没有启动", i)
		}
		if n := running.Load(); n != 1 {
			t.Fatalf("第 %d 个 app 启动后有 %d 个 worker 在跑，期望 1 个", i, n)
		}

		if err := app.Shutdown(); err != nil {
			t.Fatalf("第 %d 个 app Shutdown: %v", i, err)
		}
		if n := running.Load(); n != 0 {
			t.Fatalf("第 %d 个 app 关闭后还有 %d 个 worker 在跑", i, n)
		}
		if sqlDB, err := store.DB.DB(); err == nil {
			sqlDB.Close()
		}
	}
}
