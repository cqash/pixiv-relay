// Package cache 实现磁盘 LRU 图片缓存（设计文档 §6.2 / §6.4，HDD 友好硬约束）：
//   - 键 = URL 的 SHA-256 十六进制；sharded（默认）按哈希前两字节分两级目录
//     <dir>/ab/cd/<hash>，flat 直接 <dir>/<hash>，避免单目录数万文件；
//   - 写 = 同卷临时目录流式下载 → 校验大小 → os.Rename 原子落盘，
//     全程不整读入内存；临时目录必须与缓存目录同卷，跨卷 rename 退化为 copy；
//   - 读 = os.Open + 调用方 io.Copy 流式直出，命中路径不做额外 stat；
//   - 元数据（size/atime/content_type）存 DB cache_meta 表（迁移 0002），
//     LRU 淘汰按 DB 内 atime 升序，不依赖文件系统 atime；
//   - 淘汰：启动时 + 写后总量超水位（HighWatermark × MaxBytes）触发，
//     按批（EvictionBatch）删除、批间隔 100ms，避免瞬时随机删除打满 HDD 队列。
package cache

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// 默认值（对齐 §6.4 与 PLAN.md §7）。
const (
	DefaultMaxBytes      int64 = 20 << 30 // 20 GB
	DefaultMaxFileBytes  int64 = 50 << 20 // 单图 50 MB（§6.2）
	DefaultHighWatermark       = 0.9
	DefaultEvictionBatch       = 500
	evictBatchInterval         = 100 * time.Millisecond
)

// Config 缓存配置。零值字段取默认值（便于测试只设关键项）。
type Config struct {
	Dir           string  // 缓存根目录（必填）
	TmpDir        string  // 临时目录（默认 <Dir>/tmp，必须与 Dir 同卷）
	MaxBytes      int64   // 总容量上限（默认 20 GB）
	MaxFileBytes  int64   // 单文件上限（默认 50 MB，超限不入缓存降级直连）
	Layout        string  // sharded（默认）| flat
	HighWatermark float64 // 淘汰触发水位（默认 0.9 × MaxBytes）
	EvictionBatch int     // 每批淘汰文件数（默认 500），批间隔 100ms
}

// Meta 缓存条目元数据（对应 cache_meta 行）。
type Meta struct {
	Size        int64
	Atime       int64
	CreatedAt   int64
	ContentType string
}

// DiskLRU 磁盘 LRU 缓存。读写与淘汰并发安全；淘汰由 evictMu 串行化。
type DiskLRU struct {
	cfg   Config
	db    *sql.DB
	total atomic.Int64 // DB 内缓存总字节数（启动时汇总，Put/Evict 增量维护）

	evictMu sync.Mutex // TryLock 串行化淘汰，避免并发淘汰互相踩踏
}

// Key 计算缓存键：URL 的 SHA-256 十六进制（§6.2）。
func Key(rawURL string) string {
	sum := sha256.Sum256([]byte(rawURL))
	return hex.EncodeToString(sum[:])
}

// Open 打开缓存：建目录、汇总总量、启动时按水位淘汰一次。
func Open(db *sql.DB, cfg Config) (*DiskLRU, error) {
	if cfg.Dir == "" {
		return nil, errors.New("cache: Dir is required")
	}
	if cfg.TmpDir == "" {
		cfg.TmpDir = filepath.Join(cfg.Dir, "tmp")
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = DefaultMaxBytes
	}
	if cfg.MaxFileBytes <= 0 {
		cfg.MaxFileBytes = DefaultMaxFileBytes
	}
	if cfg.Layout == "" {
		cfg.Layout = "sharded"
	}
	if cfg.Layout != "sharded" && cfg.Layout != "flat" {
		return nil, fmt.Errorf("cache: invalid layout %q", cfg.Layout)
	}
	if cfg.HighWatermark <= 0 || cfg.HighWatermark > 1 {
		cfg.HighWatermark = DefaultHighWatermark
	}
	if cfg.EvictionBatch <= 0 {
		cfg.EvictionBatch = DefaultEvictionBatch
	}
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("cache: mkdir %s: %w", cfg.Dir, err)
	}
	if err := os.MkdirAll(cfg.TmpDir, 0o755); err != nil {
		return nil, fmt.Errorf("cache: mkdir %s: %w", cfg.TmpDir, err)
	}

	c := &DiskLRU{cfg: cfg, db: db}
	var total int64
	if err := db.QueryRow("SELECT COALESCE(SUM(size), 0) FROM cache_meta").Scan(&total); err != nil {
		return nil, fmt.Errorf("cache: sum cache size: %w", err)
	}
	c.total.Store(total)
	// 启动时淘汰一次（上次运行超限 / 配置调低水位兜底）。
	c.Evict(context.Background())
	return c, nil
}

