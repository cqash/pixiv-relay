package crypto

import (
	"encoding/base64"
	"strings"
	"testing"
)

func testKey(t *testing.T) string {
	t.Helper()
	return base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
}

func TestLoad_EmptyDisables(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatalf("load empty: %v", err)
	}
	if c != nil {
		t.Fatal("empty key must yield nil cipher (encryption disabled)")
	}
	if c.Enabled() {
		t.Fatal("nil cipher must report Enabled()=false")
	}
}

func TestLoad_InvalidKey(t *testing.T) {
	if _, err := Load("not-base64!!!"); err == nil {
		t.Fatal("bad base64 must error")
	}
	short := base64.StdEncoding.EncodeToString([]byte("too-short"))
	if _, err := Load(short); err == nil {
		t.Fatal("non-32-byte key must error")
	}
}

func TestEncryptDecrypt_Roundtrip(t *testing.T) {
	c, err := Load(testKey(t))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	const plain = `{"illustId":123,"title":"测试"}`
	enc := c.Encrypt(plain)
	if !strings.HasPrefix(enc, Prefix) {
		t.Fatalf("ciphertext must carry %q prefix, got %q", Prefix, enc)
	}
	if strings.Contains(enc, plain) {
		t.Fatal("ciphertext leaks plaintext")
	}
	// 两次加密 nonce 随机，密文应不同。
	if c.Encrypt(plain) == enc {
		t.Fatal("nonce must be random: identical plaintext produced identical ciphertext")
	}
	dec, err := c.Decrypt(enc)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if dec != plain {
		t.Fatalf("roundtrip mismatch: got %q", dec)
	}
}

func TestDecrypt_PlaintextPassthrough(t *testing.T) {
	c, err := Load(testKey(t))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	const plain = `{"legacy":true}`
	dec, err := c.Decrypt(plain) // 无前缀存量明文原样返回（混存兼容）
	if err != nil {
		t.Fatalf("decrypt plaintext: %v", err)
	}
	if dec != plain {
		t.Fatalf("plaintext passthrough mismatch: got %q", dec)
	}
}

func TestDecrypt_Corrupted(t *testing.T) {
	c, err := Load(testKey(t))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := c.Decrypt(Prefix + "@@@"); err == nil {
		t.Fatal("bad base64 ciphertext must error")
	}
	if _, err := c.Decrypt(Prefix + base64.StdEncoding.EncodeToString([]byte("x"))); err == nil {
		t.Fatal("short ciphertext must error")
	}
	tampered := c.Encrypt("secret")[:len(Prefix)+4] + "AAAA"
	if _, err := c.Decrypt(tampered); err == nil {
		t.Fatal("tampered ciphertext must error")
	}
}

func TestNilCipher_Passthrough(t *testing.T) {
	var c *Cipher
	const plain = `{"a":1}`
	if got := c.Encrypt(plain); got != plain {
		t.Fatalf("nil cipher encrypt must passthrough, got %q", got)
	}
	if got, err := c.Decrypt(plain); err != nil || got != plain {
		t.Fatalf("nil cipher decrypt plaintext must passthrough, got %q err %v", got, err)
	}
	// 数据已加密但未配置密钥：必须报错而非静默。
	if _, err := c.Decrypt(Prefix + "AAAA"); err == nil {
		t.Fatal("nil cipher must error on encrypted data")
	}
}
