package admin

import v "github.com/leivorn/base-kit/validator"

func init() {
	v.RegisterMessage("CreateUserRequest.Username.required", "用户名不能为空")
	v.RegisterMessage("CreateUserRequest.Username.min", "用户名长度不能少于3个字符")
	v.RegisterMessage("CreateUserRequest.Username.max", "用户名长度不能超过32个字符")
	v.RegisterMessage("CreateUserRequest.Password.required", "密码不能为空")
	v.RegisterMessage("CreateUserRequest.Password.min", "密码长度不能少于8位")
	v.RegisterMessage("CreateUserRequest.Password.max", "密码长度不能超过64位")
	v.RegisterMessage("CreateUserRequest.RealName.required", "真实姓名不能为空")
	v.RegisterMessage("CreateUserRequest.Email.email", "邮箱格式不正确")
	v.RegisterMessage("CreateUserRequest.Status.oneof", "状态取值不合法")

	v.RegisterMessage("UpdateUserRequest.Password.min", "密码长度不能少于8位")
	v.RegisterMessage("UpdateUserRequest.Password.max", "密码长度不能超过64位")
	v.RegisterMessage("UpdateUserRequest.Email.email", "邮箱格式不正确")
	v.RegisterMessage("UpdateUserRequest.Status.oneof", "状态取值不合法")
}
