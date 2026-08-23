// Package admin 实现管理端 /admin/v1（设计文档 §14）：部署方运维管理面，
// 独立于客户端协议；Bearer ADMIN_TOKEN 鉴权（与账号体系解耦）+
// 运行时设置热更新（DB > env > 默认）+ 缓存/账号/设备管理。
package admin

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/arkpix/relay/internal/common"
)

// middleware 管理端鉴权（§14.1）：Bearer 与 ADMIN_TOKEN 常量时间比较，
// 失败 401 INVALID_TOKEN 走统一错误信封；前置 per-IP 限流防爆破（30 次/分钟）。
type middleware struct {
	token   string
	limiter *common.Limiter
}

func newMiddleware(token string) *middleware {
	return &middleware{token: token, limiter: common.NewLimiter(30.0/60, 5)}
}

// Wrap 链：per-IP 限流 → Bearer 常量时间比较 → handler。
func (m *middleware) Wrap(next http.Handler) http.Handler {
	return m.limiter.Middleware(common.ClientIP)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		scheme, token, ok := strings.Cut(h, " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") ||
			subtle.ConstantTimeCompare([]byte(token), []byte(m.token)) != 1 {
			common.WriteError(w, r, common.Unauthorized("invalid admin token"))
			return
		}
		next.ServeHTTP(w, r)
	}))
}
