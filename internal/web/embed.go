// Package web 实现 Web 前端静态托管与跨域支持（设计文档 §6.5）。
// SPA 静态资源经 go:embed 内嵌进二进制（dist/ 由独立前端工程产出，构建前拷入）；
// WEB_DIR 非空时改从磁盘目录服务（前端开发期免重新编译）。
// 同源托管为推荐形态，CORS_ORIGINS 白名单仅在跨域部署时配置，默认完全关闭。
package web

import (
	"embed"
	"io/fs"
)

//go:embed dist
var distFS embed.FS

// embeddedDist 返回内嵌的 SPA 静态资源根（dist 目录内容）。
func embeddedDist() (fs.FS, error) {
	return fs.Sub(distFS, "dist")
}
