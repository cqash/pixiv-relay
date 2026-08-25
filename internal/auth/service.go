package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/cqash/pixiv-relay/internal/common"
)

// Service 认证业务逻辑（注册 / 刷新）。token 与 accountKey 只经手明文内存，
// 落库一律 SHA-256 哈希，且绝不写日志（§5.1 / §9）。
type Service struct {
	db          *sql.DB
	inviteCodes []string
	now         func() time.Time // 测试可注入；默认为 time.Now
}

// TokenPair 一次注册/刷新签发的凭据（明文，仅存在于响应中）。
type TokenPair struct {
	AccessToken  string
	RefreshToken string
	AccountKey   string
	ExpiresIn    int64
}

// NewService 创建认证服务。inviteCodes 为空切片表示开放注册（仅私有网络，§9）。
func NewService(db *sql.DB, inviteCodes []string) *Service {
	return &Service{db: db, inviteCodes: inviteCodes, now: time.Now}
}

// Register 注册设备（§5.1）：
//   - accountKey 非空时加入对应账号（无效按 400 拒绝，不静默新建）；
//     持有有效 accountKey 即证明账号所有权，跳过邀请码校验；
//   - accountKey 为空时新建账号并签发新 accountKey，
//     配置了 INVITE_CODES 时 inviteCode 必须匹配，否则 403。
func (s *Service) Register(ctx context.Context, deviceName, inviteCode, accountKey string) (*TokenPair, error) {
	if accountKey == "" && len(s.inviteCodes) > 0 && !slices.Contains(s.inviteCodes, inviteCode) {
		return nil, common.Forbidden("invalid invite code")
	}

	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin register tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var accountID int64
	if accountKey != "" {
		err := tx.QueryRowContext(ctx,
			"SELECT id FROM accounts WHERE account_key_hash = ?", HashToken(accountKey)).Scan(&accountID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, common.BadRequest("invalid accountKey")
		}
		if err != nil {
			return nil, fmt.Errorf("lookup account: %w", err)
		}
	} else {
		accountKey, err = NewAccountKey()
		if err != nil {
			return nil, err
		}
		res, err := tx.ExecContext(ctx,
			"INSERT INTO accounts (account_key_hash, created_at) VALUES (?, ?)",
			HashToken(accountKey), now.UnixMilli())
		if err != nil {
			return nil, fmt.Errorf("create account: %w", err)
		}
		if accountID, err = res.LastInsertId(); err != nil {
			return nil, fmt.Errorf("create account: %w", err)
		}
	}

	pair, err := s.issueTokens(ctx, tx, accountID, deviceName, inviteCode, now)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit register tx: %w", err)
	}
	pair.AccountKey = accountKey
	return pair, nil
}

// Refresh 用 refreshToken 轮换新 token 对（§5.1 轮换制）：
// 旧 access + refresh 立即失效，refreshToken 单次使用。
// 注意：accountKey 只存哈希无法还原明文，故响应不返回 accountKey（客户端注册时已持有）。
func (s *Service) Refresh(ctx context.Context, refreshToken string) (*TokenPair, error) {
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin refresh tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var deviceID, accountID int64
	err = tx.QueryRowContext(ctx,
		"SELECT id, account_id FROM devices WHERE refresh_token_hash = ? AND refresh_expires_at > ?",
		HashToken(refreshToken), now.UnixMilli()).Scan(&deviceID, &accountID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, common.Unauthorized("refresh token invalid or expired")
	}
	if err != nil {
		return nil, fmt.Errorf("lookup refresh token: %w", err)
	}

	access, err := newOpaqueToken(accessTokenPrefix)
	if err != nil {
		return nil, err
	}
	refresh, err := newOpaqueToken(refreshTokenPrefix)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE devices
		SET access_token_hash = ?, refresh_token_hash = ?, access_expires_at = ?, refresh_expires_at = ?
		WHERE id = ?`,
		HashToken(access), HashToken(refresh),
		now.Add(AccessTokenTTL).UnixMilli(), now.Add(RefreshTokenTTL).UnixMilli(),
		deviceID); err != nil {
		return nil, fmt.Errorf("rotate tokens: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit refresh tx: %w", err)
	}
	return &TokenPair{
		AccessToken:  access,
		RefreshToken: refresh,
		ExpiresIn:    ExpiresIn,
	}, nil
}

// issueTokens 在事务内为新设备签发 token 对并落库（哈希）。
func (s *Service) issueTokens(ctx context.Context, tx *sql.Tx, accountID int64, deviceName, inviteCode string, now time.Time) (*TokenPair, error) {
	access, err := newOpaqueToken(accessTokenPrefix)
	if err != nil {
		return nil, err
	}
	refresh, err := newOpaqueToken(refreshTokenPrefix)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO devices
		(account_id, device_name, access_token_hash, refresh_token_hash, invite_code, created_at, access_expires_at, refresh_expires_at)
		VALUES (?, ?, ?, ?, NULLIF(?, ''), ?, ?, ?)`,
		accountID, deviceName, HashToken(access), HashToken(refresh), inviteCode,
		now.UnixMilli(), now.Add(AccessTokenTTL).UnixMilli(), now.Add(RefreshTokenTTL).UnixMilli()); err != nil {
		return nil, fmt.Errorf("create device: %w", err)
	}
	return &TokenPair{
		AccessToken:  access,
		RefreshToken: refresh,
		ExpiresIn:    ExpiresIn,
	}, nil
}
