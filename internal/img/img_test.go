package img

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arkpix/relay/internal/auth"
	"github.com/arkpix/relay/internal/cache"
	"github.com/arkpix/relay/internal/common"
	"github.com/arkpix/relay/internal/db"
)

const staticToken = "test-static-token"

// rewriteTransport 测试用：把白名单域名的出网请求改写到 httptest 上游（host 覆盖注入）。
type rewriteTransport struct{ target *url.URL }

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r2 := req.Clone(req.Context())
	r2.URL.Scheme = t.target.Scheme
	r2.URL.Host = t.target.Host
	return http.DefaultTransport.RoundTrip(r2)
}

// setup 构建测试 handler：临时库 + 临时缓存目录（t.TempDir，绝不碰 ./data）+
// STATIC_TOKENS 直通鉴权 + img 路由（上游改写为 upstream）。返回 handler 与上游命中计数。
func setup(t *testing.T, cacheCfg cache.Config, upstream http.Handler) (http.Handler, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		upstream.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse upstream url: %v", err)
	}

	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := db.Migrate(context.Background(), database); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	mw, err := auth.NewMiddleware(database, []string{staticToken})
	if err != nil {
		t.Fatalf("new middleware: %v", err)
	}
	cacheCfg.Dir = filepath.Join(t.TempDir(), "cache")
	c, err := cache.Open(database, cacheCfg)
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}

	svc := NewService(&http.Client{Transport: &rewriteTransport{target: u}}, c, nil)
	mux := http.NewServeMux()
	RegisterRoutes(mux, svc, mw)
	return common.RequestID(mux), &hits
}

const testURL = "https://i.pximg.net/img-original/img/2024/01/01/00/00/00/123_p0.jpg"

func doFetch(t *testing.T, h http.Handler, rawURL string, authed bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/img/v1/fetch?url="+url.QueryEscape(rawURL), nil)
	if authed {
		req.Header.Set("Authorization", "Bearer "+staticToken)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestMissThenHit(t *testing.T) {
	body := bytes.Repeat([]byte("img-bytes"), 1000)
	h, hits := setup(t, cache.Config{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Referer") != "https://app-api.pixiv.net/" {
			t.Errorf("missing referer injection, got %q", r.Header.Get("Referer"))
		}
		if r.Header.Get("User-Agent") != "PixivIOSApp/5.8.0" {
			t.Errorf("missing UA injection, got %q", r.Header.Get("User-Agent"))
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "max-age=31536000")
		_, _ = w.Write(body)
	}))

	// 未命中 → MISS 透传且落盘。
	rec := doFetch(t, h, testURL, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("miss status = %d, body %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Cache") != "MISS" {
		t.Fatalf("X-Cache = %q, want MISS", rec.Header().Get("X-Cache"))
	}
	if rec.Header().Get("X-Upstream-Status") != "200" {
		t.Fatalf("X-Upstream-Status = %q", rec.Header().Get("X-Upstream-Status"))
	}
	if !bytes.Equal(rec.Body.Bytes(), body) {
		t.Fatal("miss body mismatch")
	}
	if rec.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("Content-Type = %q", rec.Header().Get("Content-Type"))
	}
	if rec.Header().Get("Cache-Control") != "max-age=31536000" {
		t.Fatalf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}
	if rec.Header().Get("Content-Disposition") != "" {
		t.Fatal("Content-Disposition must be unset by default")
	}

	// 二次请求 → HIT，不再触达上游，字节一致。
	rec2 := doFetch(t, h, testURL, true)
	if rec2.Code != http.StatusOK || rec2.Header().Get("X-Cache") != "HIT" {
		t.Fatalf("hit: status = %d X-Cache = %q", rec2.Code, rec2.Header().Get("X-Cache"))
	}
	if !bytes.Equal(rec2.Body.Bytes(), body) {
		t.Fatal("hit body mismatch with upstream bytes")
	}
	if hits.Load() != 1 {
		t.Fatalf("upstream hits = %d, want 1", hits.Load())
	}
	if rec2.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("hit Content-Type = %q", rec2.Header().Get("Content-Type"))
	}
}

func TestDispositionInline(t *testing.T) {
	h, _ := setup(t, cache.Config{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("png"))
	}))
	req := httptest.NewRequest(http.MethodGet,
		"/img/v1/fetch?url="+url.QueryEscape(testURL)+"&disposition=inline", nil)
	req.Header.Set("Authorization", "Bearer "+staticToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Disposition") != "inline" {
		t.Fatalf("Content-Disposition = %q", rec.Header().Get("Content-Disposition"))
	}
}

