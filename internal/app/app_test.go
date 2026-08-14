package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arkpix/relay/internal/config"
	"github.com/arkpix/relay/internal/db"
)

// setup 构建完整应用：临时库 + 临时目录（t.TempDir，绝不碰 ./data）。
func setup(t *testing.T, cfg *config.Config) http.Handler {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := db.Migrate(context.Background(), database); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if cfg.CacheDir == "" {
		cfg.CacheDir = filepath.Join(t.TempDir(), "cache")
	}
	if cfg.RecoverTmpDir == "" {
		cfg.RecoverTmpDir = filepath.Join(t.TempDir(), "recover-tmp")
	}
	handler, err := New(cfg, database)
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	return handler
}

func get(t *testing.T, h http.Handler, target string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHealthz(t *testing.T) {
	handler := setup(t, &config.Config{})

	rec := get(t, handler, "/healthz", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("want status ok, got %q", body["status"])
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("API response must be no-store, got %q", cc)
	}
}

// TestSPAHosting Web 托管（§6.5）：GET / 与前端路由 fallback 到 index.html，
// /healthz 不被 SPA 拦截。
func TestSPAHosting(t *testing.T) {
	handler := setup(t, &config.Config{})

	rec := get(t, handler, "/", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / want 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ArkPix") {
		t.Fatalf("GET / want embedded index.html, got %q", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("GET / Content-Type got %q", ct)
	}

	// 前端路由 fallback。
	rec = get(t, handler, "/settings", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ArkPix") {
		t.Fatalf("GET /settings want index.html fallback, got %d", rec.Code)
	}

	// /healthz 不被 SPA 拦截。
	rec = get(t, handler, "/healthz", nil)
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("/healthz must be handled by API, got Content-Type %q", ct)
	}
}

// TestSPAWebDir WEB_DIR 磁盘模式：改从磁盘目录服务。
func TestSPAWebDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>disk</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	handler := setup(t, &config.Config{WebDir: dir})
	rec := get(t, handler, "/", nil)
	if !strings.Contains(rec.Body.String(), "disk") {
		t.Fatalf("WEB_DIR mode must serve disk index.html, got %q", rec.Body.String())
	}
}

// TestBadDataEncKeyFailsStartup DATA_ENC_KEY 格式错误 → app.New 直接报错（启动退出）。
func TestBadDataEncKeyFailsStartup(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := db.Migrate(context.Background(), database); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	cfg := &config.Config{
		CacheDir:      filepath.Join(t.TempDir(), "cache"),
		RecoverTmpDir: filepath.Join(t.TempDir(), "recover-tmp"),
		DataEncKey:    "not-valid-base64!!!",
	}
	if _, err := New(cfg, database); err == nil {
		t.Fatal("invalid DATA_ENC_KEY must fail app startup")
	}
	cfg.DataEncKey = "dG9vLXNob3J0" // base64 合法但非 32 字节
	if _, err := New(cfg, database); err == nil {
		t.Fatal("non-32-byte DATA_ENC_KEY must fail app startup")
	}
}

// TestCORSEnabled CORS_ORIGINS 白名单：preflight 回显白名单 Origin；
// 空配置完全无 CORS 头。
func TestCORSEnabled(t *testing.T) {
	handler := setup(t, &config.Config{CORSOrigins: []string{"https://app.example.com"}})

	req := httptest.NewRequest(http.MethodOptions, "/sync/v1/pull", nil)
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Access-Control-Request-Method", "GET")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight want 204, got %d", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Fatalf("preflight must echo origin, got %q", got)
	}

	// 非白名单不回显。
	req = httptest.NewRequest(http.MethodOptions, "/sync/v1/pull", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	req.Header.Set("Access-Control-Request-Method", "GET")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("non-whitelisted origin must not be echoed")
	}
}

func TestCORSDisabledByDefault(t *testing.T) {
	handler := setup(t, &config.Config{})
	req := httptest.NewRequest(http.MethodOptions, "/sync/v1/pull", nil)
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Access-Control-Request-Method", "GET")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("empty CORS_ORIGINS must not emit CORS headers")
	}
}
