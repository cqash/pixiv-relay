package recover

import (
	"net/http"
	"strconv"

	"github.com/arkpix/relay/internal/auth"
)

// RegisterRoutes 挂载 GET /recover/v1/illust/{pid}（§8.1）。
// 链：auth 鉴权 → 写端点限流 60/min（burst 10，key = accountID，§9）→ handler。
func RegisterRoutes(mux *http.ServeMux, svc *Service, mw *auth.Middleware) {
	mux.Handle("GET /recover/v1/illust/{pid}",
		mw.Wrap(svc.limiter.Middleware(accountKey)(http.HandlerFunc(svc.Query))))
}

// accountKey 限流键 = accountID（限流中间件在鉴权之后执行，必有值）。
func accountKey(r *http.Request) string {
	id, _ := auth.AccountIDFrom(r.Context())
	return strconv.FormatInt(id, 10)
}
