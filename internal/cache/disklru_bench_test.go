package cache

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/cqash/pixiv-relay/internal/db"
)

// benchDir 压测数据目录：默认 b.TempDir()（CI 可跑）；BENCH_DIR 环境变量
// 覆盖到指定磁盘根目录（如 HDD：BENCH_DIR=<bench 输出目录，如 D 盘>），
// 每 benchmark 一个子目录，结束后清理。
func benchDir(b *testing.B) string {
	b.Helper()
	base := os.Getenv("BENCH_DIR")
	if base == "" {
		return b.TempDir()
	}
	dir := filepath.Join(base, b.Name())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		b.Fatalf("mkdir bench dir: %v", err)
	}
	b.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// openBenchCache 在压测目录打开缓存（临时库 + 默认配置）。
func openBenchCache(b *testing.B) *DiskLRU {
	b.Helper()
	dir := benchDir(b)
	database, err := db.Open(filepath.Join(dir, "bench.db"))
	if err != nil {
		b.Fatalf("open db: %v", err)
	}
	b.Cleanup(func() { _ = database.Close() })
	if err := db.Migrate(context.Background(), database); err != nil {
		b.Fatalf("migrate: %v", err)
	}
	c, err := Open(database, Config{Dir: filepath.Join(dir, "cache")})
	if err != nil {
		b.Fatalf("open cache: %v", err)
	}
	return c
}

// BenchmarkCacheWrite 磁盘缓存写吞吐：流式写临时文件 + rename 原子落盘 + 元数据入库。
// 覆盖 64KB / 1MB / 8MB 三档单文件大小。
func BenchmarkCacheWrite(b *testing.B) {
	for _, size := range []int64{64 << 10, 1 << 20, 8 << 20} {
		b.Run(fmt.Sprintf("%dKB", size>>10), func(b *testing.B) {
			c := openBenchCache(b)
			data := make([]byte, size)
			ctx := context.Background()
			b.SetBytes(size)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				key := Key(fmt.Sprintf("bench-write/%d/%d", size, i))
				stored, err := c.Put(ctx, key, "application/octet-stream", bytes.NewReader(data))
				if err != nil {
					b.Fatalf("put: %v", err)
				}
				if !stored {
					b.Fatal("put not stored")
				}
			}
		})
	}
}

// BenchmarkCacheRead 磁盘缓存读吞吐：DB 查元数据 + 刷新 atime + os.Open +
// io.Copy 流式直出（命中路径不 stat）。覆盖 64KB / 1MB / 8MB 三档。
func BenchmarkCacheRead(b *testing.B) {
	for _, size := range []int64{64 << 10, 1 << 20, 8 << 20} {
		b.Run(fmt.Sprintf("%dKB", size>>10), func(b *testing.B) {
			c := openBenchCache(b)
			ctx := context.Background()
			key := Key(fmt.Sprintf("bench-read/%d", size))
			if stored, err := c.Put(ctx, key, "application/octet-stream",
				bytes.NewReader(make([]byte, size))); err != nil || !stored {
				b.Fatalf("seed put: stored=%v err=%v", stored, err)
			}
			b.SetBytes(size)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				f, _, err := c.Get(ctx, key)
				if err != nil || f == nil {
					b.Fatalf("get: f=%v err=%v", f != nil, err)
				}
				if _, err := io.Copy(io.Discard, f); err != nil {
					_ = f.Close()
					b.Fatalf("read: %v", err)
				}
				_ = f.Close()
			}
		})
	}
}
