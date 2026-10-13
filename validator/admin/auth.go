package admin

import v "github.com/leivorn/base-kit/validator"

func init() {
	v.RegisterMessage("LoginRequest.Username.required", "用户名不能为空")
	v.RegisterMessage("LoginRequest.Password.required", "密码不能为空")
	v.RegisterMessage("CaptchaVerifyRequest.CaptchaID.required", "验证码已失效，请刷新")
	v.RegisterMessage("CaptchaVerifyRequest.X.gte", "拼图位置不合法")
	v.RegisterMessage("ChangePasswordRequest.OldPassword.required", "旧密码不能为空")
	v.RegisterMessage("ChangePasswordRequest.NewPassword.required", "新密码不能为空")
	v.RegisterMessage("ChangePasswordRequest.NewPassword.min", "新密码长度不能少于8位")
	v.RegisterMessage("ChangePasswordRequest.NewPassword.max", "新密码长度不能超过64位")
}
