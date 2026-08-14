package relay

import (
	"net/http"
	"strconv"

	"github.com/arkpix/relay/internal/auth"
	"github.com/arkpix/relay/internal/common"
)

// RegisterRoutes 挂载 POST /relay/v1/request（§6.1）。
// 链：auth 鉴权 → 写端点限流（默认 60/min，burst 10，key = accountID，§9；writePerMin 可覆盖）→ handler。
func RegisterRoutes(mux *http.ServeMux, svc *Service, mw *auth.Middleware, writePerMin ...float64) {
	perMin := 60.0
	if len(writePerMin) > 0 && writePerMin[0] > 0 {
		perMin = writePerMin[0]
	}
	limiter := common.NewLimiter(perMin, 10)
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
