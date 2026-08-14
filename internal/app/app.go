// Package app 负责 ServeMux 装配、中间件链与路由注册。
package app

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"

	"github.com/arkpix/relay/internal/auth"
	"github.com/arkpix/relay/internal/cache"
	"github.com/arkpix/relay/internal/common"
	"github.com/arkpix/relay/internal/config"
	"github.com/arkpix/relay/internal/crypto"
	"github.com/arkpix/relay/internal/img"
	"github.com/arkpix/relay/internal/recover"
	"github.com/arkpix/relay/internal/relay"
	syncsvc "github.com/arkpix/relay/internal/sync"
	"github.com/arkpix/relay/internal/web"
)

// New 构建 HTTP 处理器（出网客户端按 cfg.UpstreamProxy 创建）。语义同 NewWithClient。
func New(cfg *config.Config, db *sql.DB) (http.Handler, error) {
	return NewWithClient(cfg, db, nil)
}

// NewWithClient 构建 HTTP 处理器。中间件链：RequestID → AccessLog → CORS（按白名单，默认关）→ mux。
// mux 兜底挂载 SPA 静态托管（§6.5，GET / 与前端路由 fallback）。
// DATA_ENC_KEY 格式错误在此直接报错（调用方启动失败退出，§9）。
// upstream 为 relay/img/recover 三模块统一出网客户端；nil 时按 cfg.UpstreamProxy
// 创建生产客户端（relay.NewUpstreamClient）。集成测试注入自定义 Transport 把
// 白名单域名改写到 httptest mock，保证端到端测试零真实外网。
func NewWithClient(cfg *config.Config, db *sql.DB, upstream *http.Client) (http.Handler, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)

	enc, err := crypto.Load(cfg.DataEncKey)
	if err != nil {
		return nil, fmt.Errorf("load data encryption key: %w", err)
	}

	authSvc := auth.NewService(db, cfg.InviteCodes)
	auth.RegisterRoutes(mux, authSvc, cfg.RateRegisterPerHour)
	authMw, err := auth.NewMiddleware(db, cfg.StaticTokens)
	if err != nil {
		return nil, err
	}

	if upstream == nil {
		upstream, err = relay.NewUpstreamClient(cfg.UpstreamProxy)
		if err != nil {
			return nil, err
		}
	}
	relay.RegisterRoutes(mux, relay.NewService(upstream, cfg.RelayExtraHosts), authMw, cfg.RateWritePerMin)

	imgCache, err := cache.Open(db, cache.Config{
		Dir:           cfg.CacheDir,
		TmpDir:        cfg.CacheTmpDir,
		MaxBytes:      cfg.CacheMaxBytes,
		Layout:        cfg.CacheLayout,
		HighWatermark: cfg.CacheHighWatermark,
		EvictionBatch: cfg.CacheEvictionBatch,
	})
	if err != nil {
		return nil, err
	}
	img.RegisterRoutes(mux, img.NewService(upstream, imgCache, cfg.ImgExtraHosts), authMw, cfg.RateImgPerMin)

	syncSvc, err := syncsvc.NewService(context.Background(), db, enc)
	if err != nil {
		return nil, err
	}
	syncsvc.RegisterRoutes(mux, syncSvc, authMw, cfg.RateWritePerMin)

	recoverSvc, err := recover.NewService(db, imgCache, upstream, recover.Config{
		Sources:         cfg.RecoverSources,
		TTLDays:         cfg.RecoverTTLDays,
		NegativeTTLDays: cfg.RecoverNegativeTTLDays,
		Shared:          cfg.RecoverShared,
		TmpDir:          cfg.RecoverTmpDir,
		ImgExtraHosts:   cfg.ImgExtraHosts,
		RatePerMin:      cfg.RateWritePerMin,
		Enc:             enc,
	})
	if err != nil {
		return nil, err
	}
	recover.RegisterRoutes(mux, recoverSvc, authMw)

	// Web 前端静态托管（§6.5）：兜底匹配所有未被 API 命中的 GET 路径，
	// 前端路由 fallback 到 index.html。
	spa, err := web.SPAHandler(cfg.WebDir)
	if err != nil {
		return nil, fmt.Errorf("web static dir: %w", err)
	}
	mux.Handle("GET /", spa)

	return common.RequestID(common.AccessLog(web.CORS(cfg.CORSOrigins, mux))), nil
}

func healthz(w http.ResponseWriter, r *http.Request) {
	common.WriteJSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
}
