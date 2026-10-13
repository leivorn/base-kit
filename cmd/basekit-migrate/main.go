// basekit-migrate 把 base 模板里的 import 路径改写成 base-kit 的路径。
//
// 基底 v2.0.0 把框架层和系统管理模块搬到了 github.com/leivorn/base-kit，
// 下游 make sync-base 之后跑一次这个工具，业务代码里的 import 就自动改好了：
//
//	cd server && go run github.com/leivorn/base-kit/cmd/basekit-migrate ./...
//
// 不带 @latest：在 server/ 下运行时版本跟随 go.mod 里钉的 kit，和库本身一致；
// @latest 在 GOPROXY 有延迟的几分钟里会拿到旧版。
//
// 用 go/ast 改写而不是 sed：sed -i 在 BSD/GNU 下参数不同（dev.sh 还要支持
// Windows Git Bash），而且正则容易误伤字符串字面量和注释。
package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const kit = "github.com/leivorn/base-kit"

// 搬到 kit 的包。key 是模板里的旧路径，value 是 kit 里的新路径。
// 注意顺序无关：查表是精确匹配，不做前缀替换。
//
// 两个包故意不在表里，因为模板 v2.0.0 之后仍然有同名的本地包：
//   - base/internal/router：路由挂载点 SetupProject 就在里面；
//   - base/internal/store：数据层挂载点，垫片提供 DB / IsUniqueViolation / ProjectModels /
//     ProjectSeed / syncSeedMenu / refreshRoleMenus。下游写 store.DB、store.IsUniqueViolation
//     的地方一个字都不用改，改写反而会把 main.go 里的 store.ProjectModels 弄丢。
var importMap = map[string]string{
	"base/config":                   kit + "/config",
	"base/internal/dto":             kit + "/dto",
	"base/internal/dto/admin":       kit + "/dto/admin",
	"base/internal/validator":       kit + "/validator",
	"base/internal/validator/admin": kit + "/validator/admin",
	"base/internal/model":           kit + "/model",
	"base/internal/model/admin":     kit + "/model/admin",
	"base/internal/middleware":      kit + "/middleware",
	"base/internal/service/admin":   kit + "/service/admin",
	"base/internal/handler/admin":   kit + "/handler/admin",
}

func main() {
	dryRun := flag.Bool("n", false, "只列出会改动的文件，不写回")
	verbose := flag.Bool("v", false, "打印每处改写")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `用法: basekit-migrate [-n] [-v] [路径...]

把 base 模板的 import 路径改写为 base-kit 的路径（默认处理当前目录，递归）。
「./...」和目录名等价，都表示递归处理。

映射:
`)
		for _, k := range sortedKeys(importMap) {
			fmt.Fprintf(os.Stderr, "  %-32s -> %s\n", k, importMap[k])
		}
		fmt.Fprintf(os.Stderr, "\nbase/internal/router 和 base/internal/store 不在映射里：两个挂载点仍留在模板中。\n")
	}
	flag.Parse()

	roots := flag.Args()
	if len(roots) == 0 {
		roots = []string{"."}
	}

	changed := 0
	seen := map[string]bool{}
	for _, root := range roots {
		root = strings.TrimSuffix(strings.TrimSuffix(root, "/..."), "/")
		if root == "" {
			root = "."
		}
		n, err := walk(root, *dryRun, *verbose, seen)
		if err != nil {
			fmt.Fprintf(os.Stderr, "错误: %v\n", err)
			os.Exit(1)
		}
		changed += n
	}

	switch {
	case changed == 0:
		fmt.Println("没有需要改写的 import")
	case *dryRun:
		fmt.Printf("%d 个文件需要改写（-n 未写回）\n", changed)
	default:
		fmt.Printf("已改写 %d 个文件，接着执行: cd server && go mod tidy\n", changed)
		// 只改路径不重排分组：改写后的 kit 路径还留在原来 base/... 那一组里，编译没问题，看着别扭
		fmt.Println("import 分组未重排（kit 路径留在原来 base/... 那一组），要归位跑: goimports -w .")
	}

	warnLeftovers(os.Stderr, roots, seen)
}

