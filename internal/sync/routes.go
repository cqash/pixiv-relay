package sync

import (
	"net/http"
	"strconv"

	"github.com/cqash/pixiv-relay/internal/auth"
	"github.com/cqash/pixiv-relay/internal/common"
)

// RegisterRoutes 挂载同步端点（§7.2）。
// push 链：auth 鉴权 → 写端点限流（默认 60/min，burst 10，key = accountID，§9）→ handler；
// pull 为只读端点，仅挂鉴权。
// 可注入共享 Limiter（app 层统一持有，供管理端 §14.2 热调速率）；缺省内部自建。
func RegisterRoutes(mux *http.ServeMux, svc *Service, mw *auth.Middleware, limiters ...*common.Limiter) {
	limiter := sharedOrNew(limiters, 60, 10)
	mux.Handle("POST /sync/v1/push",
		mw.Wrap(limiter.Middleware(accountKey)(http.HandlerFunc(pushHandler(svc)))))
	mux.Handle("GET /sync/v1/pull",
		mw.Wrap(http.HandlerFunc(pullHandler(svc))))
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

func pushHandler(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req PushRequest
		if err := common.DecodeJSON(w, r, &req); err != nil {
			common.WriteError(w, r, err)
			return
		}
		if err := common.Required("domain", req.Domain); err != nil {
			common.WriteError(w, r, err)
			return
		}
		accountID, _ := auth.AccountIDFrom(r.Context())
		resp, err := svc.Push(r.Context(), accountID, &req)
		if err != nil {
			common.WriteError(w, r, err)
			return
		}
		common.WriteJSON(w, r, http.StatusOK, resp)
	}
}

func pullHandler(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := common.Required("domain", r.URL.Query().Get("domain")); err != nil {
			common.WriteError(w, r, err)
			return
		}
		accountID, _ := auth.AccountIDFrom(r.Context())
		resp, err := svc.Pull(r.Context(), accountID,
			r.URL.Query().Get("domain"),
			r.URL.Query().Get("since"),
			common.ParseLimit(r))
		if err != nil {
			common.WriteError(w, r, err)
			return
		}
		common.WriteJSON(w, r, http.StatusOK, resp)
	}
}
