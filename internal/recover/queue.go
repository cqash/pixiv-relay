// Package recover 实现 /recover 删除图恢复（设计文档 §8）：
// 查询状态机（ready/fetching/not_found）+ 异步抓取队列 + 正/负缓存。
// 恢复产物按 account_id 隔离（RECOVER_SHARED 可放开共享，§8.2 可见性分层）；
// 图片内容经 M4 磁盘缓存按 URL 哈希跨用户共享（公开图片，仅存储复用）。
package recover

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/arkpix/relay/internal/crypto"
	"github.com/arkpix/relay/internal/recover/sources"
	syncsvc "github.com/arkpix/relay/internal/sync"
)

const (
	// DefaultGlobalConcurrent 全局抓取并发上限（§8.2：≤ 8）。
	DefaultGlobalConcurrent = 8
	// fetchingGuardMs fetching 占位行的守卫窗口：worker 崩溃残留时
	// 过期后允许重新入队，不会永久卡在 fetching。
	fetchingGuardMs = int64(15 * time.Minute / time.Millisecond)
	// jobTimeout 单任务抓取总超时（多源轮询 + 限速探测的宽松上限）。
	jobTimeout = 5 * time.Minute
)

// Queue 异步抓取队列：goroutine 池（带缓冲 channel 信号量控全局并发）+
// 同 (account, pid) 在途去重。
type Queue struct {
	db    *sql.DB
	srcs  []sources.Source
	sem   chan struct{}
	ttlMs int64
	negMs int64
	enc   *crypto.Cipher // 静态加密（§9）；nil = 不加密
	now   func() int64

	mu       sync.Mutex
	inflight map[string]struct{}
}

func newQueue(db *sql.DB, srcs []sources.Source, globalConcurrent int, ttlMs, negMs int64, enc *crypto.Cipher) *Queue {
	if globalConcurrent <= 0 {
		globalConcurrent = DefaultGlobalConcurrent
	}
	return &Queue{
		db:       db,
		srcs:     srcs,
		sem:      make(chan struct{}, globalConcurrent),
		ttlMs:    ttlMs,
		negMs:    negMs,
		enc:      enc,
		now:      func() int64 { return time.Now().UnixMilli() },
		inflight: make(map[string]struct{}),
	}
}

// Enqueue 入队抓取任务并写 fetching 占位行；同 (account, pid) 在途只入队一次。
func (q *Queue) Enqueue(accountID int64, pid string) {
	key := strconv.FormatInt(accountID, 10) + "/" + pid
	q.mu.Lock()
	if _, ok := q.inflight[key]; ok {
		q.mu.Unlock()
		return
	}
	q.inflight[key] = struct{}{}
	q.mu.Unlock()

	if _, err := q.db.Exec(
		`INSERT INTO recover_cache (account_id, pid, pages, source, meta, status, expire)
		 VALUES (?, ?, '[]', '', '{}', 'fetching', ?)
		 ON CONFLICT(account_id, pid) DO UPDATE SET status = 'fetching', expire = excluded.expire`,
		accountID, pid, q.now()+fetchingGuardMs); err != nil {
		slog.Error("recover mark fetching failed", "err", err)
	}
	go q.work(accountID, pid, key)
}

// work 按优先级尝试数据源（§8.2）：成功 → ready（正缓存 TTL）；全部失败 →
// not_found（负缓存）。源内部已负责限速/并发/落盘。
func (q *Queue) work(accountID int64, pid, key string) {
	defer func() {
		q.mu.Lock()
		delete(q.inflight, key)
		q.mu.Unlock()
	}()
	q.sem <- struct{}{}
	defer func() { <-q.sem }()

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), jobTimeout)
	defer cancel()

	snap := q.loadSnapshot(ctx, accountID, pid)
	for _, src := range q.srcs {
		pages, err := src.Fetch(ctx, pid, snap)
		if err != nil {
			continue
		}
		if err := q.writeResult(accountID, pid, pages, src.Name(), snapshotMeta(snap), q.now()+q.ttlMs); err != nil {
			slog.Error("recover write ready failed", "err", err)
		}
		slog.Info("recover ready",
			"pid", pid, "source", src.Name(), "pages", len(pages),
			"durMs", time.Since(start).Milliseconds())
		return
	}
	if err := q.writeStatus(accountID, pid, "not_found", q.now()+q.negMs); err != nil {
		slog.Error("recover write not_found failed", "err", err)
	}
	slog.Info("recover not_found", "pid", pid, "durMs", time.Since(start).Milliseconds())
}

