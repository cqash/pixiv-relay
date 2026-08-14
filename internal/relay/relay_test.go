package relay

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arkpix/relay/internal/auth"
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

// setup 构建测试 handler：临时库（t.TempDir）+ STATIC_TOKENS 直通鉴权 +
// relay 路由（上游改写为 upstream）。外层包 RequestID 使错误响应带 requestId。
func setup(t *testing.T, upstream http.Handler, extraHosts []string) http.Handler {
	t.Helper()
	srv := httptest.NewServer(upstream)
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

	svc := NewService(&http.Client{Transport: &rewriteTransport{target: u}}, extraHosts)
	mux := http.NewServeMux()
	RegisterRoutes(mux, svc, mw)
	return common.RequestID(mux)
}

func doRelay(t *testing.T, h http.Handler, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/relay/v1/request", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v (raw: %s)", err, rec.Body.String())
	}
	return body
}

func bearer() map[string]string {
	return map[string]string{"Authorization": "Bearer " + staticToken}
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	errObj, ok := decodeBody(t, rec)["error"].(map[string]any)
	if !ok {
		t.Fatalf("missing error object: %s", rec.Body.String())
	}
	code, _ := errObj["code"].(string)
	return code
}

// 正常 GET 透传：状态码、白名单响应头、bodyBase64 往返一致；非白名单请求头不到达上游。
func TestGetPassthrough(t *testing.T) {
	var gotUA, gotAuth, gotLang, gotEvil string
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotAuth = r.Header.Get("Authorization")
		gotLang = r.Header.Get("Accept-Language")
		gotEvil = r.Header.Get("X-Evil")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Evil", "should-not-pass")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	h := setup(t, upstream, nil)

	rec := doRelay(t, h, map[string]any{
		"method": "GET",
		"url":    "https://app-api.pixiv.net/v1/illust/recommended?filter=for_android",
		"headers": map[string]string{
			"Authorization":   "Bearer pixiv-token-secret",
			"Accept-Language": "zh-CN",
			"User-Agent":      "CustomUA/1.0",
			"X-Evil":          "inject",
		},
	}, bearer())
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	if body["status"].(float64) != 200 {
		t.Fatalf("status want 200, got %v", body["status"])
	}
	headers := body["headers"].(map[string]any)
	if headers["content-type"] != "application/json" {
		t.Fatalf("content-type not passed through: %v", headers)
	}
	if headers["cache-control"] != "no-store" {
		t.Fatalf("cache-control not passed through: %v", headers)
	}
	if _, ok := headers["x-evil"]; ok {
		t.Fatalf("non-whitelisted response header leaked: %v", headers)
	}
	raw, err := base64.StdEncoding.DecodeString(body["bodyBase64"].(string))
	if err != nil || string(raw) != `{"ok":true}` {
		t.Fatalf("bodyBase64 round-trip failed: %q err=%v", raw, err)
	}

	// 上游视角：白名单头到达、非白名单头丢弃、UA 透传
	if gotAuth != "Bearer pixiv-token-secret" {
		t.Fatalf("authorization not forwarded: %q", gotAuth)
	}
	if gotLang != "zh-CN" {
		t.Fatalf("accept-language not forwarded: %q", gotLang)
	}
	if gotUA != "CustomUA/1.0" {
		t.Fatalf("client UA not passed through: %q", gotUA)
	}
	if gotEvil != "" {
		t.Fatalf("non-whitelisted header reached upstream: %q", gotEvil)
	}
}

// 正常 POST 透传：bodyBase64 解码后原样到达上游，content-type 转发。
func TestPostPassthrough(t *testing.T) {
	want := "grant_type=refresh_token&refresh_token=pixiv-rt"
	var gotBody, gotCT string
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		gotCT = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new"}`))
	})
	h := setup(t, upstream, nil)

	rec := doRelay(t, h, map[string]any{
		"method":     "POST",
		"url":        "https://oauth.secure.pixiv.net/auth/token",
		"headers":    map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
		"bodyBase64": base64.StdEncoding.EncodeToString([]byte(want)),
	}, bearer())
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if gotBody != want {
		t.Fatalf("upstream body want %q, got %q", want, gotBody)
	}
	if gotCT != "application/x-www-form-urlencoded" {
		t.Fatalf("content-type not forwarded: %q", gotCT)
	}
	body := decodeBody(t, rec)
	raw, _ := base64.StdEncoding.DecodeString(body["bodyBase64"].(string))
	if string(raw) != `{"access_token":"new"}` {
		t.Fatalf("response body mismatch: %q", raw)
	}
}

