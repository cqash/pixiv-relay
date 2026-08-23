package admin

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"

	"github.com/cqash/pixiv-relay/internal/cache"
	"github.com/cqash/pixiv-relay/internal/common"
	"github.com/cqash/pixiv-relay/internal/recover"
)

// ServerVersion 管理端上报的服务端版本（§14.3，与 /auth register 一致）。
const ServerVersion = "1.1.0"

// Service 管理端服务：概览统计、设置热更新、缓存与账号/设备管理。
type Service struct {
	db           *sql.DB
	cache        *cache.DiskLRU
	recoverSvc   *recover.Service
	writeLimiter *common.Limiter
	imgLimiter   *common.Limiter
	env          EnvSnapshot
	startedAt    time.Time
}

// NewService 创建管理端服务：启动时从 DB 读取覆盖项并立即应用热更挂钩
// （§14.2：DB > env > 默认）。
func NewService(db *sql.DB, c *cache.DiskLRU, rec *recover.Service,
	writeLimiter, imgLimiter *common.Limiter, env EnvSnapshot, startedAt time.Time) *Service {
	s := &Service{
		db:           db,
		cache:        c,
		recoverSvc:   rec,
		writeLimiter: writeLimiter,
		imgLimiter:   imgLimiter,
		env:          env,
		startedAt:    startedAt,
	}
	s.applyAll(context.Background())
	return s
}

// Overview GET /admin/v1/overview：版本、运行时长、账号/设备计数、
// 缓存用量、恢复缓存按状态计数、生效设置。
func (s *Service) Overview(ctx context.Context) (map[string]any, error) {
	var accounts, devices int64
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM accounts").Scan(&accounts); err != nil {
		return nil, fmt.Errorf("count accounts: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM devices").Scan(&devices); err != nil {
		return nil, fmt.Errorf("count devices: %w", err)
	}
	cacheBytes, cacheEntries, err := s.cache.Stats(ctx)
	if err != nil {
		return nil, err
	}
	recoverByStatus := map[string]int64{}
	rows, err := s.db.QueryContext(ctx, "SELECT status, COUNT(*) FROM recover_cache GROUP BY status")
	if err != nil {
		return nil, fmt.Errorf("count recover_cache: %w", err)
	}
	for rows.Next() {
		var status string
		var n int64
		if err := rows.Scan(&status, &n); err == nil {
			recoverByStatus[status] = n
		}
	}
	_ = rows.Close()

	return map[string]any{
		"serverVersion": ServerVersion,
		"uptimeSec":     int64(time.Since(s.startedAt).Seconds()),
		"accounts":      accounts,
		"devices":       devices,
		"cache":         map[string]any{"bytes": cacheBytes, "entries": cacheEntries},
		"recoverCache":  recoverByStatus,
		"settings":      s.resolveAll(ctx),
	}, nil
}

// CacheStats GET /admin/v1/cache/stats：用量 + 生效上限/水位 + 布局/目录。
func (s *Service) CacheStats(ctx context.Context) (map[string]any, error) {
	bytes, entries, err := s.cache.Stats(ctx)
	if err != nil {
		return nil, err
	}
	cfg := s.cache.Config()
	return map[string]any{
		"bytes":         bytes,
		"entries":       entries,
		"maxBytes":      cfg.MaxBytes,
		"highWatermark": cfg.HighWatermark,
		"layout":        cfg.Layout,
		"dir":           cfg.Dir,
	}, nil
}

// EvictCache POST /admin/v1/cache/evict：阻塞式按水位淘汰，返回释放量。
func (s *Service) EvictCache(ctx context.Context) (map[string]any, error) {
	beforeBytes, beforeEntries, err := s.cache.Stats(ctx)
	if err != nil {
		return nil, err
	}
	s.cache.Evict(ctx)
	afterBytes, afterEntries, err := s.cache.Stats(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"freedBytes":   beforeBytes - afterBytes,
		"freedEntries": beforeEntries - afterEntries,
		"bytes":        afterBytes,
		"entries":      afterEntries,
	}, nil
}

// AccountItem 账号列表项（只暴露元信息，不含任何凭据材料，§14.3）。
type AccountItem struct {
	ID             int64 `json:"id"`
	CreatedAt      int64 `json:"createdAt"`
	DeviceCount    int64 `json:"deviceCount"`
	SyncEntryCount int64 `json:"syncEntryCount"`
}

