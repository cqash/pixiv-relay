package admin_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cqash/pixiv-relay/internal/admin"
	"github.com/cqash/pixiv-relay/internal/app"
	"github.com/cqash/pixiv-relay/internal/auth"
	"github.com/cqash/pixiv-relay/internal/cache"
	"github.com/cqash/pixiv-relay/internal/common"
	"github.com/cqash/pixiv-relay/internal/config"
	"github.com/cqash/pixiv-relay/internal/db"
	"github.com/cqash/pixiv-relay/internal/recover"
)

const testToken = "test-admin-token"

// testEnv 管理端测试环境：临时库 + 临时目录缓存/恢复服务 + 共享限流器 + httptest 服务。
type testEnv struct {
	db           *sql.DB
	cache        *cache.DiskLRU
	recoverSvc   *recover.Service
	writeLimiter *common.Limiter
	imgLimiter   *common.Limiter
	srv          *httptest.Server
	env          admin.EnvSnapshot
	startedAt    time.Time
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := db.Migrate(context.Background(), database); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	c, err := cache.Open(database, cache.Config{Dir: filepath.Join(t.TempDir(), "cache")})
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	recSvc, err := recover.NewService(database, c, &http.Client{}, recover.Config{
		Sources: []string{},
		TmpDir:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("new recover service: %v", err)
	}
	e := &testEnv{
		db:           database,
		cache:        c,
		recoverSvc:   recSvc,
		writeLimiter: common.NewLimiter(60, 10),
		imgLimiter:   common.NewLimiter(300, 30),
		env:          admin.EnvSnapshotFromEnv(),
		startedAt:    time.Now(),
	}
	e.mount(t, e.newService())
	return e
}

// newService 用同一批依赖重建管理端 Service（验证 DB 覆盖项重启后仍生效）。
func (e *testEnv) newService() *admin.Service {
	return admin.NewService(e.db, e.cache, e.recoverSvc, e.writeLimiter, e.imgLimiter, e.env, e.startedAt)
}

func (e *testEnv) mount(t *testing.T, svc *admin.Service) {
	t.Helper()
	if e.srv != nil {
		e.srv.Close()
	}
	mux := http.NewServeMux()
	admin.RegisterRoutes(mux, svc, testToken)
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
}

// do 发请求并解析 JSON 响应体。
func (e *testEnv) do(t *testing.T, method, path, token string, body any) (int, map[string]any) {
	t.Helper()
	var rdr *strings.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rdr = strings.NewReader(string(raw))
	} else {
		rdr = strings.NewReader("")
	}
	req, err := http.NewRequest(method, e.srv.URL+path, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// settingsOf 提取响应中的 settings 映射。
func settingsOf(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	m, ok := body["settings"].(map[string]any)
	if !ok {
		t.Fatalf("missing settings in response: %v", body)
	}
	return m
}

func settingSource(t *testing.T, settings map[string]any, key string) (float64, string) {
	t.Helper()
	info, ok := settings[key].(map[string]any)
	if !ok {
		t.Fatalf("missing setting %s: %v", key, settings)
	}
	v, _ := info["value"].(float64)
	src, _ := info["source"].(string)
	return v, src
}

// TestDisabledWhenTokenEmpty ADMIN_TOKEN 空 = 管理端不注册（app 层，§14.1）。
func TestDisabledWhenTokenEmpty(t *testing.T) {
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
		RecoverTmpDir: t.TempDir(),
		AdminToken:    "",
	}
	handler, err := app.NewWithClient(cfg, database, &http.Client{})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	// 未注册时 /admin/ 不命中任何 API 路由：PATCH 落到 SPA 兜底 "GET /" 的
	// 方法不匹配（405），GET 则回退 index.html 而非 admin JSON。二者都证明未注册。
	req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/admin/v1/settings", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer whatever")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("admin disabled: PATCH /admin/v1/settings = %d, want 404/405", resp.StatusCode)
	}

	// 配置 token 后应注册（无 token 401 而非 404/SPA 页面）。
	cfg2 := &config.Config{
		CacheDir:      filepath.Join(t.TempDir(), "cache"),
		RecoverTmpDir: t.TempDir(),
		AdminToken:    testToken,
	}
	handler2, err := app.NewWithClient(cfg2, database, &http.Client{})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	srv2 := httptest.NewServer(handler2)
	t.Cleanup(srv2.Close)
	resp2, err := http.Get(srv2.URL + "/admin/v1/overview")
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("admin enabled: GET /admin/v1/overview = %d, want 401", resp2.StatusCode)
	}
}

