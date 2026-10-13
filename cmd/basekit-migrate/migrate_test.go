package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRewriteFileMapsKitPackagesOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "handler.go")
	write(t, path, `package biz

import (
	"base/config"
	admindto "base/internal/dto/admin"
	"base/internal/router"
	"base/internal/store"
	"base/internal/util"
)

var _ = config.C
var _ = admindto.LoginRequest{}
var _ = router.SetupProject
var _ = store.DB
var _ = util.Ping
`)

	seen := map[string]bool{}
	changed, err := rewriteFile(path, false, false, seen)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("应报告文件被改写")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := string(got)
	for _, want := range []string{
		`"github.com/leivorn/base-kit/config"`,
		`admindto "github.com/leivorn/base-kit/dto/admin"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("缺少改写结果 %s\n实际:\n%s", want, out)
		}
	}
	// router / store 是挂载点（模板里仍有同名本地包），util 是下游自己的包，都必须原样留下
	for _, keep := range []string{`"base/internal/router"`, `"base/internal/store"`, `"base/internal/util"`} {
		if !strings.Contains(out, keep) {
			t.Errorf("%s 不该被改写\n实际:\n%s", keep, out)
		}
	}
	if !seen["base/config"] || !seen["base/internal/dto/admin"] {
		t.Errorf("seen 未记录命中的旧路径: %v", seen)
	}
	for _, unmapped := range []string{"base/internal/router", "base/internal/store"} {
		if seen[unmapped] {
			t.Errorf("seen 不该记录未映射的路径 %s", unmapped)
		}
	}
}

func TestRewriteFileSkipsUnrelatedImports(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plain.go")
	body := "package biz\n\nimport \"fmt\"\n\nvar _ = fmt.Sprint\n"
	write(t, path, body)

	seen := map[string]bool{}
	changed, err := rewriteFile(path, false, false, seen)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("没有可改写的 import 时不该报告改动")
	}
	got, _ := os.ReadFile(path)
	if string(got) != body {
		t.Errorf("文件不该被重写:\n%s", got)
	}
}

// 下游在 kit 接管的目录里放过自己的文件时必须给出提示：
// 改写后引用方指向 kit，那些文件里的函数会 undefined，错误信息看不出根因。
func TestWarnLeftoversListsDownstreamFiles(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "internal/handler/admin/article.go"), "package admin\n")
	write(t, filepath.Join(root, "internal/handler/admin/order.go"), "package admin\n")
	write(t, filepath.Join(root, "internal/middleware/casbin.go"), "package middleware\n")
	// 目录空了就不该报
	if err := os.MkdirAll(filepath.Join(root, "internal/dto"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 没被改写过的包不该报，即使目录里有文件
	write(t, filepath.Join(root, "internal/model/admin/member.go"), "package admin\n")

	seen := map[string]bool{
		"base/internal/handler/admin": true,
		"base/internal/middleware":    true,
		"base/internal/dto":           true,
	}
	var buf bytes.Buffer
	warnLeftovers(&buf, []string{root}, seen)
	out := buf.String()

	for _, want := range []string{"internal/handler/admin", "article.go", "order.go", "internal/middleware", "casbin.go"} {
		if !strings.Contains(out, want) {
			t.Errorf("提示里缺少 %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "internal/dto") {
		t.Errorf("空目录不该出现在提示里:\n%s", out)
	}
	if strings.Contains(out, "member.go") {
		t.Errorf("未被改写的包不该出现在提示里:\n%s", out)
	}
}

func TestWarnLeftoversSilentWhenClean(t *testing.T) {
	root := t.TempDir()
	var buf bytes.Buffer
	warnLeftovers(&buf, []string{root + "/..."}, map[string]bool{"base/internal/dto": true})
	if buf.Len() != 0 {
		t.Errorf("没有残留时不该有输出:\n%s", buf.String())
	}
}
