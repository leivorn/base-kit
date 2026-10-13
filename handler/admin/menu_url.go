package admin

import (
	"log"
	"net/url"
	"strings"

	adminmodel "github.com/leivorn/base-kit/model/admin"

	"github.com/gofiber/fiber/v2"
)

// 菜单的外链（link）与内嵌页（iframeSrc）地址会被前端原样交给 window.open / <iframe src>。
// 不限制协议的话，有菜单编辑权限的人填一个 javascript: 地址，超管点开这个菜单时脚本就在后台域名下执行，
// 能读走 localStorage 里的 token——等于借菜单提权到超管。只放行 http(s) 链接和以 / 开头的站内路径。

const msgUnsafeMenuURL = "外链和内嵌地址只能是 http(s) 链接或以 / 开头的站内路径"

// safeMenuURL 报告地址能否交给前端打开。空串表示未设置，视为安全。
func safeMenuURL(raw string) bool {
	s := strings.TrimSpace(raw)
	if s == "" {
		return true
	}
	// 浏览器解析前会剔除制表符和换行，"java\tscript:" 照样执行：带控制字符的一律拒绝
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	// 站内路径；"//host" 这类协议相对地址沿用当前页面的 http(s)，同样安全
	if strings.HasPrefix(s, "/") {
		return true
	}
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	return (scheme == "http" || scheme == "https") && u.Host != ""
}

// runtimeMenuMeta 是下发给前端渲染路由用的 meta：库里已有的不安全地址（这道校验上线前存进去的、
// 或下游种子直接写库的）不下发。菜单管理列表仍原样展示，管理员才看得到、改得掉。
func runtimeMenuMeta(m adminmodel.Menu) fiber.Map {
	meta := menuMeta(m)
	for _, key := range []string{"iframeSrc", "link"} {
		if v, ok := meta[key].(string); ok && !safeMenuURL(v) {
			log.Printf("[menu] WARN: menu %q has an unsafe %s %q, not sent to the frontend; fix it in menu management", m.Name, key, v)
			delete(meta, key)
		}
	}
	return meta
}
