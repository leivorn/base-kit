package admin

import v "github.com/leivorn/base-kit/validator"

func init() {
	v.RegisterMessage("ConfigRequest.ConfigName.required", "配置名称不能为空")
	v.RegisterMessage("ConfigRequest.ConfigKey.required", "配置键不能为空")
	v.RegisterMessage("ConfigRequest.Status.oneof", "状态取值不合法")
}
