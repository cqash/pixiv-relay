package relay

import (
	"net/http"
	"strconv"

	"github.com/arkpix/relay/internal/auth"
	"github.com/arkpix/relay/internal/common"
)

// RegisterRoutes 挂载 POST /relay/v1/request（§6.1）。
// 链：auth 鉴权 → 写端点限流（默认 60/min，burst 10，key = accountID，§9）→ handler。
// 可注入共享 Limiter（app 层统一持有，供管理端 §14.2 热调速率）；缺省内部自建。
func RegisterRoutes(mux *http.ServeMux, svc *Service, mw *auth.Middleware, limiters ...*common.Limiter) {
	limiter := sharedOrNew(limiters, 60, 10)
	mux.Handle("POST /relay/v1/request",
		mw.Wrap(limiter.Middleware(accountKey)(http.HandlerFunc(makeHandler(svc)))))
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

func makeHandler(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req Request
		if err := common.DecodeJSON(w, r, &req); err != nil {
			common.WriteError(w, r, err)
			return
		}
		if err := common.Required("method", req.Method); err != nil {
			common.WriteError(w, r, err)
			return
		}
		if err := common.Required("url", req.URL); err != nil {
			common.WriteError(w, r, err)
			return
		}
		resp, err := svc.Do(r.Context(), &req)
		if err != nil {
			common.WriteError(w, r, err)
			return
		}
		common.WriteJSON(w, r, http.StatusOK, resp)
	}
}
