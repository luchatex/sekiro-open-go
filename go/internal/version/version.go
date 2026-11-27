// Package version 保存构建时注入的服务端版本号。
//
// 发布流水线通过 -X sekiro-go/internal/version.Version=<tag 去掉 v> 注入；
// 本地 go build / make build 未注入时显示 dev，避免把「开发构建」误认为线上版本。
package version

// Version 当前构建版本，默认 dev。
var Version = "dev"

// String 返回展示用版本串。
func String() string {
	if Version == "" {
		return "dev"
	}
	return Version
}
