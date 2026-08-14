package web

import (
	"net/http"
	"strings"
)

// apiPathPrefixes 需要 CORS 处理的 API 路径前缀（§6.5：跨域部署时放开 API 访问；
// 静态资源同源托管，无需 CORS 头）。
var apiPathPrefixes = []string{"/auth/", "/relay/", "/img/", "/sync/", "/recover/", "/healthz"}

// CORS 跨域中间件（§6.5）：origins 为白名单（逗号分隔配置解析结果）。
// 空白名单 = 完全不开跨域（同源部署不需要），原样透传。
// 白名单内 Origin 才回显 Access-Control-Allow-Origin；API 路径的 preflight
// （OPTIONS + Access-Control-Request-Method）直接应答 204，不再进入下游。
func CORS(origins []string, next http.Handler) http.Handler {
	if len(origins) == 0 {
		return next
	}
	allowed := make(map[string]bool, len(origins))
	for _, o := range origins {
		allowed[o] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" || !allowed[origin] || !isAPIPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", origin)
		h.Add("Vary", "Origin")
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			h.Set("Access-Control-Max-Age", "86400")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isAPIPath 判定路径是否属于 API（CORS 仅作用于这些路径）。
func isAPIPath(p string) bool {
	for _, prefix := range apiPathPrefixes {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}