// ListAccounts GET /admin/v1/accounts：created_at,id 升序游标分页（§4.3），
// LEFT JOIN 子查询计数设备数与同步条目数。
func (s *Service) ListAccounts(ctx context.Context, cursor string, limit int) ([]AccountItem, string, error) {
	var where string
	var args []any
	if cursor != "" {
		ts, idStr, err := common.DecodeCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			return nil, "", common.BadRequest("invalid cursor")
		}
		where = "WHERE a.created_at > ? OR (a.created_at = ? AND a.id > ?)"
		args = append(args, ts, ts, id)
	}
	args = append(args, limit+1) // 多取一行判定是否还有下一页
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.id, a.created_at,
		       COALESCE(d.cnt, 0) AS device_count,
		       COALESCE(e.cnt, 0) AS sync_entry_count
		FROM accounts a
		LEFT JOIN (SELECT account_id, COUNT(*) AS cnt FROM devices GROUP BY account_id) d
		       ON d.account_id = a.id
		LEFT JOIN (SELECT account_id, COUNT(*) AS cnt FROM sync_entries GROUP BY account_id) e
		       ON e.account_id = a.id
		`+where+`
		ORDER BY a.created_at, a.id
		LIMIT ?`, args...)
	if err != nil {
		return nil, "", fmt.Errorf("list accounts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	items := make([]AccountItem, 0, limit)
	for rows.Next() {
		var it AccountItem
		if err := rows.Scan(&it.ID, &it.CreatedAt, &it.DeviceCount, &it.SyncEntryCount); err != nil {
			return nil, "", fmt.Errorf("scan account: %w", err)
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("list accounts: %w", err)
	}

	next := ""
	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1]
		next = common.EncodeCursor(last.CreatedAt, strconv.FormatInt(last.ID, 10))
	}
	return items, next, nil
}

// DeviceItem 设备列表项（§14.3：设备名、注册与过期时间，毫秒）。
type DeviceItem struct {
	ID               int64  `json:"id"`
	DeviceName       string `json:"deviceName"`
	CreatedAt        int64  `json:"createdAt"`
	AccessExpiresAt  int64  `json:"accessExpiresAt"`
	RefreshExpiresAt int64  `json:"refreshExpiresAt"`
}

// ListDevices GET /admin/v1/accounts/{id}/devices。
func (s *Service) ListDevices(ctx context.Context, accountID int64) ([]DeviceItem, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, device_name, created_at, access_expires_at, refresh_expires_at
		FROM devices WHERE account_id = ? ORDER BY id`, accountID)
	if err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}
	defer func() { _ = rows.Close() }()
	items := make([]DeviceItem, 0)
	for rows.Next() {
		var it DeviceItem
		if err := rows.Scan(&it.ID, &it.DeviceName, &it.CreatedAt, &it.AccessExpiresAt, &it.RefreshExpiresAt); err != nil {
			return nil, fmt.Errorf("scan device: %w", err)
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

// DeleteDevice DELETE /admin/v1/devices/{id}：删行即吊销（token 立即失效）；不存在 404。
func (s *Service) DeleteDevice(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM devices WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete device: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return common.NotFound("device not found")
	}
	return nil
}

// DeleteAccount DELETE /admin/v1/accounts/{id}：事务内显式级联删除
// devices / sync_entries / sync_domains / recover_cache / accounts
// （FK 无 ON DELETE CASCADE，§14.3）；不存在 404。
func (s *Service) DeleteAccount(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin delete account tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		"DELETE FROM devices WHERE account_id = ?",
		"DELETE FROM sync_entries WHERE account_id = ?",
		"DELETE FROM sync_domains WHERE account_id = ?",
		"DELETE FROM recover_cache WHERE account_id = ?",
	} {
		if _, err := tx.ExecContext(ctx, stmt, id); err != nil {
			return fmt.Errorf("cascade delete: %w", err)
		}
	}
	res, err := tx.ExecContext(ctx, "DELETE FROM accounts WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete account: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return common.NotFound("account not found")
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete account tx: %w", err)
	}
	return nil
}
