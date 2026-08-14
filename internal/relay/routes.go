package relay

import (
	"net/http"
	"strconv"

	"github.com/arkpix/relay/internal/auth"
	"github.com/arkpix/relay/internal/common"
)

// RegisterRoutes 挂载 POST /relay/v1/request（§6.1）。
// 链：auth 鉴权 → 写端点限流 60/min（burst 10，key = accountID，§9）→ handler。
func RegisterRoutes(mux *http.ServeMux, svc *Service, mw *auth.Middleware) {
	limiter := common.NewLimiter(60, 10)
	mux.Handle("POST /relay/v1/request",
		mw.Wrap(limiter.Middleware(accountKey)(http.HandlerFunc(makeHandler(svc)))))
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
