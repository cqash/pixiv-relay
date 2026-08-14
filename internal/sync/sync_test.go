package sync

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	stdsync "sync"
	"testing"
	"time"

	"github.com/arkpix/relay/internal/auth"
	"github.com/arkpix/relay/internal/common"
	"github.com/arkpix/relay/internal/db"
)

// setup 构建测试环境：临时库（t.TempDir）+ auth 路由 + sync 路由（挂鉴权）。
// 外层包 RequestID 使错误响应带 requestId。
func setup(t *testing.T) (http.Handler, *sql.DB, *Service) {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := db.Migrate(context.Background(), database); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	svc, err := NewService(context.Background(), database)
	if err != nil {
		t.Fatalf("new sync service: %v", err)
	}
	mux := http.NewServeMux()
	auth.RegisterRoutes(mux, auth.NewService(database, nil))
	mw, err := auth.NewMiddleware(database, nil)
	if err != nil {
		t.Fatalf("new middleware: %v", err)
	}
	RegisterRoutes(mux, svc, mw)
	return common.RequestID(mux), database, svc
}

// doJSON 发起 JSON 请求。每次调用使用唯一 RemoteAddr，避免触发注册端点限流。
var requestSeq int

func doJSON(t *testing.T, h http.Handler, method, path string, body any, token string) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
	}
	requestSeq++
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	req.RemoteAddr = fmt.Sprintf("192.0.2.%d:12345", requestSeq%250+1)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
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