func TestUpstream404(t *testing.T) {
	h, hits := setup(t, cache.Config{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	rec := doFetch(t, h, testURL, true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "NOT_FOUND") {
		t.Fatalf("body = %s, want NOT_FOUND error", rec.Body.String())
	}
	if rec.Header().Get("X-Upstream-Status") != "404" {
		t.Fatalf("X-Upstream-Status = %q", rec.Header().Get("X-Upstream-Status"))
	}
	// 不入缓存：再次请求仍触达上游。
	doFetch(t, h, testURL, true)
	if hits.Load() != 2 {
		t.Fatalf("upstream hits = %d, want 2 (404 not cached)", hits.Load())
	}
}

func TestUpstream500(t *testing.T) {
	h, hits := setup(t, cache.Config{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	rec := doFetch(t, h, testURL, true)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "UPSTREAM_UNREACHABLE") {
		t.Fatalf("body = %s, want UPSTREAM_UNREACHABLE", rec.Body.String())
	}
	doFetch(t, h, testURL, true)
	if hits.Load() != 2 {
		t.Fatalf("upstream hits = %d, want 2 (500 not cached)", hits.Load())
	}
}

func TestOversizePassthrough(t *testing.T) {
	// 单图上限注入 64B：上游 128B → 直连透传且不入缓存。
	body := strings.Repeat("x", 128)
	h, hits := setup(t, cache.Config{MaxFileBytes: 64}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = io.WriteString(w, body)
	}))
	rec := doFetch(t, h, testURL, true)
	if rec.Code != http.StatusOK || rec.Body.String() != body {
		t.Fatalf("status = %d, body len = %d", rec.Code, rec.Body.Len())
	}
	rec2 := doFetch(t, h, testURL, true)
	if rec2.Header().Get("X-Cache") != "MISS" {
		t.Fatalf("oversized must not be cached, X-Cache = %q", rec2.Header().Get("X-Cache"))
	}
	if hits.Load() != 2 {
		t.Fatalf("upstream hits = %d, want 2", hits.Load())
	}
}

func TestOversizeUnknownLength(t *testing.T) {
	// 未知 Content-Length（chunked）超上限：Writer 超限降级，透传完整不入缓存。
	body := strings.Repeat("y", 128)
	h, hits := setup(t, cache.Config{MaxFileBytes: 64}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		for _, chunk := range strings.Split(body, "") {
			_, _ = io.WriteString(w, chunk)
			fl.Flush() // 强制 chunked，未知总长
		}
	}))
	rec := doFetch(t, h, testURL, true)
	if rec.Code != http.StatusOK || rec.Body.String() != body {
		t.Fatalf("status = %d, body len = %d", rec.Code, rec.Body.Len())
	}
	doFetch(t, h, testURL, true)
	if hits.Load() != 2 {
		t.Fatalf("upstream hits = %d, want 2 (chunked oversize not cached)", hits.Load())
	}
}

func TestHostNotAllowed(t *testing.T) {
	h, _ := setup(t, cache.Config{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rec := doFetch(t, h, "https://evil.example.com/x.jpg", true)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "FORBIDDEN") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestSchemeNotHTTPS(t *testing.T) {
	h, _ := setup(t, cache.Config{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rec := doFetch(t, h, "http://i.pximg.net/x.jpg", true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestMissingURL(t *testing.T) {
	h, _ := setup(t, cache.Config{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/img/v1/fetch", nil)
	req.Header.Set("Authorization", "Bearer "+staticToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestUnauthorized(t *testing.T) {
	h, _ := setup(t, cache.Config{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rec := doFetch(t, h, testURL, false)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "INVALID_TOKEN") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestExtraHosts(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("extra"))
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)

	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := db.Migrate(context.Background(), database); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	mw, err := auth.NewMiddleware(database, []string{staticToken})
	if err != nil {
		t.Fatalf("new middleware: %v", err)
	}
	c, err := cache.Open(database, cache.Config{Dir: filepath.Join(t.TempDir(), "cache")})
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	svc := NewService(&http.Client{Transport: &rewriteTransport{target: u}}, c, []string{"i.pixiv.cat"})
	mux := http.NewServeMux()
	RegisterRoutes(mux, svc, mw)
	h := common.RequestID(mux)

	rec := doFetch(t, h, "https://i.pixiv.cat/123.jpg", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("extra host should be allowed, status = %d", rec.Code)
	}
}

func TestConcurrentSameKeySingleUpstream(t *testing.T) {
	body := bytes.Repeat([]byte("concurrent"), 500)
	h, hits := setup(t, cache.Config{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond) // 拉宽竞态窗口
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(body)
	}))

	const n = 8
	results := make([]*httptest.ResponseRecorder, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = doFetch(t, h, testURL, true)
		}(i)
	}
	wg.Wait()

	for i, rec := range results {
		if rec.Code != http.StatusOK {
			t.Fatalf("req %d status = %d, body %s", i, rec.Code, rec.Body.String())
		}
		if !bytes.Equal(rec.Body.Bytes(), body) {
			t.Fatalf("req %d body mismatch", i)
		}
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("upstream hits = %d, want 1 (singleflight)", got)
	}
}
