package admin

import v "github.com/leivorn/base-kit/validator"

func init() {
	v.RegisterMessage("MenuRequest.Name.required", "菜单名称不能为空")
	v.RegisterMessage("MenuRequest.Type.required", "菜单类型不能为空")
	v.RegisterMessage("MenuRequest.Type.oneof", "菜单类型取值必须是: catalog menu button embedded link")
	v.RegisterMessage("MenuRequest.Title.required", "菜单标题不能为空")
	v.RegisterMessage("MenuRequest.Status.oneof", "状态取值不合法")
}
