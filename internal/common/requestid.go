package common

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"
)

type ctxKey string

const requestIDKey ctxKey = "requestId"

// RequestIDFrom 从上下文取 requestId（无则返回空串）。
func RequestIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}

func newRequestID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%d_%s", time.Now().UnixMilli(), hex.EncodeToString(b[:]))
}

// RequestID 中间件：生成 requestId（或沿用客户端传入的 X-Request-Id），
// 写入响应头 X-Request-Id 与请求上下文（§4.1 所有响应含 requestId）。
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}