// UA 注入：客户端未传 User-Agent 时上游收到 PixivIOSApp/5.8.0（§6.1）。
func TestUAInjection(t *testing.T) {
	var gotUA string
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusOK)
	})
	h := setup(t, upstream, nil)

	rec := doRelay(t, h, map[string]any{
		"method": "GET",
		"url":    "https://app-api.pixiv.net/v1/user/detail",
	}, bearer())
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if gotUA != defaultUA {
		t.Fatalf("UA injection want %q, got %q", defaultUA, gotUA)
	}
}

func TestHostNotAllowed(t *testing.T) {
	h := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), nil)

	rec := doRelay(t, h, map[string]any{
		"method": "GET",
		"url":    "https://evil.example.com/steal",
	}, bearer())
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d: %s", rec.Code, rec.Body.String())
	}
	if code := errCode(t, rec); code != "FORBIDDEN" {
		t.Fatalf("error.code want FORBIDDEN, got %q", code)
	}
}

// RELAY_EXTRA_HOSTS 追加域名生效。
func TestExtraHosts(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := setup(t, upstream, []string{"api.example.com"})

	rec := doRelay(t, h, map[string]any{
		"method": "GET",
		"url":    "https://api.example.com/v1/x",
	}, bearer())
	if rec.Code != http.StatusOK {
		t.Fatalf("extra host want 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestMethodNotAllowed(t *testing.T) {
	h := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), nil)

	rec := doRelay(t, h, map[string]any{
		"method": "PATCH",
		"url":    "https://app-api.pixiv.net/v1/x",
	}, bearer())
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if code := errCode(t, rec); code != "VALIDATION_FAILED" {
		t.Fatalf("error.code want VALIDATION_FAILED, got %q", code)
	}
}

// 非 https scheme 一律 400（§4.1 全程 HTTPS）。
func TestSchemeNotHTTPS(t *testing.T) {
	h := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), nil)

	rec := doRelay(t, h, map[string]any{
		"method": "GET",
		"url":    "http://app-api.pixiv.net/v1/x",
	}, bearer())
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// bodyBase64 解码后 >1MB → 400（service 级单测；HTTP 层 DecodeJSON 的 1MB 上限会先拦住）。
func TestBodyTooLarge(t *testing.T) {
	svc := NewService(&http.Client{}, nil)
	big := base64.StdEncoding.EncodeToString(make([]byte, maxBodyBytes+1))
	_, err := svc.Do(context.Background(), &Request{
		Method:     "POST",
		URL:        "https://app-api.pixiv.net/v1/x",
		BodyBase64: big,
	})
	var ae *common.APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusBadRequest {
		t.Fatalf("want 400 APIError, got %v", err)
	}
}

// 超时钳制：缺省 30 s、上限 60 s（§6.1）。
func TestClampTimeout(t *testing.T) {
	if got := clampTimeout(0); got != 30*time.Second {
		t.Fatalf("default want 30s, got %v", got)
	}
	if got := clampTimeout(-5); got != 30*time.Second {
		t.Fatalf("negative want 30s, got %v", got)
	}
	if got := clampTimeout(5000); got != 5*time.Second {
		t.Fatalf("5s want 5s, got %v", got)
	}
	if got := clampTimeout(120000); got != 60*time.Second {
		t.Fatalf("over max want 60s, got %v", got)
	}
}

// 慢上游 + 小 timeoutMs → 上游超时映射 502 UPSTREAM_UNREACHABLE。
func TestUpstreamTimeout(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Second)
		w.WriteHeader(http.StatusOK)
	})
	h := setup(t, upstream, nil)

	rec := doRelay(t, h, map[string]any{
		"method":    "GET",
		"url":       "https://app-api.pixiv.net/v1/slow",
		"timeoutMs": 100,
	}, bearer())
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d: %s", rec.Code, rec.Body.String())
	}
	if code := errCode(t, rec); code != "UPSTREAM_UNREACHABLE" {
		t.Fatalf("error.code want UPSTREAM_UNREACHABLE, got %q", code)
	}
}

