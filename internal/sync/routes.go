package sync

import (
	"net/http"
	"strconv"

	"github.com/arkpix/relay/internal/auth"
	"github.com/arkpix/relay/internal/common"
)

// RegisterRoutes 挂载同步端点（§7.2）。
// push 链：auth 鉴权 → 写端点限流（默认 60/min，burst 10，key = accountID，§9；writePerMin 可覆盖）→ handler；
// pull 为只读端点，仅挂鉴权。
func RegisterRoutes(mux *http.ServeMux, svc *Service, mw *auth.Middleware, writePerMin ...float64) {
	perMin := 60.0
	if len(writePerMin) > 0 && writePerMin[0] > 0 {
		perMin = writePerMin[0]
	}
	limiter := common.NewLimiter(perMin, 10)
	mux.Handle("POST /sync/v1/push",
		mw.Wrap(limiter.Middleware(accountKey)(http.HandlerFunc(pushHandler(svc)))))
	mux.Handle("GET /sync/v1/pull",
		mw.Wrap(http.HandlerFunc(pullHandler(svc))))
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
