package img

import (
	"net/http"
	"strconv"

	"github.com/arkpix/relay/internal/auth"
	"github.com/arkpix/relay/internal/common"
)

// RegisterRoutes 挂载 GET /img/v1/fetch（§6.2）。
// 链：auth 鉴权 → 图片端点限流 300/min（burst 30，key = accountID，§9）→ handler。
func RegisterRoutes(mux *http.ServeMux, svc *Service, mw *auth.Middleware) {
	limiter := common.NewLimiter(300, 30)
	mux.Handle("GET /img/v1/fetch",
		mw.Wrap(limiter.Middleware(accountKey)(http.HandlerFunc(svc.Fetch))))
}

// accountKey 限流键 = accountID（限流中间件在鉴权之后执行，必有值）。
func accountKey(r *http.Request) string {
	id, _ := auth.AccountIDFrom(r.Context())
	return strconv.FormatInt(id, 10)
}