// 超大 timeoutMs 被钳制到 60 s 而非报错（快上游照常成功）。
func TestTimeoutClampedNotRejected(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := setup(t, upstream, nil)

	rec := doRelay(t, h, map[string]any{
		"method":    "GET",
		"url":       "https://app-api.pixiv.net/v1/x",
		"timeoutMs": 99999999,
	}, bearer())
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// 上游连接拒绝 → 502，错误体符合统一格式（§4.2）。
func TestUpstreamUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // 立即关闭 → 连接拒绝
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
	svc := NewService(&http.Client{Transport: &rewriteTransport{target: u}}, nil)
	mux := http.NewServeMux()
	RegisterRoutes(mux, svc, mw)
	h := common.RequestID(mux)

	rec := doRelay(t, h, map[string]any{
		"method": "GET",
		"url":    "https://app-api.pixiv.net/v1/x",
	}, bearer())
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d: %s", rec.Code, rec.Body.String())
	}
	errObj := decodeBody(t, rec)["error"].(map[string]any)
	if errObj["code"] != "UPSTREAM_UNREACHABLE" {
		t.Fatalf("error.code want UPSTREAM_UNREACHABLE, got %v", errObj["code"])
	}
	if errObj["requestId"] == nil || errObj["requestId"] == "" {
		t.Fatal("error.requestId missing")
	}
	if got := rec.Header().Get("X-Request-Id"); got == "" || got != errObj["requestId"] {
		t.Fatalf("requestId mismatch: header %q vs body %v", got, errObj["requestId"])
	}
}

// 上游响应 body >1MB → 502。
func TestUpstreamResponseTooLarge(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(make([]byte, maxBodyBytes+1))
	})
	h := setup(t, upstream, nil)

	rec := doRelay(t, h, map[string]any{
		"method": "GET",
		"url":    "https://www.pixiv.net/big",
	}, bearer())
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d: %s", rec.Code, rec.Body.String())
	}
}

// 未带 Authorization → 401 统一错误格式。
func TestMissingAuthorization(t *testing.T) {
	h := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), nil)

	rec := doRelay(t, h, map[string]any{
		"method": "GET",
		"url":    "https://app-api.pixiv.net/v1/x",
	}, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d: %s", rec.Code, rec.Body.String())
	}
	if code := errCode(t, rec); code != "INVALID_TOKEN" {
		t.Fatalf("error.code want INVALID_TOKEN, got %q", code)
	}
}

// 写端点限流 60/min（burst 10，key=accountID，§9）：第 11 个请求 429。
func TestRateLimited(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := setup(t, upstream, nil)

	body := map[string]any{"method": "GET", "url": "https://app-api.pixiv.net/v1/x"}
	var last *httptest.ResponseRecorder
	for i := 0; i < 11; i++ {
		last = doRelay(t, h, body, bearer())
	}
	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("11th request want 429, got %d: %s", last.Code, last.Body.String())
	}
	if code := errCode(t, last); code != "RATE_LIMITED" {
		t.Fatalf("error.code want RATE_LIMITED, got %q", code)
	}
	if last.Header().Get("Retry-After") == "" {
		t.Fatal("429 response missing Retry-After header")
	}
}

// 日志不落敏感字段：Pixiv token / bodyBase64 绝不出现（§6.1、§9）。
func TestLogNoSensitive(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := setup(t, upstream, nil)

	secretBody := base64.StdEncoding.EncodeToString([]byte("pixiv-refresh-secret"))
	rec := doRelay(t, h, map[string]any{
		"method": "POST",
		"url":    "https://oauth.secure.pixiv.net/auth/token?secret_query=1",
		"headers": map[string]string{
			"Authorization": "Bearer pixiv-token-secret",
		},
		"bodyBase64": secretBody,
	}, bearer())
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	out := buf.String()
	for _, s := range []string{"pixiv-token-secret", secretBody, "pixiv-refresh-secret", "secret_query"} {
		if strings.Contains(out, s) {
			t.Fatalf("log leaked sensitive value %q: %s", s, out)
		}
	}
}
