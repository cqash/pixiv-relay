package recover

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
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
	"github.com/arkpix/relay/internal/crypto"
	"github.com/arkpix/relay/internal/db"
	"github.com/arkpix/relay/internal/img"
)

const (
	tokenA = "test-token-a"
	tokenB = "test-token-b"
)

// blockRewrite 测试出网 Transport：按请求 host 改写到对应 httptest 上游；
// 未登记的 host 直接报错，保证测试绝不触达真实外网。
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

// recorder mock 上游：命中计数、按路径计数、请求时间戳、并发水位观测。
type recorder struct {
	hits   atomic.Int64
	cur    atomic.Int64
	max    atomic.Int64
	delay  time.Duration
	serve  http.HandlerFunc
	mu     sync.Mutex
	times  []time.Time
	counts map[string]int
}

func (m *recorder) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m.hits.Add(1)
		m.mu.Lock()
		m.times = append(m.times, time.Now())
		m.counts[r.URL.Path]++
		m.mu.Unlock()
		cur := m.cur.Add(1)
		for {
			mx := m.max.Load()
			if cur <= mx || m.max.CompareAndSwap(mx, cur) {
				break
			}
		}
		defer m.cur.Add(-1)
		if m.delay > 0 {
			time.Sleep(m.delay)
		}
		m.serve(w, r)
	}
}

func (m *recorder) pathHits(path string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.counts[path]
}

// sortedTimes 返回全部请求时间戳（到达序即单调）。
func (m *recorder) sortedTimes() []time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]time.Time, len(m.times))
	copy(out, m.times)
	return out
}

type env struct {
	h      http.Handler
	db     *sql.DB
	mw     *auth.Middleware
	client *http.Client
	cache  *cache.DiskLRU
	mirror *recorder
	pximg  *recorder
}

// setup 构建测试环境：临时库 + 临时缓存/临时目录（t.TempDir，绝不碰 ./data）+
// mock 镜像源（i.pixiv.cat / i.pixiv.re → mirror）与 mock pximg（i.pximg.net → pximg）+
// recover 与 img 路由同 mux（验证包装 URL 真能取到图）。
func setup(t *testing.T, cfg Config, mirrorServe, pximgServe http.HandlerFunc, mirrorDelay time.Duration) *env {
	t.Helper()
	mirror := &recorder{serve: mirrorServe, delay: mirrorDelay, counts: map[string]int{}}
	mirrorSrv := httptest.NewServer(mirror.handler())
	t.Cleanup(mirrorSrv.Close)
	pximg := &recorder{serve: pximgServe, counts: map[string]int{}}
	pximgSrv := httptest.NewServer(pximg.handler())
	t.Cleanup(pximgSrv.Close)
	muURL, _ := url.Parse(mirrorSrv.URL)
	puURL, _ := url.Parse(pximgSrv.URL)
	client := &http.Client{Transport: &blockRewrite{targets: map[string]string{
		"i.pixiv.cat": muURL.Host,
		"i.pixiv.re":  muURL.Host,
		"i.pximg.net": puURL.Host,
	}}}

	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := db.Migrate(context.Background(), database); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	addAccount(t, database, tokenA)
	addAccount(t, database, tokenB)
	mw, err := auth.NewMiddleware(database, nil)
	if err != nil {
		t.Fatalf("new middleware: %v", err)
	}
	c, err := cache.Open(database, cache.Config{Dir: filepath.Join(t.TempDir(), "cache")})
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}

	if len(cfg.Sources) == 0 {
		cfg.Sources = []string{"snapshot", "pixiv_cat", "pixiv_re"}
	}
	if cfg.RatePerMin == 0 {
		cfg.RatePerMin = 100000 // 测试轮询密集，放大限流避免 429 干扰断言
	}
	cfg.TmpDir = filepath.Join(t.TempDir(), "recover-tmp")
	cfg.ImgExtraHosts = []string{"i.pixiv.cat", "i.pixiv.re"}
	svc, err := NewService(database, c, client, cfg)
	if err != nil {
		t.Fatalf("new recover service: %v", err)
	}

	imgSvc := img.NewService(client, c, cfg.ImgExtraHosts)
	mux := http.NewServeMux()
	RegisterRoutes(mux, svc, mw)
	img.RegisterRoutes(mux, imgSvc, mw)
	return &env{
		h: common.RequestID(mux), db: database, mw: mw, client: client,
		cache: c, mirror: mirror, pximg: pximg,
	}
}

