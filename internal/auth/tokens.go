// Package auth 服务认证（设计文档 §5）：设备注册、token 轮换、Bearer 鉴权中间件。
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"time"
)

// token 与 accountKey 均为不透明随机串，DB 只存 SHA-256 十六进制哈希（§5.1 / §9）。
const (
	accessTokenPrefix  = "rl_at_"
	refreshTokenPrefix = "rl_rt_"
	accountKeyPrefix   = "rk_"

	// AccessTokenTTL access token 有效期 30 天，RefreshTokenTTL refresh token 180 天。
	AccessTokenTTL  = 30 * 24 * time.Hour
	RefreshTokenTTL = 180 * 24 * time.Hour

	// ExpiresIn register/refresh 响应的 expiresIn（秒），= AccessTokenTTL。
	ExpiresIn = int64(AccessTokenTTL / time.Second) // 2592000
)

// newOpaqueToken 生成 32 字节随机不透明串，base64url 编码后加前缀。
func newOpaqueToken(prefix string) (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// NewAccountKey 生成新账号的 accountKey（`rk_` 前缀）。
func NewAccountKey() (string, error) {
	return newOpaqueToken(accountKeyPrefix)
}

// HashToken 计算凭据的 SHA-256 十六进制哈希（入库值）。
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