// registerDevice 注册设备；accountKey 非空时加入已有账号（多设备场景）。
// 返回 accessToken 与 accountKey。
func registerDevice(t *testing.T, h http.Handler, accountKey string) (string, string) {
	t.Helper()
	body := map[string]any{"deviceName": "dev"}
	if accountKey != "" {
		body["accountKey"] = accountKey
	}
	rec := doJSON(t, h, http.MethodPost, "/auth/v1/register", body, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("register want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	resp := decodeBody(t, rec)
	return resp["accessToken"].(string), resp["accountKey"].(string)
}

// item 构造 push 条目。
func item(key string, data map[string]any, updatedAt int64, deleted bool) map[string]any {
	return map[string]any{"key": key, "data": data, "updatedAt": updatedAt, "deleted": deleted}
}

// pushOK 发起 push 并断言 200，返回响应体。
func pushOK(t *testing.T, h http.Handler, token, domain, baseToken string, items []map[string]any) map[string]any {
	t.Helper()
	rec := doJSON(t, h, http.MethodPost, "/sync/v1/push",
		map[string]any{"domain": domain, "baseToken": baseToken, "items": items}, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("push want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	return decodeBody(t, rec)
}

// pullOK 发起 pull 并断言 200，返回响应体。
func pullOK(t *testing.T, h http.Handler, token, domain, since string, limit int) map[string]any {
	t.Helper()
	path := fmt.Sprintf("/sync/v1/pull?domain=%s&since=%s&limit=%d", domain, since, limit)
	rec := doJSON(t, h, http.MethodGet, path, nil, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("pull want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	return decodeBody(t, rec)
}

// pullItems 取 pull 响应的 items 数组。
func pullItems(t *testing.T, body map[string]any) []any {
	t.Helper()
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("items not array: %v", body)
	}
	return items
}

// findItem 按 key 查找 pull 条目。
func findItem(t *testing.T, items []any, key string) map[string]any {
	t.Helper()
	for _, it := range items {
		m := it.(map[string]any)
		if m["key"] == key {
			return m
		}
	}
	return nil
}

// TestPushPullLoop 闭环：设备 A push 3 条，同账号设备 B pull 全量拿到。
func TestPushPullLoop(t *testing.T) {
	h, _, _ := setup(t)
	tokA, accountKey := registerDevice(t, h, "")
	tokB, _ := registerDevice(t, h, accountKey)

	push := pushOK(t, h, tokA, "history", "", []map[string]any{
		item("1001", map[string]any{"title": "a", "viewed_at": 1}, 1000, false),
		item("1002", map[string]any{"title": "b", "viewed_at": 2}, 1001, false),
		item("1003", map[string]any{"title": "c", "viewed_at": 3}, 1002, false),
	})
	if push["accepted"].(float64) != 3 {
		t.Fatalf("accepted want 3, got %v", push["accepted"])
	}
	tok, _ := push["syncToken"].(string)
	if len(tok) < 3 || tok[:3] != "st_" {
		t.Fatalf("syncToken prefix wrong: %v", tok)
	}
	if c, ok := push["conflicts"].([]any); !ok || len(c) != 0 {
		t.Fatalf("conflicts want empty array, got %v", push["conflicts"])
	}

	pull := pullOK(t, h, tokB, "history", "", 100)
	items := pullItems(t, pull)
	if len(items) != 3 {
		t.Fatalf("pull want 3 items, got %d", len(items))
	}
	got := findItem(t, items, "1002")
	if got["data"].(map[string]any)["title"] != "b" {
		t.Fatalf("data roundtrip wrong: %v", got["data"])
	}
	if got["deleted"].(bool) {
		t.Fatal("deleted should be false")
	}
	if pull["hasMore"].(bool) {
		t.Fatal("hasMore should be false")
	}
	if pull["syncToken"] != tok {
		t.Fatalf("pull syncToken want current %v, got %v", tok, pull["syncToken"])
	}
}

// TestLWW 同 key 按 updatedAt 大者胜；墓碑覆盖旧数据；重复 push 幂等。
func TestLWW(t *testing.T) {
	h, _, _ := setup(t)
	tok, _ := registerDevice(t, h, "")

	pushOK(t, h, tok, "settings", "", []map[string]any{
		item("theme", map[string]any{"v": "dark"}, 1000, false),
	})
	// 旧写到后：不覆盖
	p := pushOK(t, h, tok, "settings", "", []map[string]any{
		item("theme", map[string]any{"v": "light"}, 500, false),
	})
	if p["accepted"].(float64) != 0 {
		t.Fatalf("stale write should be skipped, accepted=%v", p["accepted"])
	}
	items := pullItems(t, pullOK(t, h, tok, "settings", "", 100))
	if got := findItem(t, items, "theme"); got["data"].(map[string]any)["v"] != "dark" {
		t.Fatalf("LWW stale write overwrote: %v", got["data"])
	}
	// 墓碑覆盖旧数据
	p = pushOK(t, h, tok, "settings", "", []map[string]any{
		item("theme", map[string]any{}, 2000, true),
	})
	if p["accepted"].(float64) != 1 {
		t.Fatalf("tombstone should be accepted, got %v", p["accepted"])
	}
	items = pullItems(t, pullOK(t, h, tok, "settings", "", 100))
	if got := findItem(t, items, "theme"); !got["deleted"].(bool) {
		t.Fatalf("tombstone should overwrite, got %v", got)
	}
	// 重复 push 同一条：幂等跳过，条目仍唯一
	p = pushOK(t, h, tok, "settings", "", []map[string]any{
		item("theme", map[string]any{}, 2000, true),
	})
	if p["accepted"].(float64) != 0 {
		t.Fatalf("duplicate push should be idempotent, accepted=%v", p["accepted"])
	}
	items = pullItems(t, pullOK(t, h, tok, "settings", "", 100))
	if len(items) != 1 {
		t.Fatalf("want 1 entry, got %d", len(items))
	}
}

// TestMultiDeviceCross 多设备交叉：A push → B pull → B push 修改 → A 增量 pull 拿到。
func TestMultiDeviceCross(t *testing.T) {
	h, _, _ := setup(t)
	tokA, accountKey := registerDevice(t, h, "")
	tokB, _ := registerDevice(t, h, accountKey)

	pushOK(t, h, tokA, "mute", "", []map[string]any{
		item("tag:AI", map[string]any{"type": "tag", "value": "AI"}, 1000, false),
	})
	pullB := pullOK(t, h, tokB, "mute", "", 100)
	if len(pullItems(t, pullB)) != 1 {
		t.Fatal("B should see A's item")
	}
	tokenB := pullB["syncToken"].(string)

	pushOK(t, h, tokB, "mute", tokenB, []map[string]any{
		item("user:42", map[string]any{"type": "user", "value": "42"}, 2000, false),
	})
	// A 用旧 token 增量拉，只拿到 B 新增的
	pullA := pullOK(t, h, tokA, "mute", tokenB, 100)
	items := pullItems(t, pullA)
	if len(items) != 1 || items[0].(map[string]any)["key"] != "user:42" {
		t.Fatalf("A incremental pull want only user:42, got %v", items)
	}
}

// TestFullRequired baseToken 落后超 90 天 → 409；pull 无效/过旧 since → 409。
func TestFullRequired(t *testing.T) {
	h, _, _ := setup(t)
	tok, _ := registerDevice(t, h, "")
	pushOK(t, h, tok, "history", "", []map[string]any{
		item("1", map[string]any{"x": 1}, 1000, false),
	})

	old := fmt.Sprintf("st_%d_abcdef", time.Now().UnixMilli()-91*24*time.Hour.Milliseconds())
	rec := doJSON(t, h, http.MethodPost, "/sync/v1/push", map[string]any{
		"domain": "history", "baseToken": old,
		"items": []map[string]any{item("2", map[string]any{"x": 2}, 2000, false)},
	}, tok)
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale baseToken want 409, got %d: %s", rec.Code, rec.Body.String())
	}
	if code := decodeBody(t, rec)["error"].(map[string]any)["code"]; code != "SYNC_FULL_REQUIRED" {
		t.Fatalf("want SYNC_FULL_REQUIRED, got %v", code)
	}
	// baseToken 不一致但在保留期内：正常接受（LWW 容忍旧 baseToken）
	recent := fmt.Sprintf("st_%d_abcdef", time.Now().UnixMilli()-time.Hour.Milliseconds())
	pushOK(t, h, tok, "history", recent, []map[string]any{
		item("3", map[string]any{"x": 3}, 3000, false),
	})
	// pull：无效 since → 409
	rec = doJSON(t, h, http.MethodGet, "/sync/v1/pull?domain=history&since=garbage", nil, tok)
	if rec.Code != http.StatusConflict {
		t.Fatalf("invalid since want 409, got %d", rec.Code)
	}
	// pull：过旧 since → 409
	rec = doJSON(t, h, http.MethodGet, "/sync/v1/pull?domain=history&since="+old, nil, tok)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expired since want 409, got %d", rec.Code)
	}
}

// TestTombstone 墓碑随 pull 传播；超 90 天墓碑在 push 时被惰性清理。
func TestTombstone(t *testing.T) {
	h, database, _ := setup(t)
	tok, _ := registerDevice(t, h, "")

	pushOK(t, h, tok, "bookmark_snapshot", "", []map[string]any{
		item("777", map[string]any{"illustId": 777, "imageUrls": []string{"https://i.pximg.net/x.jpg"}}, 1000, false),
		item("888", map[string]any{"illustId": 888}, 1001, false),
	})
	pushOK(t, h, tok, "bookmark_snapshot", "", []map[string]any{
		item("888", map[string]any{}, 2000, true),
	})
	items := pullItems(t, pullOK(t, h, tok, "bookmark_snapshot", "", 100))
	if got := findItem(t, items, "888"); !got["deleted"].(bool) {
		t.Fatalf("tombstone should propagate, got %v", got)
	}

	// 直接操作 DB 造一条 91 天前的过期墓碑（不依赖真实时间流逝）
	var accountID int64
	if err := database.QueryRow("SELECT account_id FROM devices LIMIT 1").Scan(&accountID); err != nil {
		t.Fatalf("lookup account: %v", err)
	}
	expired := time.Now().UnixMilli() - 91*24*time.Hour.Milliseconds()
	if _, err := database.Exec(
		`INSERT INTO sync_entries (account_id, domain, "key", data, updated_at, deleted, seq)
		 VALUES (?, ?, 'old_key', '{}', ?, 1, 1)`,
		accountID, "bookmark_snapshot", expired); err != nil {
		t.Fatalf("insert expired tombstone: %v", err)
	}
	// 清理前可见
	items = pullItems(t, pullOK(t, h, tok, "bookmark_snapshot", "", 100))
	if findItem(t, items, "old_key") == nil {
		t.Fatal("expired tombstone should be visible before prune")
	}
	// push 触发惰性清理
	pushOK(t, h, tok, "bookmark_snapshot", "", []map[string]any{
		item("999", map[string]any{"illustId": 999}, 3000, false),
	})
	items = pullItems(t, pullOK(t, h, tok, "bookmark_snapshot", "", 100))
	if findItem(t, items, "old_key") != nil {
		t.Fatal("expired tombstone should be pruned after push")
	}
	if findItem(t, items, "888") == nil || findItem(t, items, "999") == nil {
		t.Fatalf("live entries should survive prune: %v", items)
	}
}

// TestValidation 批量上限 / 非法域 / 敏感字段 / 收藏快照结构。
func TestValidation(t *testing.T) {
	h, _, _ := setup(t)
	tok, _ := registerDevice(t, h, "")

	// 501 条超限
	big := make([]map[string]any, 501)
	for i := range big {
		big[i] = item(fmt.Sprintf("k%d", i), map[string]any{}, int64(i), false)
	}
	rec := doJSON(t, h, http.MethodPost, "/sync/v1/push",
		map[string]any{"domain": "history", "baseToken": "", "items": big}, tok)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("501 items want 400, got %d", rec.Code)
	}
	// 非法 domain
	rec = doJSON(t, h, http.MethodPost, "/sync/v1/push", map[string]any{
		"domain": "pixiv_tokens", "baseToken": "",
		"items": []map[string]any{item("k", map[string]any{}, 1, false)},
	}, tok)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid domain want 400, got %d", rec.Code)
	}
	// 敏感字段（大小写不敏感 + 嵌套）
	for _, data := range []map[string]any{
		{"pixivToken": "x"},
		{"outer": map[string]any{"ACCESS_TOKEN": "x"}},
	} {
		rec = doJSON(t, h, http.MethodPost, "/sync/v1/push", map[string]any{
			"domain": "settings", "baseToken": "",
			"items": []map[string]any{item("k", data, 1, false)},
		}, tok)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("sensitive field want 400, got %d (data=%v)", rec.Code, data)
		}
		if code := decodeBody(t, rec)["error"].(map[string]any)["code"]; code != "SENSITIVE_FIELD_REJECTED" {
			t.Fatalf("want SENSITIVE_FIELD_REJECTED, got %v", code)
		}
	}
	// bookmark_snapshot：缺 illustId → 400
	rec = doJSON(t, h, http.MethodPost, "/sync/v1/push", map[string]any{
		"domain": "bookmark_snapshot", "baseToken": "",
		"items": []map[string]any{item("1", map[string]any{"title": "x"}, 1, false)},
	}, tok)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("snapshot missing illustId want 400, got %d", rec.Code)
	}
	// imageUrls 元素非 string → 400
	rec = doJSON(t, h, http.MethodPost, "/sync/v1/push", map[string]any{
		"domain": "bookmark_snapshot", "baseToken": "",
		"items": []map[string]any{item("1", map[string]any{"illustId": 1, "imageUrls": []int{1, 2}}, 1, false)},
	}, tok)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("snapshot bad imageUrls want 400, got %d", rec.Code)
	}
	// 合法快照（imageUrls 可空数组）→ 200
	pushOK(t, h, tok, "bookmark_snapshot", "", []map[string]any{
		item("1", map[string]any{"illustId": 1, "imageUrls": []string{}}, 1, false),
	})
}

