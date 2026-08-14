package common

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
)

// WriteJSON 写出成功响应并注入 requestId（§4.1）。payload 为对象时合并
// requestId 字段；非对象 payload 包装为 {"data": ...}。
func WriteJSON(w http.ResponseWriter, r *http.Request, status int, payload any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	body := map[string]any{}
	if err := json.Unmarshal(raw, &body); err != nil {
		body = map[string]any{"data": payload}
	}
	body["requestId"] = RequestIDFrom(r.Context())
	writeRaw(w, status, body)
}

// WriteError 写出统一错误格式（§4.2）。非 *APIError 一律降级 500 INTERNAL，
// 原始错误只进日志不外泄。
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	var ae *APIError
	if !errors.As(err, &ae) {
		slog.ErrorContext(r.Context(), "internal error", "err", err)
		ae = NewError(http.StatusInternalServerError, "INTERNAL", "internal error")
	}
	writeRaw(w, ae.Status, map[string]any{
		"error": map[string]any{
			"code":      ae.Code,
			"message":   ae.Message,
			"requestId": RequestIDFrom(r.Context()),
		},
	})
}

// writeRaw 统一 JSON 输出。nosniff + no-store：API 响应含用户数据，禁止中间缓存（§9）。
func writeRaw(w http.ResponseWriter, status int, body map[string]any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
