package admin

import v "github.com/leivorn/base-kit/validator"

func init() {
	v.RegisterMessage("CreateRoleRequest.Name.required", "角色名称不能为空")
	v.RegisterMessage("CreateRoleRequest.Code.required", "角色编码不能为空")
	v.RegisterMessage("CreateRoleRequest.Status.oneof", "状态取值不合法")
}
