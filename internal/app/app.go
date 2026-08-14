// Package app 负责 ServeMux 装配、中间件链与路由注册。
package app

import (
	"net/http"

	"github.com/arkpix/relay/internal/common"
)

// New 构建 HTTP 处理器。中间件链：RequestID → AccessLog → mux。
// 路由按里程碑逐步挂载（M2 auth / M3 relay / M4 img / M5 sync / M6 recover）。
func New() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	return common.RequestID(common.AccessLog(mux))
}

func healthz(w http.ResponseWriter, r *http.Request) {
	common.WriteJSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
}
