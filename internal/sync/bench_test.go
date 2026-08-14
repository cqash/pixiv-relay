package sync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/arkpix/relay/internal/db"
)

// benchDir 压测数据目录：默认 b.TempDir()（CI 可跑）；BENCH_DIR 环境变量
// 覆盖到指定磁盘根目录（如 HDD：BENCH_DIR=pix_relay_bench），
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

// BenchmarkSyncPushBatch 同步批量 push 吞吐：100 / 500 条一批，
// LWW 查重 + 事务 upsert + token 前进 + 墓碑惰性清理全链路。
func BenchmarkSyncPushBatch(b *testing.B) {
	for _, n := range []int{100, 500} {
		b.Run(fmt.Sprintf("%d", n), func(b *testing.B) {
			database, err := db.Open(filepath.Join(benchDir(b), "bench.db"))
			if err != nil {
				b.Fatalf("open db: %v", err)
			}
			b.Cleanup(func() { _ = database.Close() })
			ctx := context.Background()
			if err := db.Migrate(ctx, database); err != nil {
				b.Fatalf("migrate: %v", err)
			}
			// 直插账号行（push 只按 account_id 隔离，不校验设备）。
			res, err := database.Exec(
				"INSERT INTO accounts (account_key_hash, created_at) VALUES ('bench-account', 0)")
			if err != nil {
				b.Fatalf("insert account: %v", err)
			}
			accountID, _ := res.LastInsertId()

			svc, err := NewService(ctx, database, nil)
			if err != nil {
				b.Fatalf("new service: %v", err)
			}

			// 单条 data 约 64 B JSON，SetBytes 按整批估算负载字节数。
			const itemBytes = int64(64)
			b.SetBytes(itemBytes * int64(n))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				items := make([]PushItem, n)
				base := time.Now().UnixMilli()
				for j := range items {
					items[j] = PushItem{
						Key:       fmt.Sprintf("bench-%d-%d", i, j),
						Data:      []byte(`{"illustId":12345678,"title":"benchmark 压测条目"}`),
						UpdatedAt: base + int64(j),
					}
				}
				if _, err := svc.Push(ctx, accountID, &PushRequest{
					Domain: DomainHistory,
					Items:  items,
				}); err != nil {
					b.Fatalf("push: %v", err)
				}
			}
		})
	}
}
