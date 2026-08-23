package admin

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/cqash/pixiv-relay/internal/common"
)

// RegisterRoutes 挂载 /admin/v1 路由（§14.3）。全部端点经管理端鉴权中间件
// （Bearer ADMIN_TOKEN 常量时间比较 + per-IP 限流 30/min，§14.1）。
func RegisterRoutes(mux *http.ServeMux, svc *Service, token string) {
	mw := newMiddleware(token)
	mux.Handle("GET /admin/v1/overview", mw.Wrap(http.HandlerFunc(overviewHandler(svc))))
	mux.Handle("GET /admin/v1/settings", mw.Wrap(http.HandlerFunc(getSettingsHandler(svc))))
	mux.Handle("PATCH /admin/v1/settings", mw.Wrap(http.HandlerFunc(patchSettingsHandler(svc))))
	mux.Handle("GET /admin/v1/cache/stats", mw.Wrap(http.HandlerFunc(cacheStatsHandler(svc))))
	mux.Handle("POST /admin/v1/cache/evict", mw.Wrap(http.HandlerFunc(cacheEvictHandler(svc))))
	mux.Handle("GET /admin/v1/accounts", mw.Wrap(http.HandlerFunc(listAccountsHandler(svc))))
	mux.Handle("GET /admin/v1/accounts/{id}/devices", mw.Wrap(http.HandlerFunc(listDevicesHandler(svc))))
	mux.Handle("DELETE /admin/v1/devices/{id}", mw.Wrap(http.HandlerFunc(deleteDeviceHandler(svc))))
	mux.Handle("DELETE /admin/v1/accounts/{id}", mw.Wrap(http.HandlerFunc(deleteAccountHandler(svc))))
}

// pathID 解析路径参数 {id} 为 int64，非法返回 400。
func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		common.WriteError(w, r, common.BadRequest("id must be an integer"))
		return 0, false
	}
	return id, true
}

func overviewHandler(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp, err := svc.Overview(r.Context())
		if err != nil {
			common.WriteError(w, r, err)
			return
		}
		common.WriteJSON(w, r, http.StatusOK, resp)
	}
}

func getSettingsHandler(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		common.WriteJSON(w, r, http.StatusOK, map[string]any{
			"settings": svc.resolveAll(r.Context()),
		})
	}
}

// patchSettingsHandler PATCH /admin/v1/settings（§14.2）：部分键值 JSON，
// 全部校验通过才落库并立即热生效；未知键/非法值 400。
func patchSettingsHandler(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		if err := common.DecodeJSON(w, r, &body); err != nil {
			common.WriteError(w, r, err)
			return
		}
		patch := make(map[string]string, len(body))
		for key, raw := range body {
			var v json.Number
			if err := json.Unmarshal(raw, &v); err != nil {
				common.WriteError(w, r, common.BadRequest("invalid value for "+key+": must be a number"))
				return
			}
			patch[key] = v.String()
		}
		normalized, err := validatePatch(patch)
		if err != nil {
			common.WriteError(w, r, err)
			return
		}
		if err := svc.persistSettings(r.Context(), normalized); err != nil {
			common.WriteError(w, r, err)
			return
		}
		svc.applyAll(r.Context())
		common.WriteJSON(w, r, http.StatusOK, map[string]any{
			"settings": svc.resolveAll(r.Context()),
		})
	}
}

func cacheStatsHandler(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp, err := svc.CacheStats(r.Context())
		if err != nil {
			common.WriteError(w, r, err)
			return
		}
		common.WriteJSON(w, r, http.StatusOK, resp)
	}
}

func cacheEvictHandler(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp, err := svc.EvictCache(r.Context())
		if err != nil {
			common.WriteError(w, r, err)
			return
		}
		common.WriteJSON(w, r, http.StatusOK, resp)
	}
}

func listAccountsHandler(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		items, next, err := svc.ListAccounts(r.Context(), r.URL.Query().Get("cursor"), common.ParseLimit(r))
		if err != nil {
			common.WriteError(w, r, err)
			return
		}
		common.WriteJSON(w, r, http.StatusOK, map[string]any{
			"items":      items,
			"nextCursor": next,
		})
	}
}

func listDevicesHandler(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		items, err := svc.ListDevices(r.Context(), id)
		if err != nil {
			common.WriteError(w, r, err)
			return
		}
		common.WriteJSON(w, r, http.StatusOK, map[string]any{"items": items})
	}
}

func deleteDeviceHandler(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		if err := svc.DeleteDevice(r.Context(), id); err != nil {
			common.WriteError(w, r, err)
			return
		}
		common.WriteJSON(w, r, http.StatusOK, map[string]any{"deleted": true})
	}
}

func deleteAccountHandler(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		if err := svc.DeleteAccount(r.Context(), id); err != nil {
			common.WriteError(w, r, err)
			return
		}
		common.WriteJSON(w, r, http.StatusOK, map[string]any{"deleted": true})
	}
}