// warnLeftovers 检查 kit 接管的包目录里是否还留着下游自己的 .go 文件。
//
// 下游可能在 internal/handler/admin/ 这类目录里加过自己的文件（真实案例：一个下游有 15 个）。
// 基底 v2.0.0 只删自己那份，下游的文件会留下来，于是同一个 import 路径下存在两个来源：
// 本地包里下游自己的函数，和 kit 包里基底的函数。改写 import 后，引用下游那些函数的地方
// 会指向 kit 并报 undefined——错误信息里看不出根因，所以在这里显式提示。
func warnLeftovers(w io.Writer, roots []string, seen map[string]bool) {
	type leftover struct {
		dir   string
		files []string
	}
	var found []leftover
	for _, old := range sortedKeys(importMap) {
		if !seen[old] {
			continue
		}
		// "base/internal/handler/admin" -> "internal/handler/admin"，再挂到各个 root 下面找
		rel := strings.TrimPrefix(old, "base/")
		for _, root := range roots {
			root = strings.TrimSuffix(strings.TrimSuffix(root, "/..."), "/")
			if root == "" {
				root = "."
			}
			dir := filepath.Join(root, rel)
			entries, err := os.ReadDir(dir)
			if err != nil {
				continue
			}
			var files []string
			for _, e := range entries {
				if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
					files = append(files, e.Name())
				}
			}
			if len(files) > 0 {
				found = append(found, leftover{dir: dir, files: files})
			}
		}
	}
	if len(found) == 0 {
		return
	}
	fmt.Fprintf(w, "\n注意: 下面的目录已由 base-kit 接管，但仍留着文件——应该是你自己加的:\n")
	for _, lo := range found {
		fmt.Fprintf(w, "  %s: %s\n", lo.dir, strings.Join(lo.files, " "))
	}
	fmt.Fprintf(w, `
这些文件本身没被改动，但引用它们的地方 import 已经指向 kit 了，编译会报 undefined。
两种处理方式，任选其一:
  1. 把它们挪到自己的包（推荐），比如 internal/handler/biz/，再改引用方的 import；
  2. 保留原地，把引用方改成同时 import kit 包和本地包（各起一个别名）。
`)
}

func walk(root string, dryRun, verbose bool, seen map[string]bool) (int, error) {
	changed := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// vendor / 隐藏目录 / node_modules 一律跳过
			name := d.Name()
			if name != "." && name != ".." && (strings.HasPrefix(name, ".") || name == "vendor" || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		ok, err := rewriteFile(path, dryRun, verbose, seen)
		if err != nil {
			return err
		}
		if ok {
			changed++
		}
		return nil
	})
	return changed, err
}

func rewriteFile(path string, dryRun, verbose bool, seen map[string]bool) (bool, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return false, fmt.Errorf("解析 %s: %w", path, err)
	}

	modified := false
	for _, spec := range file.Imports {
		old, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		new, ok := importMap[old]
		if !ok {
			continue
		}
		spec.Path.Value = strconv.Quote(new)
		modified = true
		seen[old] = true
		if verbose {
			fmt.Printf("  %s: %s -> %s\n", path, old, new)
		}
	}
	if !modified {
		return false, nil
	}

	fmt.Println(path)
	if dryRun {
		return true, nil
	}

	// 重排 import 分组，避免留下 gofmt 不干净的文件
	ast.SortImports(fset, file)
	var buf strings.Builder
	if err := format.Node(&buf, fset, file); err != nil {
		return false, fmt.Errorf("格式化 %s: %w", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if err := os.WriteFile(path, []byte(buf.String()), info.Mode().Perm()); err != nil {
		return false, err
	}
	return true, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := range keys {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	return keys
}
