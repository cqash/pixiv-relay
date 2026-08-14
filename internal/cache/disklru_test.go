package cache

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arkpix/relay/internal/db"
)

// openTestCache 临时目录缓存 + 临时 SQLite（已跑迁移），绝不触碰 ./data。
func openTestCache(t *testing.T, cfg Config) (*DiskLRU, *sql.DB) {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := db.Migrate(context.Background(), database); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if cfg.Dir == "" {
		cfg.Dir = filepath.Join(t.TempDir(), "cache")
	}
	c, err := Open(database, cfg)
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	return c, database
}

func TestKeyShardedPath(t *testing.T) {
	c, _ := openTestCache(t, Config{})
	key := Key("https://i.pximg.net/img-original/img/2024/01/01/00/00/00/1_p0.jpg")
	if len(key) != 64 {
		t.Fatalf("key should be sha256 hex, got %q", key)
	}
	p := c.pathFor(key)
	want := filepath.Join(c.cfg.Dir, key[:2], key[2:4], key)
	if p != want {
		t.Fatalf("sharded path = %q, want %q", p, want)
	}
	if strings.Contains(p, "pximg") {
		t.Fatal("path must not contain url content")
	}
}

func TestFlatLayout(t *testing.T) {
	c, _ := openTestCache(t, Config{Layout: "flat"})
	key := Key("https://i.pximg.net/a.jpg")
	if p := c.pathFor(key); p != filepath.Join(c.cfg.Dir, key) {
		t.Fatalf("flat path = %q", p)
	}
}

func TestTmpDirSameVolumePrefix(t *testing.T) {
	// rename 原子性前提：临时目录与缓存目录同卷同前缀（§6.4）。
	c, _ := openTestCache(t, Config{})
	if !strings.HasPrefix(c.cfg.TmpDir, c.cfg.Dir) {
		t.Fatalf("tmp %q not under cache dir %q", c.cfg.TmpDir, c.cfg.Dir)
	}
}

func TestPutGetRoundtrip(t *testing.T) {
	c, _ := openTestCache(t, Config{})
	ctx := context.Background()
	key := Key("https://i.pximg.net/roundtrip.jpg")
	body := bytes.Repeat([]byte("pixiv-image-bytes"), 1000)

	stored, err := c.Put(ctx, key, "image/jpeg", bytes.NewReader(body))
	if err != nil || !stored {
		t.Fatalf("Put: stored=%v err=%v", stored, err)
	}
	f, meta, err := c.Get(ctx, key)
	if err != nil || f == nil {
		t.Fatalf("Get: f=%v err=%v", f, err)
	}
	defer func() { _ = f.Close() }()
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatal("content mismatch")
	}
	if meta.Size != int64(len(body)) || meta.ContentType != "image/jpeg" {
		t.Fatalf("meta = %+v", meta)
	}
	// 未命中
	f2, _, err := c.Get(ctx, Key("https://i.pximg.net/absent.jpg"))
	if err != nil || f2 != nil {
		t.Fatalf("miss: f=%v err=%v", f2, err)
	}
}

func TestPutOversizeNotStored(t *testing.T) {
	c, _ := openTestCache(t, Config{MaxFileBytes: 64})
	key := Key("https://i.pximg.net/big.jpg")
	stored, err := c.Put(context.Background(), key, "image/png", strings.NewReader(strings.Repeat("x", 65)))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if stored {
		t.Fatal("oversized file must not be stored")
	}
	if _, err := os.Stat(c.pathFor(key)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file should not exist, stat err = %v", err)
	}
}

func TestEvictionByAtime(t *testing.T) {
	// 小容量：3 × 100B，水位 0.9 × 250B = 225B → 淘汰至 ≤225B（删最旧两条）。
	c, database := openTestCache(t, Config{MaxBytes: 250, EvictionBatch: 2})
	ctx := context.Background()
	keys := []string{Key("https://i.pximg.net/1.jpg"), Key("https://i.pximg.net/2.jpg"), Key("https://i.pximg.net/3.jpg")}
	for i, k := range keys {
		if _, err := c.Put(ctx, k, "image/jpeg", strings.NewReader(strings.Repeat("d", 100))); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
		// 直接改 DB 内 atime，保证淘汰顺序确定（不依赖文件系统 atime）。
		if _, err := database.Exec("UPDATE cache_meta SET atime = ? WHERE key = ?", int64(1000+i), k); err != nil {
			t.Fatalf("set atime: %v", err)
		}
	}

	c.Evict(ctx)

	if total := c.total.Load(); total != 100 {
		t.Fatalf("total after evict = %d, want 100", total)
	}
	for _, k := range keys[:2] {
		if _, err := os.Stat(c.pathFor(k)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("evicted file %s should not exist, stat err = %v", k, err)
		}
		var n int
		if err := database.QueryRow("SELECT COUNT(1) FROM cache_meta WHERE key = ?", k).Scan(&n); err != nil || n != 0 {
			t.Fatalf("evicted meta %s should be gone, n = %d err = %v", k, n, err)
		}
	}
	if _, err := os.Stat(c.pathFor(keys[2])); err != nil {
		t.Fatalf("newest file should survive: %v", err)
	}
}

func TestStartupEviction(t *testing.T) {
	// 上次运行已超限，本次 Open（水位收紧）应立即淘汰。
	dir := t.TempDir()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = database.Close() }()
	if err := db.Migrate(context.Background(), database); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	c1, err := Open(database, Config{Dir: dir, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	key := Key("https://i.pximg.net/old.jpg")
	if _, err := c1.Put(context.Background(), key, "image/jpeg", strings.NewReader(strings.Repeat("d", 100))); err != nil {
		t.Fatalf("put: %v", err)
	}
	c2, err := Open(database, Config{Dir: dir, MaxBytes: 10}) // 水位 9B < 100B
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := os.Stat(c2.pathFor(key)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("startup evict should remove file, stat err = %v", err)
	}
}

func TestConcurrentPutSameKey(t *testing.T) {
	c, _ := openTestCache(t, Config{})
	ctx := context.Background()
	key := Key("https://i.pximg.net/race.jpg")
	body := strings.Repeat("z", 4096)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Put(ctx, key, "image/jpeg", strings.NewReader(body)); err != nil {
				t.Errorf("Put: %v", err)
			}
		}()
	}
	wg.Wait()
	got, err := os.ReadFile(c.pathFor(key))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if string(got) != body {
		t.Fatalf("content corrupted: %d bytes", len(got))
	}
	if c.total.Load() != int64(len(body)) {
		t.Fatalf("total = %d, want %d", c.total.Load(), len(body))
	}
}

func TestGetUpdatesAtime(t *testing.T) {
	c, database := openTestCache(t, Config{})
	ctx := context.Background()
	key := Key("https://i.pximg.net/atime.jpg")
	if _, err := c.Put(ctx, key, "image/jpeg", strings.NewReader("data")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	before := time.Now().UnixMilli()
	f, _, err := c.Get(ctx, key)
	if err != nil || f == nil {
		t.Fatalf("Get: %v", err)
	}
	_ = f.Close()
	var atime int64
	if err := database.QueryRow("SELECT atime FROM cache_meta WHERE key = ?", key).Scan(&atime); err != nil {
		t.Fatalf("query atime: %v", err)
	}
	if atime < before {
		t.Fatalf("atime not refreshed: %d < %d", atime, before)
	}
}