// TestAuthWrongToken 错误 token → 401 统一信封 INVALID_TOKEN（§4.2/§14.1）。
func TestAuthWrongToken(t *testing.T) {
	e := newTestEnv(t)
	code, body := e.do(t, http.MethodGet, "/admin/v1/overview", "wrong-token", nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", code)
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("missing error envelope: %v", body)
	}
	if errObj["code"] != "INVALID_TOKEN" {
		t.Fatalf("error code = %v, want INVALID_TOKEN", errObj["code"])
	}
	// 缺失 Authorization 头同样 401。
	code, _ = e.do(t, http.MethodGet, "/admin/v1/overview", "", nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("no token: status = %d, want 401", code)
	}
}

// TestAuthRateLimit 管理端认证路径 per-IP 限流 30/min（burst 5）：打爆后 429。
func TestAuthRateLimit(t *testing.T) {
	e := newTestEnv(t)
	for i := 0; i < 5; i++ {
		code, _ := e.do(t, http.MethodGet, "/admin/v1/overview", "bad", nil)
		if code != http.StatusUnauthorized {
			t.Fatalf("request %d: status = %d, want 401", i, code)
		}
	}
	code, body := e.do(t, http.MethodGet, "/admin/v1/overview", "bad", nil)
	if code != http.StatusTooManyRequests {
		t.Fatalf("6th request: status = %d, want 429 (%v)", code, body)
	}
	if body["error"].(map[string]any)["code"] != "RATE_LIMITED" {
		t.Fatalf("error body = %v", body)
	}
}

