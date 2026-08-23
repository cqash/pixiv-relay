package img

import (
	"net/http"
	"strconv"

	"github.com/cqash/pixiv-relay/internal/auth"
	"github.com/cqash/pixiv-relay/internal/common"
)

// RegisterRoutes 挂载 GET /img/v1/fetch（§6.2）。
// 链：auth 鉴权 → 图片端点限流（默认 300/min，burst 30，key = accountID，§9）→ handler。
// 可注入共享 Limiter（app 层统一持有，供管理端 §14.2 热调速率）；缺省内部自建。
func RegisterRoutes(mux *http.ServeMux, svc *Service, mw *auth.Middleware, limiters ...*common.Limiter) {
	limiter := sharedOrNew(limiters, 300, 30)
	mux.Handle("GET /img/v1/fetch",
		mw.Wrap(limiter.Middleware(accountKey)(http.HandlerFunc(svc.Fetch))))
}

// sharedOrNew 取注入的共享限流器；未注入时按配额自建（保持既有测试调用兼容）。
func sharedOrNew(limiters []*common.Limiter, perMin float64, burst int) *common.Limiter {
	if len(limiters) > 0 && limiters[0] != nil {
		return limiters[0]
	}
	return common.NewLimiter(perMin, burst)
}

// accountKey 限流键 = accountID（限流中间件在鉴权之后执行，必有值）。
func accountKey(r *http.Request) string {
	id, _ := auth.AccountIDFrom(r.Context())
	return strconv.FormatInt(id, 10)
}
