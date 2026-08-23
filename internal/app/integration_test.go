package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arkpix/relay/internal/config"
	"github.com/arkpix/relay/internal/db"
)

// 本文件为 M8 端到端集成测试：httptest.NewServer 起真实 HTTP 服务，
// 按设计文档 §13 端点清单串通一条完整用户旅程。出网经统一注入点
// NewWithClient 的 blockRewrite Transport 改写到 httptest mock 上游，
// 未登记 host 直接报错，测试绝不触达真实外网；数据目录一律 t.TempDir()。

// blockRewrite 测试出网 Transport：按请求 host 改写到对应 httptest 上游
// （与 recover 模块测试同款模式，M8 将其提升到 app 层统一注入）。
type blockRewrite struct{ targets map[string]string } // host -> httptest host:port

func (t *blockRewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	tgt, ok := t.targets[strings.ToLower(req.URL.Hostname())]
	if !ok {
		return nil, fmt.Errorf("test: blocked real upstream host %s", req.URL.Hostname())
	}
	r2 := req.Clone(req.Context())
	r2.URL.Scheme = "http"
	r2.URL.Host = tgt
	return http.DefaultTransport.RoundTrip(r2)
}

// e2eEnv 端到端环境：真实 HTTP 服务 + 库句柄（供安全自查直查）+ mock 上游计数。
type e2eEnv struct {
	srv      *httptest.Server
	db       *sql.DB
	upstream *hitCounter
}

// hitCounter 统计 mock 上游被触达次数（验证缓存 HIT 不再出网）。
type hitCounter struct {
	handler http.Handler
	hits    chan struct{}
}

func (h *hitCounter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	select {
	case h.hits <- struct{}{}:
	default:
	}
	h.handler.ServeHTTP(w, r)
}

func (h *hitCounter) count() int { return len(h.hits) }

// startE2E 构建端到端环境：临时库 + 临时数据目录 + 全量路由（NewWithClient
// 注入改写 Transport），upstream 为 mock 上游 handler，upHosts 为登记改写的
// 白名单域名（全部指向同一个 mock 上游）。
func startE2E(t *testing.T, cfg *config.Config, upstream http.Handler, upHosts ...string) *e2eEnv {
	t.Helper()
	hc := &hitCounter{handler: upstream, hits: make(chan struct{}, 1024)}
	upSrv := httptest.NewServer(hc)
	t.Cleanup(upSrv.Close)
	u, err := url.Parse(upSrv.URL)
	if err != nil {
		t.Fatalf("parse upstream url: %v", err)
	}
	targets := make(map[string]string, len(upHosts))
	for _, h := range upHosts {
		targets[h] = u.Host
	}

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

	handler, err := NewWithClient(cfg, database, &http.Client{Transport: &blockRewrite{targets: targets}})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &e2eEnv{srv: srv, db: database, upstream: hc}
}

// e2eConfig 端到端默认配置：限流放大避免轮询触发 429 干扰断言；恢复源全量声明。
func e2eConfig() *config.Config {
	return &config.Config{
		RecoverSources:      []string{"snapshot", "pixiv_cat", "pixiv_re"},
		ImgExtraHosts:       []string{"i.pixiv.cat", "i.pixiv.re"},
		RateWritePerMin:     100000,
		RateImgPerMin:       100000,
		RateRegisterPerHour: 100000,
	}
}

// doJSON 发 JSON 请求并返回状态码与解码后的响应体。
func doJSON(t *testing.T, method, url, token string, body any) (int, map[string]any, http.Header) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	out := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("%s %s: decode body: %v (raw: %s)", method, url, err, raw)
		}
	}
	return resp.StatusCode, out, resp.Header
}