// TestSettingsSources GET /settings 三来源标记：default / env / db。
func TestSettingsSources(t *testing.T) {
	t.Setenv("RATE_IMG_PER_MIN", "123")
	e := newTestEnv(t)
	code, body := e.do(t, http.MethodGet, "/admin/v1/settings", testToken, nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d %v", code, body)
	}
	settings := settingsOf(t, body)

	// env 设置项 → source=env。
	if v, src := settingSource(t, settings, "rate_img_per_min"); v != 123 || src != "env" {
		t.Fatalf("rate_img_per_min = %v/%s, want 123/env", v, src)
	}
	// 未设置项 → 内置默认。
	if v, src := settingSource(t, settings, "recover_ttl_days"); v != 90 || src != "default" {
		t.Fatalf("recover_ttl_days = %v/%s, want 90/default", v, src)
	}

	// PATCH 后 → source=db，立即生效。
	code, body = e.do(t, http.MethodPatch, "/admin/v1/settings", testToken,
		map[string]any{"recover_ttl_days": 30, "recover_negative_ttl_days": 3})
	if code != http.StatusOK {
		t.Fatalf("patch status = %d %v", code, body)
	}
	settings = settingsOf(t, body)
	if v, src := settingSource(t, settings, "recover_ttl_days"); v != 30 || src != "db" {
		t.Fatalf("recover_ttl_days after patch = %v/%s, want 30/db", v, src)
	}
	// recover 队列 TTL 热更挂钩生效（读回 ttlMs 换算）。
	if ttl, neg := e.recoverSvc.TTLDays(); ttl != 30 || neg != 3 {
		t.Fatalf("recover TTLDays = %d/%d, want 30/3", ttl, neg)
	}

	// 重建 Service（模拟重启）：DB 覆盖项仍生效。
	e.mount(t, e.newService())
	code, body = e.do(t, http.MethodGet, "/admin/v1/settings", testToken, nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	settings = settingsOf(t, body)
	if v, src := settingSource(t, settings, "recover_ttl_days"); v != 30 || src != "db" {
		t.Fatalf("recover_ttl_days after rebuild = %v/%s, want 30/db", v, src)
	}
	if ttl, _ := e.recoverSvc.TTLDays(); ttl != 30 {
		t.Fatalf("recover TTLDays after rebuild = %d, want 30", ttl)
	}
}

// TestSettingsPatchValidation 未知键 / 非法值 → 400 且明确 message。
// 管理端认证路径有 30/min（burst 5）限流，用例多时每个用例用独立环境避免互相打爆。
func TestSettingsPatchValidation(t *testing.T) {
	t.Run("unknown key", func(t *testing.T) {
		e := newTestEnv(t)
		code, body := e.do(t, http.MethodPatch, "/admin/v1/settings", testToken,
			map[string]any{"no_such_key": 1})
		if code != http.StatusBadRequest {
			t.Fatalf("unknown key: status = %d, want 400", code)
		}
		msg := body["error"].(map[string]any)["message"].(string)
		if !strings.Contains(msg, "no_such_key") {
			t.Fatalf("message should name the key: %s", msg)
		}
	})

	for _, bad := range []map[string]any{
		{"cache_max_bytes": 0},
		{"cache_max_bytes": -5},
		{"cache_high_watermark": 1.5},
		{"cache_high_watermark": 0},
		{"rate_write_per_min": 0},
		{"recover_ttl_days": -1},
	} {
		t.Run("invalid value", func(t *testing.T) {
			e := newTestEnv(t)
			code, body := e.do(t, http.MethodPatch, "/admin/v1/settings", testToken, bad)
			if code != http.StatusBadRequest {
				t.Fatalf("patch %v: status = %d, want 400 (%v)", bad, code, body)
			}
		})
	}

	t.Run("mixed patch rejected atomically", func(t *testing.T) {
		e := newTestEnv(t)
		code, _ := e.do(t, http.MethodPatch, "/admin/v1/settings", testToken,
			map[string]any{"recover_ttl_days": 45, "cache_max_bytes": 0})
		if code != http.StatusBadRequest {
			t.Fatalf("mixed patch: status = %d, want 400", code)
		}
		var cnt int
		if err := e.db.QueryRow("SELECT COUNT(*) FROM settings WHERE key = 'recover_ttl_days'").Scan(&cnt); err != nil {
			t.Fatalf("query settings: %v", err)
		}
		if cnt != 0 {
			t.Fatalf("rejected patch must not persist any key")
		}
	})
}

// TestSettingsPatchCacheLimits PATCH cache_max_bytes 后 cache.SetLimits 立即生效。
func TestSettingsPatchCacheLimits(t *testing.T) {
	e := newTestEnv(t)
	code, body := e.do(t, http.MethodPatch, "/admin/v1/settings", testToken,
		map[string]any{"cache_max_bytes": 5000, "cache_high_watermark": 0.5})
	if code != http.StatusOK {
		t.Fatalf("patch status = %d %v", code, body)
	}
	cfg := e.cache.Config()
	if cfg.MaxBytes != 5000 {
		t.Fatalf("cache MaxBytes = %d, want 5000", cfg.MaxBytes)
	}
	if cfg.HighWatermark != 0.5 {
		t.Fatalf("cache HighWatermark = %v, want 0.5", cfg.HighWatermark)
	}
}

// TestCacheStatsAndEvict 缓存统计与手动淘汰：返回释放字节/条数。
func TestCacheStatsAndEvict(t *testing.T) {
	e := newTestEnv(t)

	// 写入三个缓存条目。
	for _, u := range []string{"https://a/1.jpg", "https://a/2.jpg", "https://a/3.jpg"} {
		stored, err := e.cache.Put(context.Background(), cache.Key(u), "image/jpeg",
			strings.NewReader(strings.Repeat("x", 1000)))
		if err != nil || !stored {
			t.Fatalf("put %s: stored=%v err=%v", u, stored, err)
		}
	}

	code, body := e.do(t, http.MethodGet, "/admin/v1/cache/stats", testToken, nil)
	if code != http.StatusOK {
		t.Fatalf("stats status = %d", code)
	}
	if body["bytes"].(float64) != 3000 || body["entries"].(float64) != 3 {
		t.Fatalf("stats = %v, want 3000 bytes / 3 entries", body)
	}
	if body["layout"] != "sharded" || body["dir"] == "" {
		t.Fatalf("stats missing layout/dir: %v", body)
	}

	// 调低上限使总量超水位，随后手动淘汰。
	code, _ = e.do(t, http.MethodPatch, "/admin/v1/settings", testToken,
		map[string]any{"cache_max_bytes": 1000, "cache_high_watermark": 0.5})
	if code != http.StatusOK {
		t.Fatalf("patch status = %d", code)
	}
	code, body = e.do(t, http.MethodPost, "/admin/v1/cache/evict", testToken, nil)
	if code != http.StatusOK {
		t.Fatalf("evict status = %d", code)
	}
	if body["freedBytes"].(float64) <= 0 || body["freedEntries"].(float64) <= 0 {
		t.Fatalf("evict should free something: %v", body)
	}
	if body["bytes"].(float64) > 500 {
		t.Fatalf("after evict bytes = %v, want <= 500 (watermark)", body["bytes"])
	}
}

// TestOverview 概览端点：版本/运行时长/计数/生效设置。
func TestOverview(t *testing.T) {
	e := newTestEnv(t)
	seedAccount(t, e.db, 1)
	seedAccount(t, e.db, 2)

	code, body := e.do(t, http.MethodGet, "/admin/v1/overview", testToken, nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d %v", code, body)
	}
	if body["serverVersion"] != "1.1.0" {
		t.Fatalf("serverVersion = %v", body["serverVersion"])
	}
	if body["accounts"].(float64) != 2 {
		t.Fatalf("accounts = %v, want 2", body["accounts"])
	}
	if _, ok := body["uptimeSec"].(float64); !ok {
		t.Fatalf("missing uptimeSec: %v", body)
	}
	if _, ok := body["cache"].(map[string]any); !ok {
		t.Fatalf("missing cache: %v", body)
	}
	if _, ok := body["recoverCache"].(map[string]any); !ok {
		t.Fatalf("missing recoverCache: %v", body)
	}
	settingsOf(t, body)
}

// seedAccount 直接插入账号行（created_at 递增保证分页顺序确定）。
func seedAccount(t *testing.T, database *sql.DB, n int64) {
	t.Helper()
	if _, err := database.Exec(
		"INSERT INTO accounts (id, account_key_hash, created_at) VALUES (?, ?, ?)",
		n, "hash-"+string(rune('a'+n)), 1700000000000+n); err != nil {
		t.Fatalf("seed account %d: %v", n, err)
	}
}

// TestAccountsPagination 账号列表游标分页 + 计数子查询。
func TestAccountsPagination(t *testing.T) {
	e := newTestEnv(t)
	for i := int64(1); i <= 3; i++ {
		seedAccount(t, e.db, i)
	}
	// 账号 2 加两台设备与一条同步条目。
	for _, d := range []int64{21, 22} {
		if _, err := e.db.Exec(`INSERT INTO devices
			(id, account_id, device_name, access_token_hash, refresh_token_hash, created_at, access_expires_at, refresh_expires_at)
			VALUES (?, 2, 'dev', ?, ?, 1700000000000, 1, 1)`,
			d, "ah-"+string(rune('a'+d)), "rh-"+string(rune('a'+d))); err != nil {
			t.Fatalf("seed device: %v", err)
		}
	}
	if _, err := e.db.Exec(`INSERT INTO sync_entries (account_id, domain, "key", data, updated_at, seq)
		VALUES (2, 'history', 'k1', '{}', 1700000000000, 1)`); err != nil {
		t.Fatalf("seed sync entry: %v", err)
	}

	code, body := e.do(t, http.MethodGet, "/admin/v1/accounts?limit=2", testToken, nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d %v", code, body)
	}
	items := body["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("page1 items = %d, want 2", len(items))
	}
	next, _ := body["nextCursor"].(string)
	if next == "" {
		t.Fatalf("page1 nextCursor empty")
	}

	code, body = e.do(t, http.MethodGet, "/admin/v1/accounts?limit=2&cursor="+next, testToken, nil)
	if code != http.StatusOK {
		t.Fatalf("page2 status = %d", code)
	}
	items = body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("page2 items = %d, want 1", len(items))
	}
	if body["nextCursor"] != "" {
		t.Fatalf("page2 nextCursor = %v, want empty", body["nextCursor"])
	}

	// 计数断言：账号 2 应带 deviceCount=2 / syncEntryCount=1。
	code, body = e.do(t, http.MethodGet, "/admin/v1/accounts", testToken, nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	for _, it := range body["items"].([]any) {
		m := it.(map[string]any)
		if m["id"].(float64) == 2 {
			if m["deviceCount"].(float64) != 2 || m["syncEntryCount"].(float64) != 1 {
				t.Fatalf("account 2 counts = %v", m)
			}
		}
	}
}

// TestDevicesList 账号设备列表。
func TestDevicesList(t *testing.T) {
	e := newTestEnv(t)
	seedAccount(t, e.db, 1)
	if _, err := e.db.Exec(`INSERT INTO devices
		(id, account_id, device_name, access_token_hash, refresh_token_hash, created_at, access_expires_at, refresh_expires_at)
		VALUES (11, 1, 'my-phone', 'ah11', 'rh11', 1700000000000, 1700003600000, 1800000000000)`); err != nil {
		t.Fatalf("seed device: %v", err)
	}

	code, body := e.do(t, http.MethodGet, "/admin/v1/accounts/1/devices", testToken, nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d %v", code, body)
	}
	items := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("devices = %d, want 1", len(items))
	}
	dev := items[0].(map[string]any)
	if dev["deviceName"] != "my-phone" ||
		dev["accessExpiresAt"].(float64) != 1700003600000 ||
		dev["refreshExpiresAt"].(float64) != 1800000000000 {
		t.Fatalf("device item = %v", dev)
	}
}