// addAccount 直插账号+设备行，返回 account_id（access token = 明文 token，库内只存哈希）。
func addAccount(t *testing.T, database *sql.DB, token string) int64 {
	t.Helper()
	now := time.Now().UnixMilli()
	res, err := database.Exec("INSERT INTO accounts (account_key_hash, created_at) VALUES (?, ?)",
		auth.HashToken("ak-"+token), now)
	if err != nil {
		t.Fatalf("insert account: %v", err)
	}
	id, _ := res.LastInsertId()
	exp := now + int64(365*24*time.Hour/time.Millisecond)
	if _, err := database.Exec(
		`INSERT INTO devices (account_id, device_name, access_token_hash, refresh_token_hash, created_at, access_expires_at, refresh_expires_at)
		 VALUES (?, 'dev', ?, ?, ?, ?, ?)`,
		id, auth.HashToken(token), auth.HashToken(token+"-refresh"), now, exp, exp); err != nil {
		t.Fatalf("insert device: %v", err)
	}
	return id
}

// makeJPEG 生成合法 JPEG 字节（保证 > 1 KB 过内容校验）。
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

func doGet(t *testing.T, h http.Handler, token, pid string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/recover/v1/illust/"+pid, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// waitStatus 轮询直到响应码为 want（模拟客户端 §8.3 轮询），返回响应体。
func waitStatus(t *testing.T, h http.Handler, token, pid string, want int) map[string]any {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		rec := doGet(t, h, token, pid)
		if rec.Code == want {
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			return body
		}
		if time.Now().After(deadline) {
			t.Fatalf("wait status %d timeout, last = %d body %s", want, rec.Code, rec.Body.String())
		}
		time.Sleep(15 * time.Millisecond)
	}
}

func servePages(paths map[string][]byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, ok := paths[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(body)
	}
}

func serve404(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNotFound)
}

// TestStateMachine 202 → 后台抓取 → 轮询 200 ready；pages[].url 已包装
// /img/v1/fetch 形式，且用包装 URL 真能取到图（走 img 端点 HIT）。
func TestStateMachine(t *testing.T) {
	jpg := makeJPEG(t, 64, 48)
	e := setup(t, Config{ProbeEvery: time.Millisecond},
		servePages(map[string][]byte{
			"/424242.jpg":    jpg,
			"/424242_p1.jpg": jpg,
			"/424242_p2.jpg": jpg,
		}), serve404, 0)

	rec := doGet(t, e.h, tokenA, "424242")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("first status = %d, want 202, body %s", rec.Code, rec.Body.String())
	}
	var first map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &first)
	if first["status"] != "fetching" || first["retryAfterSec"] != float64(5) {
		t.Fatalf("202 body = %v", first)
	}

	body := waitStatus(t, e.h, tokenA, "424242", http.StatusOK)
	if body["status"] != "ready" || body["source"] != "pixiv_cat" {
		t.Fatalf("ready body = %v", body)
	}
	pages, ok := body["pages"].([]any)
	if !ok || len(pages) != 3 {
		t.Fatalf("pages = %v, want 3 pages", body["pages"])
	}
	p0 := pages[0].(map[string]any)
	rawURL, _ := p0["url"].(string)
	if !strings.Contains(rawURL, "/img/v1/fetch?url=") {
		t.Fatalf("page url not wrapped: %s", rawURL)
	}
	if p0["page"] != float64(0) || p0["width"] != float64(64) || p0["height"] != float64(48) {
		t.Fatalf("page0 = %v, want page 0 64x48", p0)
	}
	esc := rawURL[strings.Index(rawURL, "url=")+4:]
	orig, err := url.QueryUnescape(esc)
	if err != nil || orig != "https://i.pixiv.cat/424242.jpg" {
		t.Fatalf("wrapped url decodes to %q (err %v)", orig, err)
	}

	// 用包装 URL 走 img 端点取图：恢复时已落缓存，应 HIT 且不再触达镜像源。
	mirrorBefore := e.mirror.hits.Load()
	req := httptest.NewRequest(http.MethodGet, rawURL, nil)
	req.Header.Set("Authorization", "Bearer "+tokenA)
	rec2 := httptest.NewRecorder()
	e.h.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusOK || rec2.Header().Get("X-Cache") != "HIT" {
		t.Fatalf("img fetch status = %d X-Cache = %q", rec2.Code, rec2.Header().Get("X-Cache"))
	}
	if !bytes.Equal(rec2.Body.Bytes(), jpg) {
		t.Fatal("img fetch body mismatch with mirror bytes")
	}
	if got := e.mirror.hits.Load(); got != mirrorBefore {
		t.Fatalf("mirror hits changed on HIT: %d -> %d", mirrorBefore, got)
	}
}