// makeJPEG 生成合法 JPEG 字节（> 1 KB，过恢复模块内容校验）。
func makeJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	im := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			im.Set(x, y, color.RGBA{uint8(x * 3), uint8(y * 5), uint8(x + y), 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, im, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	b := buf.Bytes()
	for len(b) <= 1024 {
		b = append(b, 0) // 尾部填充不影响图片头解码
	}
	return b
}

// TestUserJourney 按 §13 端点清单串通完整用户旅程：
// healthz → 注册设备A → 同 accountKey 注册设备B → refresh 轮换 → relay 中继 →
// img MISS→HIT → sync push/pull（双设备增量一致）→ recover 202→轮询 200→
// 包装 URL 取图 → 各端点无 token 401（统一错误格式）→ SPA 托管。
func TestUserJourney(t *testing.T) {
	jpg := makeJPEG(t, 64, 48)
	jpg2 := makeJPEG(t, 32, 32)
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/illust/recommended": // relay mock
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/img-original/img/x/555_p0.jpg": // recover 快照 URL
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(jpg)
		case "/img-original/img/y/100_p0.jpg": // img MISS/HIT 用
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(jpg2)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	e := startE2E(t, e2eConfig(), upstream,
		"app-api.pixiv.net", "oauth.secure.pixiv.net", "www.pixiv.net",
		"i.pximg.net", "i.pixiv.cat", "i.pixiv.re")
	base := e.srv.URL

	// 1. 存活探针（无需鉴权）。
	code, body, _ := doJSON(t, http.MethodGet, base+"/healthz", "", nil)
	if code != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("healthz = %d %v", code, body)
	}

	// 2. 设备 A 注册：拿 token + accountKey。
	code, regA, _ := doJSON(t, http.MethodPost, base+"/auth/v1/register", "",
		map[string]any{"deviceName": "devA"})
	if code != http.StatusOK {
		t.Fatalf("register A = %d %v", code, regA)
	}
	tokenA, _ := regA["accessToken"].(string)
	refreshA, _ := regA["refreshToken"].(string)
	accountKey, _ := regA["accountKey"].(string)
	if tokenA == "" || refreshA == "" || accountKey == "" {
		t.Fatalf("register A missing credentials: %v", regA)
	}
	if regA["serverVersion"] != "1.1.0" {
		t.Fatalf("serverVersion = %v", regA["serverVersion"])
	}

	// 3. 设备 B 带 accountKey 注册 → 加入同一账号。
	code, regB, _ := doJSON(t, http.MethodPost, base+"/auth/v1/register", "",
		map[string]any{"deviceName": "devB", "accountKey": accountKey})
	if code != http.StatusOK {
		t.Fatalf("register B = %d %v", code, regB)
	}
	tokenB, _ := regB["accessToken"].(string)
	if tokenB == "" || tokenB == tokenA {
		t.Fatalf("register B token invalid: %v", regB)
	}

	// 4. refresh 轮换：旧 refreshToken 单次使用（401）。
	code, ref, _ := doJSON(t, http.MethodPost, base+"/auth/v1/refresh", "",
		map[string]any{"refreshToken": refreshA})
	if code != http.StatusOK {
		t.Fatalf("refresh = %d %v", code, ref)
	}
	tokenA, _ = ref["accessToken"].(string)
	if tokenA == "" {
		t.Fatalf("refresh missing accessToken: %v", ref)
	}
	code, errBody, _ := doJSON(t, http.MethodPost, base+"/auth/v1/refresh", "",
		map[string]any{"refreshToken": refreshA})
	if code != http.StatusUnauthorized || errCodeOf(errBody) != "INVALID_TOKEN" {
		t.Fatalf("reuse old refreshToken = %d %v, want 401 INVALID_TOKEN", code, errBody)
	}

	// 5. relay 中继：透传 mock 上游响应。
	code, relayResp, _ := doJSON(t, http.MethodPost, base+"/relay/v1/request", tokenA,
		map[string]any{
			"method": "GET",
			"url":    "https://app-api.pixiv.net/v1/illust/recommended?filter=for_android",
			"headers": map[string]string{
				"Authorization": "Bearer pixiv-token-secret",
			},
		})
	if code != http.StatusOK || relayResp["status"].(float64) != 200 {
		t.Fatalf("relay = %d %v", code, relayResp)
	}
	raw, err := base64.StdEncoding.DecodeString(relayResp["bodyBase64"].(string))
	if err != nil || string(raw) != `{"ok":true}` {
		t.Fatalf("relay bodyBase64 = %q err=%v", raw, err)
	}

	// 6. img 图片中继：MISS 200 → 再请求 HIT，字节一致，HIT 不再出网。
	imgURL := base + "/img/v1/fetch?url=" + url.QueryEscape("https://i.pximg.net/img-original/img/y/100_p0.jpg")
	missCode, missBody, missHdr := fetchRaw(t, imgURL, tokenA)
	if missCode != http.StatusOK || missHdr.Get("X-Cache") != "MISS" {
		t.Fatalf("img MISS = %d X-Cache=%q", missCode, missHdr.Get("X-Cache"))
	}
	if !bytes.Equal(missBody, jpg2) {
		t.Fatal("img MISS body mismatch")
	}
	upHits := e.upstream.count()
	hitCode, hitBody, hitHdr := fetchRaw(t, imgURL, tokenA)
	if hitCode != http.StatusOK || hitHdr.Get("X-Cache") != "HIT" {
		t.Fatalf("img HIT = %d X-Cache=%q", hitCode, hitHdr.Get("X-Cache"))
	}
	if !bytes.Equal(hitBody, jpg2) {
		t.Fatal("img HIT body mismatch")
	}
	if got := e.upstream.count(); got != upHits {
		t.Fatalf("img HIT must not hit upstream: %d -> %d", upHits, got)
	}

	// 7. sync：设备 A push history + bookmark_snapshot → A/B pull 一致 → 增量。
	nowMs := time.Now().UnixMilli()
	code, push1, _ := doJSON(t, http.MethodPost, base+"/sync/v1/push", tokenA,
		map[string]any{
			"domain": "history",
			"items": []map[string]any{
				{"key": "h1", "data": map[string]any{"illustId": 101, "title": "第一"}, "updatedAt": nowMs},
				{"key": "h2", "data": map[string]any{"illustId": 102}, "updatedAt": nowMs + 1},
			},
		})
	if code != http.StatusOK || push1["accepted"].(float64) != 2 {
		t.Fatalf("push history = %d %v", code, push1)
	}
	histToken, _ := push1["syncToken"].(string)
	if histToken == "" {
		t.Fatalf("push history missing syncToken: %v", push1)
	}

	code, pushSnap, _ := doJSON(t, http.MethodPost, base+"/sync/v1/push", tokenA,
		map[string]any{
			"domain": "bookmark_snapshot",
			"items": []map[string]any{
				{"key": "555", "data": map[string]any{
					"illustId": 555, "title": "已删除作品", "userName": "画师",
					"width": 64, "height": 48,
					"imageUrls": []string{"https://i.pximg.net/img-original/img/x/555_p0.jpg"},
				}, "updatedAt": nowMs + 2},
			},
		})
	if code != http.StatusOK || pushSnap["accepted"].(float64) != 1 {
		t.Fatalf("push snapshot = %d %v", code, pushSnap)
	}

	// 设备 A 全量 pull。
	code, pullA, _ := doJSON(t, http.MethodGet, base+"/sync/v1/pull?domain=history", tokenA, nil)
	if code != http.StatusOK || len(pullA["items"].([]any)) != 2 {
		t.Fatalf("pull A = %d %v", code, pullA)
	}
	// 设备 B（同账号）全量 pull：数据一致。
	code, pullB, _ := doJSON(t, http.MethodGet, base+"/sync/v1/pull?domain=history", tokenB, nil)
	if code != http.StatusOK || len(pullB["items"].([]any)) != 2 {
		t.Fatalf("pull B = %d %v", code, pullB)
	}
	if pullB["items"].([]any)[0].(map[string]any)["key"] != "h1" {
		t.Fatalf("pull B items = %v", pullB["items"])
	}

	// 设备 A 再 push 一条，设备 B 带 since 增量 pull：只拿到新条目。
	code, push2, _ := doJSON(t, http.MethodPost, base+"/sync/v1/push", tokenA,
		map[string]any{
			"domain":    "history",
			"baseToken": histToken,
			"items": []map[string]any{
				{"key": "h3", "data": map[string]any{"illustId": 103}, "updatedAt": nowMs + 3},
			},
		})
	if code != http.StatusOK || push2["accepted"].(float64) != 1 {
		t.Fatalf("push h3 = %d %v", code, push2)
	}
	code, pullB2, _ := doJSON(t, http.MethodGet,
		base+"/sync/v1/pull?domain=history&since="+url.QueryEscape(histToken), tokenB, nil)
	if code != http.StatusOK {
		t.Fatalf("incremental pull B = %d %v", code, pullB2)
	}
	items := pullB2["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["key"] != "h3" {
		t.Fatalf("incremental pull B items = %v, want only h3", items)
	}

	// 8. recover：202 入队 → 轮询 200（快照源）→ 包装 URL 实际取到图（img HIT）。
	code, recBody, _ := doJSON(t, http.MethodGet, base+"/recover/v1/illust/555", tokenA, nil)
	if code != http.StatusAccepted || recBody["status"] != "fetching" {
		t.Fatalf("recover first = %d %v, want 202 fetching", code, recBody)
	}
	var ready map[string]any
	deadline := time.Now().Add(30 * time.Second)
	for {
		code, ready, _ = doJSON(t, http.MethodGet, base+"/recover/v1/illust/555", tokenA, nil)
		if code == http.StatusOK {
			break
		}
		if code != http.StatusAccepted {
			t.Fatalf("recover poll = %d %v", code, ready)
		}
		if time.Now().After(deadline) {
			t.Fatal("recover poll timeout (still fetching after 30s)")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if ready["status"] != "ready" || ready["source"] != "snapshot" {
		t.Fatalf("recover ready = %v, want source=snapshot", ready)
	}
	pages, ok := ready["pages"].([]any)
	if !ok || len(pages) != 1 {
		t.Fatalf("recover pages = %v, want 1", ready["pages"])
	}
	pageURL, _ := pages[0].(map[string]any)["url"].(string)
	if !strings.Contains(pageURL, "/img/v1/fetch?url=") {
		t.Fatalf("page url not wrapped: %s", pageURL)
	}
	imgCode, imgBody, imgHdr := fetchRaw(t, pageURL, tokenA)
	if imgCode != http.StatusOK || !bytes.Equal(imgBody, jpg) {
		t.Fatalf("fetch recovered page = %d (bytes match=%v)", imgCode, bytes.Equal(imgBody, jpg))
	}
	if imgHdr.Get("X-Cache") != "HIT" {
		t.Fatalf("recovered page should be cache HIT, got %q", imgHdr.Get("X-Cache"))
	}

	// 9. 无 token 访问各端点 → 401 + 统一错误格式（requestId 头体一致）。
	noAuth := []struct {
		method, path string
	}{
		{http.MethodPost, "/relay/v1/request"},
		{http.MethodGet, "/img/v1/fetch?url=https://i.pximg.net/x.jpg"},
		{http.MethodPost, "/sync/v1/push"},
		{http.MethodGet, "/sync/v1/pull?domain=history"},
		{http.MethodGet, "/recover/v1/illust/555"},
	}
	for _, ep := range noAuth {
		code, body, hdr := doJSON(t, ep.method, base+ep.path, "",
			map[string]any{"domain": "history", "method": "GET", "url": "https://app-api.pixiv.net/v1/x"})
		if code != http.StatusUnauthorized {
			t.Fatalf("%s no-auth = %d, want 401", ep.path, code)
		}
		errObj, ok := body["error"].(map[string]any)
		if !ok || errObj["code"] != "INVALID_TOKEN" || errObj["message"] == "" {
			t.Fatalf("%s error body = %v, want unified INVALID_TOKEN", ep.path, body)
		}
		if errObj["requestId"] == "" || errObj["requestId"] != hdr.Get("X-Request-Id") {
			t.Fatalf("%s requestId mismatch: body=%v header=%q", ep.path, errObj["requestId"], hdr.Get("X-Request-Id"))
		}
	}

	// 10. SPA 托管：GET / 返回内嵌 index.html。
	req, err := http.NewRequest(http.MethodGet, base+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	html, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(html), "ArkPix") {
		t.Fatalf("GET / = %d, want embedded SPA index.html", resp.StatusCode)
	}
}

// fetchRaw GET 取原始字节（图片端点）。
func fetchRaw(t *testing.T, url, token string) (int, []byte, http.Header) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, raw, resp.Header
}

// errCodeOf 取统一错误体的 error.code。
func errCodeOf(body map[string]any) string {
	errObj, _ := body["error"].(map[string]any)
	code, _ := errObj["code"].(string)
	return code
}
