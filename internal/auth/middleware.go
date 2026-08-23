package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/cqash/pixiv-relay/internal/common"
)

type ctxKey string

const (
	accountIDKey ctxKey = "accountId"
	deviceIDKey  ctxKey = "deviceId"
)

// AccountIDFrom 从上下文取鉴权通过的账号 ID（未鉴权返回 0, false）。
func AccountIDFrom(ctx context.Context) (int64, bool) {
	v, ok := ctx.Value(accountIDKey).(int64)
	return v, ok
}

// DeviceIDFrom 从上下文取鉴权通过的设备 ID（STATIC_TOKENS 模式为 0）。
func DeviceIDFrom(ctx context.Context) (int64, bool) {
	v, ok := ctx.Value(deviceIDKey).(int64)
	return v, ok
}

// staticAccountKeyHash 内置共享账号（STATIC_TOKENS 模式）的占位哈希：
// 取固定哨兵串的 SHA-256，不与任何真实 accountKey 冲突，也无对应明文可导出。
var staticAccountKeyHash = HashToken("arkpix-static-shared-account")

// Middleware Bearer 鉴权中间件：校验 access token 未过期并注入 accountID/deviceID；
// STATIC_TOKENS 预置 token 直接映射到内置共享账号（§5.1 私有部署模式）。
type Middleware struct {
	db              *sql.DB
	staticTokens    map[string]struct{}
	staticAccountID int64
}

// NewMiddleware 创建鉴权中间件；配置了 staticTokens 时确保内置共享账号存在。
func NewMiddleware(db *sql.DB, staticTokens []string) (*Middleware, error) {
	m := &Middleware{db: db}
	if len(staticTokens) == 0 {
		return m, nil
	}
	m.staticTokens = make(map[string]struct{}, len(staticTokens))
	for _, t := range staticTokens {
		m.staticTokens[t] = struct{}{}
	}
	id, err := ensureStaticAccount(db)
	if err != nil {
		return nil, err
	}
	m.staticAccountID = id
	return m, nil
}

// ensureStaticAccount 幂等创建内置共享账号行，返回其 ID。
func ensureStaticAccount(db *sql.DB) (int64, error) {
	if _, err := db.Exec(
		"INSERT OR IGNORE INTO accounts (account_key_hash, created_at) VALUES (?, ?)",
		staticAccountKeyHash, time.Now().UnixMilli()); err != nil {
		return 0, fmt.Errorf("ensure static account: %w", err)
	}
	var id int64
	if err := db.QueryRow(
		"SELECT id FROM accounts WHERE account_key_hash = ?", staticAccountKeyHash).Scan(&id); err != nil {
		return 0, fmt.Errorf("lookup static account: %w", err)
	}
	return id, nil
}

// Wrap 返回鉴权中间件：缺失/失效的 Authorization 一律 401 INVALID_TOKEN（§4.2）。
func (m *Middleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok {
			common.WriteError(w, r, common.Unauthorized("missing bearer token"))
			return
		}
		if _, ok := m.staticTokens[token]; ok {
			ctx := context.WithValue(r.Context(), accountIDKey, m.staticAccountID)
			ctx = context.WithValue(ctx, deviceIDKey, int64(0))
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		var accountID, deviceID int64
		err := m.db.QueryRowContext(r.Context(),
			"SELECT account_id, id FROM devices WHERE access_token_hash = ? AND access_expires_at > ?",
			HashToken(token), time.Now().UnixMilli()).Scan(&accountID, &deviceID)
		if errors.Is(err, sql.ErrNoRows) {
			common.WriteError(w, r, common.Unauthorized("access token invalid or expired"))
			return
		}
		if err != nil {
			common.WriteError(w, r, fmt.Errorf("lookup access token: %w", err))
			return
		}
		ctx := context.WithValue(r.Context(), accountIDKey, accountID)
		ctx = context.WithValue(ctx, deviceIDKey, deviceID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// bearerToken 解析 `Authorization: Bearer <token>` 头。
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	scheme, token, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", false
	}
	return token, true
}