// TestTokenMonotonic 连续 push token 递增；并发 push 同域 token 仍严格单调。
func TestTokenMonotonic(t *testing.T) {
	h, _, _ := setup(t)
	tok, _ := registerDevice(t, h, "")

	p1 := pushOK(t, h, tok, "exif_config", "", []map[string]any{
		item("a", map[string]any{"x": 1}, 1000, false),
	})
	p2 := pushOK(t, h, tok, "exif_config", "", []map[string]any{
		item("b", map[string]any{"x": 2}, 1001, false),
	})
	ms1, err := ParseToken(p1["syncToken"].(string))
	if err != nil {
		t.Fatalf("parse token1: %v", err)
	}
	ms2, err := ParseToken(p2["syncToken"].(string))
	if err != nil {
		t.Fatalf("parse token2: %v", err)
	}
	if ms2 <= ms1 {
		t.Fatalf("tokens not increasing: %d then %d", ms1, ms2)
	}

	// 并发 push 同域：所有 token 数值部分互不相同（严格单调）
	const n = 8
	tokens := make([]string, n)
	var wg stdsync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := pushOK(t, h, tok, "exif_config", "", []map[string]any{
				item(fmt.Sprintf("c%d", i), map[string]any{"x": i}, int64(2000+i), false),
			})
			tokens[i] = p["syncToken"].(string)
		}(i)
	}
	wg.Wait()
	seen := make(map[int64]bool)
	for _, tk := range tokens {
		ms, err := ParseToken(tk)
		if err != nil {
			t.Fatalf("parse concurrent token: %v", err)
		}
		if seen[ms] {
			t.Fatalf("duplicate token watermark %d under concurrency: %v", ms, tokens)
		}
		seen[ms] = true
	}
}

