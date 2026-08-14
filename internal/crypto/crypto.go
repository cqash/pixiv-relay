// Package crypto 提供用户数据（同步条目 / 恢复缓存）的静态加密（设计文档 §9）。
// 算法 AES-256-GCM，密钥由部署方经 DATA_ENC_KEY 环境变量管理（base64 编码 32 字节）。
// 密文格式：enc:v1:base64(nonce || ciphertext)。空密钥 = 不加密（向后兼容存量明文）；
// 读出时按前缀区分，无前缀视为明文原样返回（加密开启前后数据可混存）。
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Prefix 密文前缀标记：版本化，便于未来轮换算法/密钥格式。
const Prefix = "enc:v1:"

// keySize AES-256 密钥长度（字节）。
const keySize = 32

// Cipher 数据加密器。nil *Cipher 表示未启用加密，Encrypt/Decrypt 均为透传，
// 因此调用方无需判空。
type Cipher struct {
	aead cipher.AEAD
}

// Load 解析 DATA_ENC_KEY：空串 → (nil, nil) 不加密；
// base64 解码失败或长度非 32 字节 → 报错（调用方应启动失败退出）。
func Load(keyB64 string) (*Cipher, error) {
	keyB64 = strings.TrimSpace(keyB64)
	if keyB64 == "" {
		return nil, nil
	}
	key, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil {
		return nil, fmt.Errorf("crypto: DATA_ENC_KEY base64 decode: %w", err)
	}
	if len(key) != keySize {
		return nil, fmt.Errorf("crypto: DATA_ENC_KEY must be %d bytes after base64 decode, got %d", keySize, len(key))
	}
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: aead}, nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: aes: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: gcm: %w", err)
	}
	return aead, nil
}

// Enabled 报告是否启用了加密。
func (c *Cipher) Enabled() bool { return c != nil }

// Encrypt 加密明文并返回带前缀的密文串；未启用（nil）时原样返回。
func (c *Cipher) Encrypt(plaintext string) string {
	if c == nil {
		return plaintext
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		// crypto/rand 失败属于致命环境错误，没有可降级的安全路径。
		panic(fmt.Sprintf("crypto: rand nonce: %v", err))
	}
	sealed := c.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return Prefix + base64.StdEncoding.EncodeToString(sealed)
}

// Decrypt 解密数据：有 enc:v1: 前缀则解密，无前缀视为明文原样返回（混存兼容）。
// 未启用（nil）时：明文透传；遇密文前缀则报错（数据已加密但缺少密钥）。
func (c *Cipher) Decrypt(s string) (string, error) {
	if !strings.HasPrefix(s, Prefix) {
		return s, nil
	}
	if c == nil {
		return "", errors.New("crypto: data is encrypted but DATA_ENC_KEY is not configured")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(s, Prefix))
	if err != nil {
		return "", fmt.Errorf("crypto: ciphertext base64 decode: %w", err)
	}
	if len(raw) < c.aead.NonceSize() {
		return "", errors.New("crypto: ciphertext too short")
	}
	nonce, sealed := raw[:c.aead.NonceSize()], raw[c.aead.NonceSize():]
	plain, err := c.aead.Open(nil, nonce, sealed, nil)
	if err != nil {
		return "", fmt.Errorf("crypto: decrypt: %w", err)
	}
	return string(plain), nil
}
