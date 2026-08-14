package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func serve(t *testing.T, h http.Handler, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestSPA_EmbedIndex(t *testing.T) {
	h, err := SPAHandler("")
	if err != nil {
		t.Fatalf("spa handler: %v", err)
	}
	rec := serve(t, h, http.MethodGet, "/", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / want 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("GET / Content-Type want text/html, got %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("index.html must be no-cache, got %q", cc)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing X-Content-Type-Options: nosniff")
	}
	if !strings.Contains(rec.Body.String(), "ArkPix") {
		t.Fatalf("GET / want placeholder index.html, got %q", rec.Body.String())
	}
}

func TestSPA_FrontendRouteFallback(t *testing.T) {
	h, err := SPAHandler("")
	if err != nil {
		t.Fatalf("spa handler: %v", err)
	}
	for _, p := range []string{"/settings", "/detail/123", "/index.html"} {
		rec := serve(t, h, http.MethodGet, p, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s want 200, got %d", p, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "ArkPix") {
			t.Fatalf("GET %s want index.html fallback", p)
		}
	}
}

func TestSPA_WebDirDiskMode(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>disk</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.js"), []byte("console.log(1)"), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := SPAHandler(dir)
	if err != nil {
		t.Fatalf("spa handler: %v", err)
	}

	rec := serve(t, h, http.MethodGet, "/app.js", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /app.js want 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/javascript; charset=utf-8" {
		t.Fatalf("GET /app.js Content-Type got %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != staticCacheControl {
		t.Fatalf("static asset Cache-Control got %q", cc)
	}
	if rec.Body.String() != "console.log(1)" {
		t.Fatalf("GET /app.js body got %q", rec.Body.String())
	}

	// 磁盘模式生效：index 来自磁盘而非 embed 占位页。
	rec = serve(t, h, http.MethodGet, "/", nil)
	if !strings.Contains(rec.Body.String(), "disk") {
		t.Fatalf("WEB_DIR mode must serve disk index.html, got %q", rec.Body.String())
	}
}

func TestSPA_WebDirInvalid(t *testing.T) {
	if _, err := SPAHandler(filepath.Join(t.TempDir(), "nonexistent")); err == nil {
		t.Fatal("nonexistent WEB_DIR must error at startup")
	}
}

func TestCORS_DisabledByDefault(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := CORS(nil, next)
	rec := serve(t, h, http.MethodOptions, "/sync/v1/pull", map[string]string{
		"Origin":                        "https://app.example.com",
		"Access-Control-Request-Method": "GET",
	})
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("empty whitelist must not emit CORS headers")
	}
}

func TestCORS_WhitelistedOrigin(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := CORS([]string{"https://app.example.com"}, next)

	// preflight：204 + 回显 Origin + 允许 Authorization/Content-Type。
	rec := serve(t, h, http.MethodOptions, "/sync/v1/pull", map[string]string{
		"Origin":                        "https://app.example.com",
		"Access-Control-Request-Method": "GET",
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight want 204, got %d", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Fatalf("preflight must echo whitelisted origin, got %q", got)
	}
	if ah := rec.Header().Get("Access-Control-Allow-Headers"); !strings.Contains(ah, "Authorization") || !strings.Contains(ah, "Content-Type") {
		t.Fatalf("Allow-Headers got %q", ah)
	}

	// 普通跨域请求：回显 Origin 并放行到下游。
	rec = serve(t, h, http.MethodGet, "/img/v1/fetch", map[string]string{"Origin": "https://app.example.com"})
	if rec.Code != http.StatusOK || rec.Header().Get("Access-Control-Allow-Origin") == "" {
		t.Fatalf("simple request want 200 + echoed origin, got %d %q", rec.Code, rec.Header().Get("Access-Control-Allow-Origin"))
	}
}

func TestCORS_NonWhitelistedOrigin(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := CORS([]string{"https://app.example.com"}, next)
	rec := serve(t, h, http.MethodOptions, "/sync/v1/pull", map[string]string{
		"Origin":                        "https://evil.example.com",
		"Access-Control-Request-Method": "GET",
	})
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("non-whitelisted origin must not be echoed")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("non-whitelisted preflight falls through to next handler, got %d", rec.Code)
	}
}

func TestCORS_NotAppliedToStaticPaths(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := CORS([]string{"https://app.example.com"}, next)
	rec := serve(t, h, http.MethodOptions, "/settings", map[string]string{
		"Origin":                        "https://app.example.com",
		"Access-Control-Request-Method": "GET",
	})
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("CORS must only apply to API paths")
	}
}