// Config 返回规范化后的配置（img 服务需要 MaxFileBytes 做直连判定）。
func (c *DiskLRU) Config() Config { return c.cfg }

// pathFor 计算键对应的落盘路径（sharded：前两字节两级子目录）。
func (c *DiskLRU) pathFor(key string) string {
	if c.cfg.Layout == "flat" {
		return filepath.Join(c.cfg.Dir, key)
	}
	return filepath.Join(c.cfg.Dir, key[:2], key[2:4], key)
}

// Get 读取缓存：命中返回已打开的文件（调用方负责 Close）与元数据，
// 未命中返回 (nil, Meta{}, nil)。命中会刷新 DB 内 atime（LRU 依据）。
// 文件缺失但元数据残留（漂移）时清理元数据并按未命中处理。
func (c *DiskLRU) Get(ctx context.Context, key string) (*os.File, Meta, error) {
	var m Meta
	err := c.db.QueryRowContext(ctx,
		"SELECT size, atime, created_at, content_type FROM cache_meta WHERE key = ?", key).
		Scan(&m.Size, &m.Atime, &m.CreatedAt, &m.ContentType)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, Meta{}, nil
	}
	if err != nil {
		return nil, Meta{}, fmt.Errorf("cache: get meta: %w", err)
	}
	f, err := os.Open(c.pathFor(key))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// 元数据残留：清掉，按未命中走。
			if _, derr := c.db.ExecContext(ctx, "DELETE FROM cache_meta WHERE key = ?", key); derr == nil {
				c.total.Add(-m.Size)
			}
			return nil, Meta{}, nil
		}
		return nil, Meta{}, fmt.Errorf("cache: open file: %w", err)
	}
	// 刷新 atime（小写入，WAL 下代价低；命中路径不 stat 文件）。
	if _, err := c.db.ExecContext(ctx,
		"UPDATE cache_meta SET atime = ? WHERE key = ?", time.Now().UnixMilli(), key); err != nil {
		slog.WarnContext(ctx, "cache touch atime failed")
	}
	return f, m, nil
}

// Writer 流式缓存写入器：数据写入同卷临时文件，Commit 校验后 rename 原子落盘。
// 为不阻塞客户端透传，Write 永不报错：超单文件上限或磁盘写失败时转为降级
// （记录状态、丢弃已写内容），Commit 时不落盘。
type Writer struct {
	c           *DiskLRU
	key         string
	contentType string
	f           *os.File
	n           int64
	overflow    bool // 超过 MaxFileBytes
	failed      bool // 磁盘写失败
}

// NewWriter 在临时目录创建写入器。CreateTemp 失败即降级（调用方应直连透传）。
func (c *DiskLRU) NewWriter(key, contentType string) (*Writer, error) {
	f, err := os.CreateTemp(c.cfg.TmpDir, "dl-*")
	if err != nil {
		return nil, fmt.Errorf("cache: create temp: %w", err)
	}
	return &Writer{c: c, key: key, contentType: contentType, f: f}, nil
}

// Write 实现 io.Writer（配合 io.TeeReader 边下边存）。
// 永不返回错误：超限/写盘失败进入降级路径，由 Commit 清理。
func (w *Writer) Write(p []byte) (int, error) {
	if w.overflow || w.failed {
		return len(p), nil
	}
	n, err := w.f.Write(p)
	w.n += int64(n)
	if err != nil {
		w.failed = true
		return len(p), nil
	}
	if w.n > w.c.cfg.MaxFileBytes {
		w.overflow = true
	}
	return len(p), nil
}