// TestNegativeCacheAndRetry 全部源失败 → 404 not_found；负缓存期内再请求仍 404
// 且不再触发抓取；负缓存过期后允许重试。
func TestNegativeCacheAndRetry(t *testing.T) {
	e := setup(t, Config{ProbeEvery: time.Millisecond}, serve404, serve404, 0)

	doGet(t, e.h, tokenA, "313131")
	body := waitStatus(t, e.h, tokenA, "313131", http.StatusNotFound)
	if body["status"] != "not_found" {
		t.Fatalf("404 body = %v", body)
	}
	hits := e.mirror.hits.Load()

	// 负缓存期内：直接 404，不再触达镜像源。
	rec := doGet(t, e.h, tokenA, "313131")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cached status = %d, want 404", rec.Code)
	}
	if got := e.mirror.hits.Load(); got != hits {
		t.Fatalf("negative cache should not refetch: hits %d -> %d", hits, got)
	}

	// 负缓存过期 → 重新触发抓取。
	if _, err := e.db.Exec("UPDATE recover_cache SET expire = 0"); err != nil {
		t.Fatalf("expire row: %v", err)
	}
	rec = doGet(t, e.h, tokenA, "313131")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("after expire status = %d, want 202", rec.Code)
	}
	waitStatus(t, e.h, tokenA, "313131", http.StatusNotFound)
	if got := e.mirror.hits.Load(); got <= hits {
		t.Fatalf("expired negative cache should refetch: hits %d -> %d", hits, got)
	}
}

// TestSnapshotPriority 有存活快照 URL 时快照源优先：source=snapshot 且不触达镜像源。
func TestSnapshotPriority(t *testing.T) {
	jpg := makeJPEG(t, 64, 48)
	e := setup(t, Config{ProbeEvery: time.Millisecond},
		serve404, servePages(map[string][]byte{
			"/img-original/img/x/555_p0.jpg": jpg,
			"/img-original/img/x/555_p1.jpg": jpg,
		}), 0)

	var accountID int64
	if err := e.db.QueryRow("SELECT account_id FROM devices WHERE access_token_hash = ?",
		auth.HashToken(tokenA)).Scan(&accountID); err != nil {
		t.Fatalf("lookup account: %v", err)
	}
	data := `{"illustId":555,"title":"T","userName":"U","width":64,"height":48,` +
		`"imageUrls":["https://i.pximg.net/img-original/img/x/555_p0.jpg",` +
		`"https://i.pximg.net/img-original/img/x/555_p1.jpg"]}`
	if _, err := e.db.Exec(
		`INSERT INTO sync_entries (account_id, domain, "key", data, updated_at, deleted, seq)
		 VALUES (?, 'bookmark_snapshot', '555', ?, ?, 0, 1)`,
		accountID, data, time.Now().UnixMilli()); err != nil {
		t.Fatalf("insert snapshot: %v", err)
	}

	doGet(t, e.h, tokenA, "555")
	body := waitStatus(t, e.h, tokenA, "555", http.StatusOK)
	if body["source"] != "snapshot" {
		t.Fatalf("source = %v, want snapshot", body["source"])
	}
	if pages := body["pages"].([]any); len(pages) != 2 {
		t.Fatalf("pages = %v, want 2", pages)
	}
	meta := body["meta"].(map[string]any)
	if meta["title"] != "T" || meta["userName"] != "U" {
		t.Fatalf("meta = %v", meta)
	}
	if got := e.mirror.hits.Load(); got != 0 {
		t.Fatalf("mirror should not be touched, hits = %d", got)
	}
	if got := e.pximg.hits.Load(); got < 2 {
		t.Fatalf("pximg should be probed+downloaded, hits = %d", got)
	}
}

