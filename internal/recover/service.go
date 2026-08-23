package recover

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/arkpix/relay/internal/auth"
	"github.com/arkpix/relay/internal/cache"
	"github.com/arkpix/relay/internal/common"
	"github.com/arkpix/relay/internal/crypto"
	"github.com/arkpix/relay/internal/recover/sources"
)

// 正/负缓存 TTL 默认值（§8.2：正缓存 90 天、负缓存 7 天）。
const (
	defaultTTLDays    = 90
	defaultNegTTLDays = 7
	retryAfterSec     = 5
	defaultRatePerMin = 60.0
	defaultRateBurst  = 10
	dayMs             = int64(24 * time.Hour / time.Millisecond)
)

// Config 恢复服务配置。TmpDir 必填（config 包默认 <DATA_DIR>/recover-tmp）；
// ProbeEvery / SourceMaxConcurrent / GlobalConcurrent / RatePerMin 零值取默认，
// 测试可注入小参数避免真实等待。
type Config struct {
	Sources         []string // RECOVER_SOURCES 优先级列表（snapshot,pixiv_cat,pixiv_re）
	TTLDays         int      // 正缓存天数（默认 90）
	NegativeTTLDays int      // 负缓存天数（默认 7）
	Shared          bool     // RECOVER_SHARED：放开跨账号共享（§8.2）
	TmpDir          string   // 抓取临时目录（独立于缓存目录，HDD 策略）
	ImgExtraHosts   []string // /img 白名单追加项（镜像源域名启动校验，§6.2）

	ProbeEvery          time.Duration // 单源探测间隔（默认 1s）
	SourceMaxConcurrent int           // 每源并发上限（默认 2）
	GlobalConcurrent    int           // 全局抓取并发上限（默认 8）
	RatePerMin          float64       // 端点限流（默认 60/min，burst 10）

	// Limiter 共享写端点限流器（app 层统一持有，供管理端 §14.2 热调速率）；
	// nil 时按 RatePerMin 自建（保持既有测试调用兼容）。
	Limiter *common.Limiter

	// Enc 用户数据静态加密器（§9，与 sync 共用 DATA_ENC_KEY；nil = 不加密）。
	// recover_cache 的 pages/meta 同属用户数据，与存量明文按前缀混存兼容。
	Enc *crypto.Cipher
}

// Service 恢复服务：查询状态机 + 异步队列。
type Service struct {
	db      *sql.DB
	queue   *Queue
	shared  bool
	enc     *crypto.Cipher
	limiter *common.Limiter
	now     func() int64
}

// NewService 创建恢复服务：建临时目录、按优先级组装数据源、
// 校验镜像源域名已在 IMG_EXTRA_HOSTS 声明（否则客户端走 /img/v1/fetch 取图会 403）。
func NewService(db *sql.DB, c *cache.DiskLRU, client *http.Client, cfg Config) (*Service, error) {
	if cfg.TmpDir == "" {
		return nil, errors.New("recover: TmpDir is required")
	}
	if cfg.TTLDays <= 0 {
		cfg.TTLDays = defaultTTLDays
	}
	if cfg.NegativeTTLDays <= 0 {
		cfg.NegativeTTLDays = defaultNegTTLDays
	}
	if cfg.RatePerMin <= 0 {
		cfg.RatePerMin = defaultRatePerMin
	}
	if err := os.MkdirAll(cfg.TmpDir, 0o755); err != nil {
		return nil, fmt.Errorf("recover: mkdir tmp: %w", err)
	}

	srcs, mirrorHosts := sources.Build(cfg.Sources, sources.Deps{
		Client:        client,
		Cache:         c,
		TmpDir:        cfg.TmpDir,
		ProbeEvery:    cfg.ProbeEvery,
		MaxConcurrent: cfg.SourceMaxConcurrent,
	})
	allowed := make(map[string]bool, len(cfg.ImgExtraHosts))
	for _, h := range cfg.ImgExtraHosts {
		allowed[strings.ToLower(h)] = true
	}
	for _, h := range mirrorHosts {
		if !allowed[strings.ToLower(h)] {
			slog.Warn("recover mirror host not in IMG_EXTRA_HOSTS; /img/v1/fetch will 403", "host", h)
		}
	}

	limiter := cfg.Limiter
	if limiter == nil {
		limiter = common.NewLimiter(cfg.RatePerMin, defaultRateBurst)
	}
	return &Service{
		db:      db,
		queue:   newQueue(db, srcs, cfg.GlobalConcurrent, int64(cfg.TTLDays)*dayMs, int64(cfg.NegativeTTLDays)*dayMs, cfg.Enc),
		shared:  cfg.Shared,
		enc:     cfg.Enc,
		limiter: limiter,
		now:     func() int64 { return time.Now().UnixMilli() },
	}, nil
}