// Commit 校验并原子落盘（rename）+ 元数据入 DB；降级/超限时仅清理临时文件。
// stored = false 表示未入缓存（超限或写盘失败），err 仅报告 rename/DB 异常。
func (w *Writer) Commit(ctx context.Context) (stored bool, err error) {
	tmpPath := w.f.Name()
	defer func() {
		_ = w.f.Close()
		if !stored {
			_ = os.Remove(tmpPath)
		}
	}()
	if w.overflow || w.failed {
		return false, nil
	}
	if err := w.f.Close(); err != nil {
		return false, nil
	}

	finalPath := w.c.pathFor(w.key)
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		return false, fmt.Errorf("cache: mkdir shard: %w", err)
	}
	// Windows 上目标文件被瞬时占用（读者/索引器）会导致 rename 失败，退避重试；
	// 最终失败仅降级不缓存（§4.2：磁盘写失败不返回错误码）。
	var renameErr error
	for attempt := 0; attempt < 5; attempt++ {
		if renameErr = os.Rename(tmpPath, finalPath); renameErr == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if renameErr != nil {
		return false, fmt.Errorf("cache: rename: %w", renameErr)
	}

	now := time.Now().UnixMilli()
	var oldSize int64
	_ = w.c.db.QueryRowContext(ctx, "SELECT size FROM cache_meta WHERE key = ?", w.key).Scan(&oldSize)
	if _, err := w.c.db.ExecContext(ctx,
		"INSERT OR REPLACE INTO cache_meta (key, size, atime, created_at, content_type) VALUES (?, ?, ?, ?, ?)",
		w.key, w.n, now, now, w.contentType); err != nil {
		return false, fmt.Errorf("cache: upsert meta: %w", err)
	}
	w.c.total.Add(w.n - oldSize)
	w.c.maybeEvict()
	return true, nil
}

// Put 供其他模块（M6 恢复）整块写入缓存：流式下载到临时文件后原子落盘。
// stored = false 表示超限未入缓存。
func (c *DiskLRU) Put(ctx context.Context, key, contentType string, r io.Reader) (bool, error) {
	w, err := c.NewWriter(key, contentType)
	if err != nil {
		return false, err
	}
	_, _ = io.Copy(w, r) // Writer 降级语义，错误在 Commit 体现
	return w.Commit(ctx)
}

// maybeEvict 写后检查水位，超限则后台异步淘汰；TryLock 保证同一时刻只有一个淘汰，
// 已有淘汰在跑时直接跳过（下轮写后再触发）。
func (c *DiskLRU) maybeEvict() {
	if c.total.Load() <= c.watermark() {
		return
	}
	go func() {
		if !c.evictMu.TryLock() {
			return
		}
		defer c.evictMu.Unlock()
		c.evict(context.Background())
	}()
}

func (c *DiskLRU) watermark() int64 {
	return int64(c.cfg.HighWatermark * float64(c.cfg.MaxBytes))
}

// Evict 淘汰至水位以下（阻塞等待进行中的淘汰完成后执行，启动与测试用）。
func (c *DiskLRU) Evict(ctx context.Context) {
	c.evictMu.Lock()
	defer c.evictMu.Unlock()
	c.evict(ctx)
}

// evict 按 DB 内 atime 升序批量淘汰至水位以下，调用方需持有 evictMu。
// 文件删除失败（如 Windows 上被读者占用）跳过该条、保留元数据，留待下轮重试。
func (c *DiskLRU) evict(ctx context.Context) {

	for c.total.Load() > c.watermark() {
		rows, err := c.db.QueryContext(ctx,
			"SELECT key, size FROM cache_meta ORDER BY atime ASC LIMIT ?", c.cfg.EvictionBatch)
		if err != nil {
			slog.WarnContext(ctx, "cache evict query failed")
			return
		}
		type entry struct {
			key  string
			size int64
		}
		var batch []entry
		for rows.Next() {
			var e entry
			if err := rows.Scan(&e.key, &e.size); err == nil {
				batch = append(batch, e)
			}
		}
		_ = rows.Close()
		if len(batch) == 0 {
			return
		}

		for _, e := range batch {
			err := os.Remove(c.pathFor(e.key))
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				// 文件被占用等：跳过，元数据保留，下轮再试。
				continue
			}
			if _, err := c.db.ExecContext(ctx, "DELETE FROM cache_meta WHERE key = ?", e.key); err != nil {
				continue
			}
			c.total.Add(-e.size)
		}
		// 批量限速：批间隔 100ms，避免瞬时随机删除打满 HDD 队列（§6.4）。
		if c.total.Load() > c.watermark() {
			time.Sleep(evictBatchInterval)
		}
	}
}