// TestProbeInterval 单源探测间隔 ≥ 注入的 ProbeEvery（礼貌策略，§8.2）。
func TestProbeInterval(t *testing.T) {
	jpg := makeJPEG(t, 64, 48)
	e := setup(t, Config{ProbeEvery: 50 * time.Millisecond},
		servePages(map[string][]byte{
			"/777.jpg":    jpg,
			"/777_p1.jpg": jpg,
			"/777_p2.jpg": jpg,
		}), serve404, 0)

	doGet(t, e.h, tokenA, "777")
	waitStatus(t, e.h, tokenA, "777", http.StatusOK)

	times := e.mirror.sortedTimes()
	// 777.jpg + _p1 + _p2 + 连续 2 个 404（_p3/_p4）= 5 次探测。
	if len(times) != 5 {
		t.Fatalf("mirror requests = %d, want 5", len(times))
	}
	for i := 1; i < len(times); i++ {
		if d := times[i].Sub(times[i-1]); d < 45*time.Millisecond {
			t.Fatalf("probe interval %v < 50ms between req %d/%d", d, i-1, i)
		}
	}
}

// TestGlobalConcurrent 全局抓取并发 ≤ 注入上限（每源并发放宽到 4 以隔离全局信号量效果）。
func TestGlobalConcurrent(t *testing.T) {
	jpg := makeJPEG(t, 64, 48)
	serve := func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".jpg") && !strings.Contains(r.URL.Path, "_p") {
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(jpg)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}
	e := setup(t, Config{
		ProbeEvery:          time.Millisecond,
		GlobalConcurrent:    2,
		SourceMaxConcurrent: 4,
	}, serve, serve404, 60*time.Millisecond)

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			pid := fmt.Sprintf("90000%d", i)
			doGet(t, e.h, tokenA, pid)
			waitStatus(t, e.h, tokenA, pid, http.StatusOK)
		}(i)
	}
	wg.Wait()
	if got := e.mirror.max.Load(); got > 2 {
		t.Fatalf("max upstream concurrency = %d, want <= 2 (global cap)", got)
	}
}

// TestInvalidPIDAndAuth pid 非纯数字 400；无鉴权 401。
func TestInvalidPIDAndAuth(t *testing.T) {
	e := setup(t, Config{}, serve404, serve404, 0)
	for _, pid := range []string{"abc", "12a3", "1.5", "0x1"} {
		rec := doGet(t, e.h, tokenA, pid)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("pid %q status = %d, want 400", pid, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "VALIDATION_FAILED") {
			t.Fatalf("pid %q body = %s", pid, rec.Body.String())
		}
	}
	rec := doGet(t, e.h, "", "12345")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no auth status = %d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "INVALID_TOKEN") {
		t.Fatalf("no auth body = %s", rec.Body.String())
	}
}

// TestAccountIsolation 恢复产物按账号隔离；RECOVER_SHARED=true 放开共享。
func TestAccountIsolation(t *testing.T) {
	jpg := makeJPEG(t, 64, 48)
	e := setup(t, Config{ProbeEvery: time.Millisecond},
		servePages(map[string][]byte{
			"/202020.jpg": jpg,
			"/202021.jpg": jpg,
		}), serve404, 0)

	doGet(t, e.h, tokenA, "202020")
	waitStatus(t, e.h, tokenA, "202020", http.StatusOK)

	// B 账号查不到 A 的恢复产物：按未命中处理（202 入自己的队）。
	rec := doGet(t, e.h, tokenB, "202020")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("account B status = %d, want 202 (isolated)", rec.Code)
	}
	waitStatus(t, e.h, tokenB, "202020", http.StatusOK) // 等 B 自己的抓取收尾，避免悬挂写

	// A 恢复另一作品；共享模式下 B 可直接命中 A 的产物。
	doGet(t, e.h, tokenA, "202021")
	waitStatus(t, e.h, tokenA, "202021", http.StatusOK)

	// RECOVER_SHARED=true：同库共享命中，B 直接 200。
	sharedSvc, err := NewService(e.db, e.cache, e.client, Config{
		Sources:       []string{"snapshot", "pixiv_cat", "pixiv_re"},
		TmpDir:        filepath.Join(t.TempDir(), "recover-tmp-shared"),
		ImgExtraHosts: []string{"i.pixiv.cat", "i.pixiv.re"},
		Shared:        true,
		ProbeEvery:    time.Millisecond,
		RatePerMin:    100000,
	})
	if err != nil {
		t.Fatalf("new shared service: %v", err)
	}
	mux := http.NewServeMux()
	RegisterRoutes(mux, sharedSvc, e.mw)
	h := common.RequestID(mux)
	rec = doGet(t, h, tokenB, "202021")
	if rec.Code != http.StatusOK {
		t.Fatalf("shared account B status = %d, want 200, body %s", rec.Code, rec.Body.String())
	}
}

