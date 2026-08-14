// Package db 提供 SQLite 连接（WAL/NORMAL，纯 Go 驱动）与迁移执行器。
package db

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// Open 打开 SQLite 数据库（不存在则创建）。
// Pragma 对齐 PLAN.md §0 机械硬盘策略：WAL + synchronous=NORMAL，
// foreign_keys=ON，busy_timeout=5000ms 等待写锁而非立即 SQLITE_BUSY。
func Open(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)",
		filepath.ToSlash(path))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	return db, nil
}
