package common

import (
	"log/slog"
	"net/http"
	"time"
)

// statusWriter 捕获响应状态码；Unwrap 供 http.ResponseController 取回底层
// writer（M4 图片流式透传需要 Flusher 能力）。
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// AccessLog 访问日志中间件：只记 method + 路径（不含 query）+ 状态码 + 耗时 + requestId。
// 绝不记录 query / body / 请求头（§6.1、§9）。
func AccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		slog.InfoContext(r.Context(), "request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"durMs", time.Since(start).Milliseconds(),
			"requestId", RequestIDFrom(r.Context()),
		)
	})
}