// SetTTLDays 热更新正/负缓存 TTL 天数（管理端 §14.2），仅影响新写入的缓存行。
func (s *Service) SetTTLDays(ttlDays, negativeTTLDays int) {
	s.queue.SetTTLs(int64(ttlDays)*dayMs, int64(negativeTTLDays)*dayMs)
}

// TTLDays 读回当前生效的正/负缓存 TTL 天数。
func (s *Service) TTLDays() (ttlDays, negativeTTLDays int) {
	ttlMs, negMs := s.queue.TTLs()
	return int(ttlMs / dayMs), int(negMs / dayMs)
}

// cacheRow recover_cache 命中行（仅 ready / not_found 且未过期）。
type cacheRow struct {
	status string
	pages  []sources.Page
	source string
	meta   map[string]any
}

// Query 处理 GET /recover/v1/illust/{pid}（§8.1 状态机）：
// ready 未过期 → 200；not_found 负缓存未过期 → 404 {status:"not_found"}；
// 无记录/过期/fetching → 幂等入队 → 202 {status:"fetching",retryAfterSec:5}。
func (s *Service) Query(w http.ResponseWriter, r *http.Request) {
	accountID, _ := auth.AccountIDFrom(r.Context())
	pid := r.PathValue("pid")
	if !isNumeric(pid) {
		common.WriteError(w, r, common.BadRequest("pid must be numeric"))
		return
	}

	row, err := s.lookup(r.Context(), accountID, pid)
	if err != nil {
		common.WriteError(w, r, err)
		return
	}
	if row != nil && row.status == "not_found" {
		// 负缓存：404 + {status:"not_found"}（§8.1），期内不再触发抓取。
		common.WriteJSON(w, r, http.StatusNotFound, map[string]any{"status": "not_found"})
		return
	}
	if row != nil { // ready
		base := requestBase(r)
		pages := make([]sources.Page, len(row.pages))
		for i, p := range row.pages {
			p.URL = base + "/img/v1/fetch?url=" + url.QueryEscape(p.URL)
			pages[i] = p
		}
		common.WriteJSON(w, r, http.StatusOK, map[string]any{
			"status": "ready",
			"pages":  pages,
			"source": row.source,
			"meta":   row.meta,
		})
		return
	}

	// 未命中（无记录/过期/fetching 残留）：幂等入队，在途去重由队列保证。
	s.queue.Enqueue(accountID, pid)
	common.WriteJSON(w, r, http.StatusAccepted, map[string]any{
		"status":        "fetching",
		"retryAfterSec": retryAfterSec,
	})
}

// lookup 查恢复缓存：只认 ready / not_found 且未过期的行（fetching 占位行
// 不参与命中，统一走幂等入队路径）。Shared 模式跨账号共享命中（ready 优先，§8.2）。
func (s *Service) lookup(ctx context.Context, accountID int64, pid string) (*cacheRow, error) {
	var row *sql.Row
	if s.shared {
		row = s.db.QueryRowContext(ctx,
			`SELECT pages, source, meta, status FROM recover_cache
			 WHERE pid = ? AND expire > ? AND status IN ('ready', 'not_found')
			 ORDER BY CASE status WHEN 'ready' THEN 0 ELSE 1 END, expire DESC LIMIT 1`,
			pid, s.now())
	} else {
		row = s.db.QueryRowContext(ctx,
			`SELECT pages, source, meta, status FROM recover_cache
			 WHERE account_id = ? AND pid = ? AND expire > ? AND status IN ('ready', 'not_found')`,
			accountID, pid, s.now())
	}
	var pagesJSON, metaJSON string
	out := &cacheRow{}
	err := row.Scan(&pagesJSON, &out.source, &metaJSON, &out.status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("recover: lookup: %w", err)
	}
	// 静态加密（§9）：pages/meta 密文按 enc:v1: 前缀解密，存量明文原样。
	pagesJSON, err = s.enc.Decrypt(pagesJSON)
	if err != nil {
		return nil, fmt.Errorf("recover: decrypt pages: %w", err)
	}
	if metaJSON, err = s.enc.Decrypt(metaJSON); err != nil {
		return nil, fmt.Errorf("recover: decrypt meta: %w", err)
	}
	if err := json.Unmarshal([]byte(pagesJSON), &out.pages); err != nil {
		return nil, fmt.Errorf("recover: parse pages: %w", err)
	}
	out.meta = map[string]any{}
	_ = json.Unmarshal([]byte(metaJSON), &out.meta) // 损坏时降级为空对象
	return out, nil
}

// requestBase 从请求推导公共基址（r.Host + scheme），用于包装 /img/v1/fetch URL。
func requestBase(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// isNumeric 校验 pid 为纯数字（防目录穿越/注入）。
func isNumeric(pid string) bool {
	if pid == "" {
		return false
	}
	for _, c := range pid {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