// TestQueueDedup 同 (account, pid) 并发 3 个请求只入队一次（镜像源只被探测一轮）。
func TestQueueDedup(t *testing.T) {
	jpg := makeJPEG(t, 64, 48)
	e := setup(t, Config{ProbeEvery: time.Millisecond},
		servePages(map[string][]byte{"/646464.jpg": jpg}), serve404, 50*time.Millisecond)

	var wg sync.WaitGroup
	codes := make([]int, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = doGet(t, e.h, tokenA, "646464").Code
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusAccepted && c != http.StatusOK {
			t.Fatalf("req %d status = %d", i, c)
		}
	}
	waitStatus(t, e.h, tokenA, "646464", http.StatusOK)
	if got := e.mirror.pathHits("/646464.jpg"); got != 1 {
		t.Fatalf("mirror /646464.jpg hits = %d, want 1 (queue dedup)", got)
	}
}

// TestEncryptedAtRest 开启数据静态加密（M7，§9）：快照源的 sync_entries 密文可解密读取，
// 恢复产物 pages/meta 落库为 enc:v1: 密文，查询读出还原一致。
func TestEncryptedAtRest(t *testing.T) {
	jpg := makeJPEG(t, 64, 48)
	enc, err := crypto.Load("MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	if err != nil {
		t.Fatalf("load test key: %v", err)
	}
	e := setup(t, Config{ProbeEvery: time.Millisecond, Enc: enc},
		serve404, servePages(map[string][]byte{
			"/img-original/img/x/777_p0.jpg": jpg,
		}), 0)

	var accountID int64
	if err := e.db.QueryRow("SELECT account_id FROM devices WHERE access_token_hash = ?",
		auth.HashToken(tokenA)).Scan(&accountID); err != nil {
		t.Fatalf("lookup account: %v", err)
	}
	// 快照以密文形式入库（模拟 sync 加密开启后的写入形态）。
	data := `{"illustId":777,"title":"密","userName":"U","width":64,"height":48,` +
		`"imageUrls":["https://i.pximg.net/img-original/img/x/777_p0.jpg"]}`
	if _, err := e.db.Exec(
		`INSERT INTO sync_entries (account_id, domain, "key", data, updated_at, deleted, seq)
		 VALUES (?, 'bookmark_snapshot', '777', ?, ?, 0, 1)`,
		accountID, enc.Encrypt(data), time.Now().UnixMilli()); err != nil {
		t.Fatalf("insert encrypted snapshot: %v", err)
	}

	doGet(t, e.h, tokenA, "777")
	body := waitStatus(t, e.h, tokenA, "777", http.StatusOK)
	if body["source"] != "snapshot" {
		t.Fatalf("source = %v, want snapshot（密文快照应能解密读取）", body["source"])
	}
	if meta := body["meta"].(map[string]any); meta["title"] != "密" {
		t.Fatalf("meta = %v, want decrypted title", meta)
	}
	if pages := body["pages"].([]any); len(pages) != 1 {
		t.Fatalf("pages = %v, want 1", pages)
	}

	// 直接查库：pages/meta 必须是 enc:v1: 密文且不含明文。
	var pages, meta string
	if err := e.db.QueryRow(
		"SELECT pages, meta FROM recover_cache WHERE pid = '777'").Scan(&pages, &meta); err != nil {
		t.Fatalf("query raw recover_cache: %v", err)
	}
	for _, v := range []string{pages, meta} {
		if !strings.HasPrefix(v, crypto.Prefix) {
			t.Fatalf("recover_cache column must carry %q prefix, got %q", crypto.Prefix, v)
		}
	}
	if strings.Contains(pages, "777_p0") || strings.Contains(meta, "密") {
		t.Fatal("recover_cache ciphertext leaks plaintext")
	}
}