// TestDeleteDeviceRevokesToken 吊销设备后原 access token 立即 401。
func TestDeleteDeviceRevokesToken(t *testing.T) {
	e := newTestEnv(t)

	// 真实注册一台设备拿到 token。
	authSvc := auth.NewService(e.db, nil)
	pair, err := authSvc.Register(context.Background(), "probe-device", "", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	authMw, err := auth.NewMiddleware(e.db, nil)
	if err != nil {
		t.Fatalf("auth middleware: %v", err)
	}
	// 受 auth 保护的探测端点挂在同一 mux 需要重建——直接新建探测服务。
	probe := httptest.NewServer(authMw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))
	t.Cleanup(probe.Close)

	probeReq := func() int {
		req, _ := http.NewRequest(http.MethodGet, probe.URL, nil)
		req.Header.Set("Authorization", "Bearer "+pair.AccessToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if code := probeReq(); code != http.StatusOK {
		t.Fatalf("probe before delete = %d, want 200", code)
	}

	// 找到设备 ID 并经管理端吊销。
	var deviceID int64
	if err := e.db.QueryRow("SELECT id FROM devices WHERE device_name = 'probe-device'").Scan(&deviceID); err != nil {
		t.Fatalf("lookup device: %v", err)
	}
	code, _ := e.do(t, http.MethodDelete, "/admin/v1/devices/"+
		strconv.FormatInt(deviceID, 10), testToken, nil)
	if code != http.StatusOK {
		t.Fatalf("delete device = %d", code)
	}
	if code := probeReq(); code != http.StatusUnauthorized {
		t.Fatalf("probe after delete = %d, want 401", code)
	}

	// 不存在 404。
	code, _ = e.do(t, http.MethodDelete, "/admin/v1/devices/99999", testToken, nil)
	if code != http.StatusNotFound {
		t.Fatalf("delete missing device = %d, want 404", code)
	}
}

// TestDeleteAccountCascade 删除账号：事务级联清理四张关联表；不存在 404。
func TestDeleteAccountCascade(t *testing.T) {
	e := newTestEnv(t)
	seedAccount(t, e.db, 1)
	seedAccount(t, e.db, 2)
	if _, err := e.db.Exec(`INSERT INTO devices
		(id, account_id, device_name, access_token_hash, refresh_token_hash, created_at, access_expires_at, refresh_expires_at)
		VALUES (11, 1, 'd1', 'ah11', 'rh11', 1, 1, 1)`); err != nil {
		t.Fatalf("seed device: %v", err)
	}
	if _, err := e.db.Exec(`INSERT INTO sync_entries (account_id, domain, "key", data, updated_at, seq)
		VALUES (1, 'history', 'k1', '{}', 1, 1)`); err != nil {
		t.Fatalf("seed sync_entries: %v", err)
	}
	if _, err := e.db.Exec(`INSERT INTO sync_domains (account_id, domain, sync_token)
		VALUES (1, 'history', 'st_1_x')`); err != nil {
		t.Fatalf("seed sync_domains: %v", err)
	}
	if _, err := e.db.Exec(`INSERT INTO recover_cache (account_id, pid, status, expire)
		VALUES (1, '123', 'ready', 1)`); err != nil {
		t.Fatalf("seed recover_cache: %v", err)
	}

	code, _ := e.do(t, http.MethodDelete, "/admin/v1/accounts/1", testToken, nil)
	if code != http.StatusOK {
		t.Fatalf("delete account = %d", code)
	}
	for table, where := range map[string]string{
		"devices":       "account_id = 1",
		"sync_entries":  "account_id = 1",
		"sync_domains":  "account_id = 1",
		"recover_cache": "account_id = 1",
	} {
		var cnt int
		if err := e.db.QueryRow("SELECT COUNT(*) FROM " + table + " WHERE " + where).Scan(&cnt); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if cnt != 0 {
			t.Fatalf("%s rows for account 1 = %d, want 0", table, cnt)
		}
	}
	var accountCnt int
	if err := e.db.QueryRow("SELECT COUNT(*) FROM accounts WHERE id = 1").Scan(&accountCnt); err != nil {
		t.Fatalf("count accounts: %v", err)
	}
	if accountCnt != 0 {
		t.Fatalf("account 1 should be deleted")
	}
	// 账号 2 不受影响。
	var cnt int
	if err := e.db.QueryRow("SELECT COUNT(*) FROM accounts WHERE id = 2").Scan(&cnt); err != nil {
		t.Fatalf("count accounts: %v", err)
	}
	if cnt != 1 {
		t.Fatalf("account 2 should survive")
	}

	// 不存在 404。
	code, _ = e.do(t, http.MethodDelete, "/admin/v1/accounts/99999", testToken, nil)
	if code != http.StatusNotFound {
		t.Fatalf("delete missing account = %d, want 404", code)
	}
}