// TestPullPagination 增量游标翻页：hasMore 时返回续页 token，回传续拉不丢不重。
func TestPullPagination(t *testing.T) {
	h, _, _ := setup(t)
	tok, _ := registerDevice(t, h, "")

	pushOK(t, h, tok, "search_history", "", []map[string]any{
		item("w1", map[string]any{"search_type": "illust"}, 1000, false),
		item("w2", map[string]any{"search_type": "illust"}, 1001, false),
		item("w3", map[string]any{"search_type": "user"}, 1002, false),
	})
	page1 := pullOK(t, h, tok, "search_history", "", 2)
	if !page1["hasMore"].(bool) {
		t.Fatal("page1 should have hasMore")
	}
	items1 := pullItems(t, page1)
	if len(items1) != 2 {
		t.Fatalf("page1 want 2 items, got %d", len(items1))
	}
	page2 := pullOK(t, h, tok, "search_history", page1["syncToken"].(string), 2)
	if page2["hasMore"].(bool) {
		t.Fatal("page2 should be last page")
	}
	items2 := pullItems(t, page2)
	if len(items2) != 1 || items2[0].(map[string]any)["key"] != "w3" {
		t.Fatalf("page2 want only w3, got %v", items2)
	}
	// 全集校验：两页合起来恰好 3 个不同 key
	got := map[string]bool{}
	for _, it := range append(items1, items2...) {
		got[it.(map[string]any)["key"].(string)] = true
	}
	if len(got) != 3 {
		t.Fatalf("paged pull lost/duplicated entries: %v", got)
	}
}

// TestAccountIsolation 另一账号 pull 不到数据。
func TestAccountIsolation(t *testing.T) {
	h, _, _ := setup(t)
	tokA, _ := registerDevice(t, h, "")
	tokC, _ := registerDevice(t, h, "") // 不传 accountKey → 新账号

	pushOK(t, h, tokA, "history", "", []map[string]any{
		item("1", map[string]any{"x": 1}, 1000, false),
	})
	pull := pullOK(t, h, tokC, "history", "", 100)
	if n := len(pullItems(t, pull)); n != 0 {
		t.Fatalf("other account should see nothing, got %d items", n)
	}
}

// TestUnauthorized 无鉴权一律 401。
func TestUnauthorized(t *testing.T) {
	h, _, _ := setup(t)
	rec := doJSON(t, h, http.MethodPost, "/sync/v1/push",
		map[string]any{"domain": "history", "baseToken": "", "items": []any{}}, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("push without auth want 401, got %d", rec.Code)
	}
	rec = doJSON(t, h, http.MethodGet, "/sync/v1/pull?domain=history", nil, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("pull without auth want 401, got %d", rec.Code)
	}
}