// loadSnapshot 从本账号 bookmark_snapshot 同步域读该 pid 的快照（§7.3/§8.2）。
// 无快照或数据损坏返回 nil（快照源将直接判负）。
func (q *Queue) loadSnapshot(ctx context.Context, accountID int64, pid string) *sources.Snapshot {
	var data string
	err := q.db.QueryRowContext(ctx,
		`SELECT data FROM sync_entries
		 WHERE account_id = ? AND domain = ? AND "key" = ? AND deleted = 0`,
		accountID, syncsvc.DomainBookmarkSnapshot, pid).Scan(&data)
	if err != nil {
		return nil
	}
	// 静态加密（§9）：密文按前缀解密，存量明文原样；解密失败按无快照处理。
	if data, err = q.enc.Decrypt(data); err != nil {
		slog.Warn("recover snapshot decrypt failed", "err", err)
		return nil
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(data), &obj); err != nil {
		return nil
	}
	snap := &sources.Snapshot{Meta: make(map[string]any, len(obj))}
	for k, v := range obj {
		switch k {
		case "imageUrls":
			if arr, ok := v.([]any); ok {
				for _, e := range arr {
					if s, ok := e.(string); ok {
						snap.ImageURLs = append(snap.ImageURLs, s)
					}
				}
			}
		case "width":
			snap.Width = jsonInt(v)
		case "height":
			snap.Height = jsonInt(v)
		default:
			snap.Meta[k] = v
		}
	}
	return snap
}

// snapshotMeta 提取快照元数据（title/userName 等）随恢复结果透传（§8.1 meta）。
func snapshotMeta(snap *sources.Snapshot) map[string]any {
	if snap == nil {
		return map[string]any{}
	}
	return snap.Meta
}

func jsonInt(v any) int {
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return 0
}

// writeResult 写 ready 行（正缓存）：pages/meta 序列化为 JSON 文本落库。
func (q *Queue) writeResult(accountID int64, pid string, pages []sources.Page, source string, meta map[string]any, expire int64) error {
	pagesJSON, err := json.Marshal(pages)
	if err != nil {
		return err
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	_, err = q.db.Exec(
		`INSERT INTO recover_cache (account_id, pid, pages, source, meta, status, expire)
		 VALUES (?, ?, ?, ?, ?, 'ready', ?)
		 ON CONFLICT(account_id, pid) DO UPDATE SET
		   pages = excluded.pages, source = excluded.source, meta = excluded.meta,
		   status = 'ready', expire = excluded.expire`,
		// 静态加密（§9）：pages/meta 同属用户数据，落库前加密。
		accountID, pid, q.enc.Encrypt(string(pagesJSON)), source, q.enc.Encrypt(string(metaJSON)), expire)
	return err
}

// writeStatus 写终态行（not_found 负缓存）。
func (q *Queue) writeStatus(accountID int64, pid, status string, expire int64) error {
	_, err := q.db.Exec(
		`INSERT INTO recover_cache (account_id, pid, pages, source, meta, status, expire)
		 VALUES (?, ?, '[]', '', '{}', ?, ?)
		 ON CONFLICT(account_id, pid) DO UPDATE SET
		   pages = '[]', source = '', meta = '{}', status = excluded.status, expire = excluded.expire`,
		accountID, pid, status, expire)
	return err
}
