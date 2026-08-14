// Package app 负责 ServeMux 装配、中间件链与路由注册。
package app

import (
	"database/sql"
	"net/http"

	"github.com/arkpix/relay/internal/auth"
	"github.com/arkpix/relay/internal/common"
	"github.com/arkpix/relay/internal/config"
)

// New 构建 HTTP 处理器。中间件链：RequestID → AccessLog → mux。
// 路由按里程碑逐步挂载（M2 auth / M3 relay / M4 img / M5 sync / M6 recover）。
func New(cfg *config.Config, db *sql.DB) (http.Handler, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)

	authSvc := auth.NewService(db, cfg.InviteCodes)
	auth.RegisterRoutes(mux, authSvc)
	// 鉴权中间件在 M3+ 挂到受保护路由；此处先构造以建 STATIC_TOKENS 内置共享账号。
	if _, err := auth.NewMiddleware(db, cfg.StaticTokens); err != nil {
		return nil, err
	}

	return common.RequestID(common.AccessLog(mux)), nil
}

func healthz(w http.ResponseWriter, r *http.Request) {
	common.WriteJSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
}
