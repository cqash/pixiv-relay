package auth

import (
	"net/http"

	"github.com/arkpix/relay/internal/common"
)

// serverVersion 与 capabilities 供客户端按能力降级 UI（§12）。
const serverVersion = "1.0.0"

var capabilities = []string{"relay", "img", "sync", "recover"}

type registerRequest struct {
	DeviceName string `json:"deviceName"`
	InviteCode string `json:"inviteCode"`
	AccountKey string `json:"accountKey"`
}

type refreshRequest struct {
	RefreshToken string `json:"refreshToken"`
}

type tokenResponse struct {
	AccessToken  string   `json:"accessToken"`
	RefreshToken string   `json:"refreshToken"`
	ExpiresIn    int64    `json:"expiresIn"`
	AccountKey   string   `json:"accountKey,omitempty"`
	ServerVer    string   `json:"serverVersion"`
	Capabilities []string `json:"capabilities"`
}

// RegisterRoutes 挂载 /auth/v1 路由（§13）。
// register 端点独立限流 10 次/小时/IP（§9 防批量注册），refresh 走认证用户通用配额（M3+ 挂载）。
func RegisterRoutes(mux *http.ServeMux, svc *Service) {
	limiter := common.NewLimiter(10.0/60, 3)
	mux.Handle("POST /auth/v1/register",
		limiter.Middleware(common.ClientIP)(http.HandlerFunc(makeRegisterHandler(svc))))
	mux.HandleFunc("POST /auth/v1/refresh", makeRefreshHandler(svc))
}

func makeRegisterHandler(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req registerRequest
		if err := common.DecodeJSON(w, r, &req); err != nil {
			common.WriteError(w, r, err)
			return
		}
		if err := common.Required("deviceName", req.DeviceName); err != nil {
			common.WriteError(w, r, err)
			return
		}
		pair, err := svc.Register(r.Context(), req.DeviceName, req.InviteCode, req.AccountKey)
		if err != nil {
			common.WriteError(w, r, err)
			return
		}
		common.WriteJSON(w, r, http.StatusOK, tokenResponse{
			AccessToken:  pair.AccessToken,
			RefreshToken: pair.RefreshToken,
			ExpiresIn:    pair.ExpiresIn,
			AccountKey:   pair.AccountKey,
			ServerVer:    serverVersion,
			Capabilities: capabilities,
		})
	}
}

func makeRefreshHandler(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req refreshRequest
		if err := common.DecodeJSON(w, r, &req); err != nil {
			common.WriteError(w, r, err)
			return
		}
		if err := common.Required("refreshToken", req.RefreshToken); err != nil {
			common.WriteError(w, r, err)
			return
		}
		pair, err := svc.Refresh(r.Context(), req.RefreshToken)
		if err != nil {
			common.WriteError(w, r, err)
			return
		}
		common.WriteJSON(w, r, http.StatusOK, tokenResponse{
			AccessToken:  pair.AccessToken,
			RefreshToken: pair.RefreshToken,
			ExpiresIn:    pair.ExpiresIn,
			ServerVer:    serverVersion,
			Capabilities: capabilities,
		})
	}
}
