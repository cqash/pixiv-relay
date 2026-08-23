// Package sync 数据同步（设计文档 §7）：6 个同步域的 LWW 合并、
// 单调 syncToken 游标、墓碑（删除标记）传播。
//
// 关键取舍：syncToken 格式 st_<seq>_<hex>，其数值部分直接等于该批次最大 seq，
// 与条目的服务端 seq 同源分配（max(上一水位+1, 当前毫秒) 起递增）。因此
// pull 的 since 游标无需额外映射表，解析 token 即得 seq 水位；
// token 仍按（账号×域）单调递增，满足 §7.2。
package sync

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	stdsync "sync"
)

// TokenStore 每（账号×域）当前 syncToken 的内存游标（PLAN §0：每域游标常驻内存）。
// 启动时从 sync_domains 表恢复。
type TokenStore struct {
	mu  stdsync.Mutex
	cur map[string]string // accountID+domain -> 当前 token
}

// NewTokenStore 创建空游标表。
func NewTokenStore() *TokenStore {
	return &TokenStore{cur: make(map[string]string)}
}

func tokenKey(accountID int64, domain string) string {
	return strconv.FormatInt(accountID, 10) + "\x00" + domain
}

// Load 启动时从 sync_domains 恢复各域游标（同名取数值更大者）。
func (s *TokenStore) Load(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx,
		"SELECT account_id, domain, sync_token FROM sync_domains WHERE sync_token <> ''")
	if err != nil {
		return fmt.Errorf("load sync tokens: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var accountID int64
		var domain, tok string
		if err := rows.Scan(&accountID, &domain, &tok); err != nil {
			return fmt.Errorf("scan sync token: %w", err)
		}
		k := tokenKey(accountID, domain)
		if cur, ok := s.cur[k]; ok {
			curMs, err1 := ParseToken(cur)
			newMs, err2 := ParseToken(tok)
			if err1 == nil && err2 == nil && curMs >= newMs {
				continue
			}
		}
		s.cur[k] = tok
	}
	return rows.Err()
}

// Current 返回该域当前 token（从未 push 过返回空串）。
func (s *TokenStore) Current(accountID int64, domain string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur[tokenKey(accountID, domain)]
}

// Next 推进该域游标：为本批 n 个条目分配连续 seq，返回起始 seq 与新 token。
// 水位 = max(上一水位+1, nowMs)，保证跨进程重启后仍单调（§7.2）。
// n=0（空批/全部幂等跳过）时仅推进 token，不产生 seq 消耗之外的语义。
func (s *TokenStore) Next(accountID int64, domain string, n int, nowMs int64) (seqBase int64, token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := tokenKey(accountID, domain)
	var prev int64
	if cur, ok := s.cur[k]; ok {
		if ms, err := ParseToken(cur); err == nil {
			prev = ms
		}
	}
	start := prev + 1
	if nowMs > start {
		start = nowMs
	}
	last := start
	if n > 0 {
		last = start + int64(n) - 1
	}
	tok := fmt.Sprintf("st_%d_%s", last, randHex6())
	s.cur[k] = tok
	return start, tok
}

// ParseToken 解析 st_<ms>_<6hex>，返回数值部分（= 该批次最大 seq）。
func ParseToken(tok string) (int64, error) {
	rest, ok := strings.CutPrefix(tok, "st_")
	if !ok {
		return 0, fmt.Errorf("invalid sync token: bad prefix")
	}
	msStr, hx, ok := strings.Cut(rest, "_")
	if !ok || len(hx) != 6 {
		return 0, fmt.Errorf("invalid sync token: bad format")
	}
	ms, err := strconv.ParseInt(msStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid sync token: bad number")
	}
	return ms, nil
}

// randHex6 生成 6 位随机 hex（token 随机段，防猜）。
func randHex6() string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand 失败属系统级故障
	}
	return hex.EncodeToString(b[:])
}
