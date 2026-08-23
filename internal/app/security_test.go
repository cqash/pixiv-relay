package app

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/cqash/pixiv-relay/internal/common"
	"github.com/cqash/pixiv-relay/internal/crypto"
)

// 本文件为 M8 安全自查脚本化测试（§9）：日志脱敏端到端、目录穿越防护、
// token/accountKey 不落库明文、注册限流、静态加密落库。
// 数据静态加密的逐字段混存/解密用例在 M7 已覆盖（sync.TestEncryptedAtRest /
// sync.TestEncryptedWithoutKeyErrors / recover.TestEncryptedAtRest），
// 此处仅补一条 app 装配层（NewWithClient 接线）的端到端确认，不重复造用例。

// TestSecurityLogRedaction 日志脱敏端到端：slog 重定向到 buffer（与 main.go 同款
// RedactHandler 装配），触发 relay（敏感头/query/body）与 img（URL 半敏感）请求后，
// 断言日志输出不含任何敏感值。
func TestSecurityLogRedaction(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(common.NewRedactHandler(slog.NewJSONHandler(&buf, nil))))
	t.Cleanup(func() { slog.SetDefault(old) })

	e := startE2E(t, e2eConfig(),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		}),
		"app-api.pixiv.net", "i.pximg.net")

	code, reg, _ := doJSON(t, http.MethodPost, e.srv.URL+"/auth/v1/register", "",
		map[string]any{"deviceName": "sec-dev"})
	if code != http.StatusOK {
		t.Fatalf("register = %d %v", code, reg)
	}
	token, _ := reg["accessToken"].(string)
	accountKey, _ := reg["accountKey"].(string)

	// relay：敏感 Authorization 头 + URL query + bodyBase64。
	secretBody := base64.StdEncoding.EncodeToString([]byte("pixiv-refresh-secret"))
	code, _, _ = doJSON(t, http.MethodPost, e.srv.URL+"/relay/v1/request", token,
		map[string]any{
			"method":     "POST",
			"url":        "https://app-api.pixiv.net/v1/x?secret_query=1",
			"headers":    map[string]string{"Authorization": "Bearer pixiv-token-secret"},
			"bodyBase64": secretBody,
		})
	if code != http.StatusOK {
		t.Fatalf("relay = %d", code)
	}
	// img：URL 含半敏感 query。
	imgCode, _, _ := fetchRaw(t,
		e.srv.URL+"/img/v1/fetch?url=https%3A%2F%2Fi.pximg.net%2Fsecret%2Fimg.jpg%3Ftoken%3Dsecret", token)
	if imgCode != http.StatusOK {
		t.Fatalf("img = %d", imgCode)
	}

	out := buf.String()
	for _, s := range []string{
		"pixiv-token-secret", "pixiv-refresh-secret", secretBody,
		"secret_query", "token=secret",
		token, accountKey, // 服务凭据绝不出现在任何日志行
	} {
		if strings.Contains(out, s) {
			t.Fatalf("log leaked sensitive value %q:\n%s", s, out)
		}
	}
}

// TestSecurityPathTraversal 目录穿越防护：recover pid 非纯数字（含 ../ 形态）→ 400；
// img url 非白名单域名带 .. → 403。均走统一错误格式。
func TestSecurityPathTraversal(t *testing.T) {
	e := startE2E(t, e2eConfig(),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }),
		"i.pximg.net")
	code, reg, _ := doJSON(t, http.MethodPost, e.srv.URL+"/auth/v1/register", "",
		map[string]any{"deviceName": "sec-dev"})
	if code != http.StatusOK {
		t.Fatalf("register = %d %v", code, reg)
	}
	token, _ := reg["accessToken"].(string)

	// recover：转义形态 "../etc" 作为单个路径段送达 handler，pid 校验拒绝。
	// （裸 "../" 形态由 ServeMux 路径清洗 301 重定向兜底，同样到不了 handler。）
	code, body, _ := doJSON(t, http.MethodGet,
		e.srv.URL+"/recover/v1/illust/..%2Fetc", token, nil)
	if code != http.StatusBadRequest || errCodeOf(body) != "VALIDATION_FAILED" {
		t.Fatalf("recover traversal = %d %v, want 400 VALIDATION_FAILED", code, body)
	}

	// img：非白名单域名（路径带 ..）→ 403 FORBIDDEN。
	code, body, _ = doJSON(t, http.MethodGet,
		e.srv.URL+"/img/v1/fetch?url=https%3A%2F%2Fevil.example.com%2F..%2Fsecret", token, nil)
	if code != http.StatusForbidden || errCodeOf(body) != "FORBIDDEN" {
		t.Fatalf("img traversal = %d %v, want 403 FORBIDDEN", code, body)
	}
}

// TestSecurityNoPlaintextSecrets token / accountKey 不落库明文：
// register 后直接查库，devices 与 accounts 表只存 SHA-256 十六进制哈希。
func TestSecurityNoPlaintextSecrets(t *testing.T) {
	e := startE2E(t, e2eConfig(),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }),
		"app-api.pixiv.net")
	code, reg, _ := doJSON(t, http.MethodPost, e.srv.URL+"/auth/v1/register", "",
		map[string]any{"deviceName": "sec-dev"})
	if code != http.StatusOK {
		t.Fatalf("register = %d %v", code, reg)
	}
	plain := []string{
		reg["accessToken"].(string), reg["refreshToken"].(string), reg["accountKey"].(string),
	}

	rows, err := e.db.Query(
		"SELECT access_token_hash, refresh_token_hash FROM devices")
	if err != nil {
		t.Fatalf("query devices: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var hashes []string
	for rows.Next() {
		var at, rt string
		if err := rows.Scan(&at, &rt); err != nil {
			t.Fatalf("scan devices: %v", err)
		}
		hashes = append(hashes, at, rt)
	}
	var akHash string
	if err := e.db.QueryRow("SELECT account_key_hash FROM accounts").Scan(&akHash); err != nil {
		t.Fatalf("query accounts: %v", err)
	}
	hashes = append(hashes, akHash)

	if len(hashes) < 3 {
		t.Fatalf("expect stored hashes, got %v", hashes)
	}
	for _, h := range hashes {
		if len(h) != 64 || strings.Trim(h, "0123456789abcdef") != "" {
			t.Fatalf("stored value is not a sha256 hex hash: %q", h)
		}
		for _, p := range plain {
			if h == p || strings.Contains(h, p) {
				t.Fatalf("plaintext credential stored in db: %q", p)
			}
		}
	}
}

// TestSecurityRegisterRateLimit 注册端点限流（§9：10 次/小时/IP，burst 3）：
// 调低配额后连发超限 → 429 + Retry-After + RATE_LIMITED。
func TestSecurityRegisterRateLimit(t *testing.T) {
	cfg := e2eConfig()
	cfg.RateRegisterPerHour = 6 // 0.1 次/秒，burst 3：第 4 个请求必被限流
	e := startE2E(t, cfg,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }),
		"app-api.pixiv.net")

	var code int
	var body map[string]any
	var hdr http.Header
	for i := 0; i < 4; i++ {
		code, body, hdr = doJSON(t, http.MethodPost, e.srv.URL+"/auth/v1/register", "",
			map[string]any{"deviceName": fmt.Sprintf("dev-%d", i)})
	}
	if code != http.StatusTooManyRequests {
		t.Fatalf("4th register = %d %v, want 429", code, body)
	}
	if errCodeOf(body) != "RATE_LIMITED" {
		t.Fatalf("error.code = %v, want RATE_LIMITED", body)
	}
	if hdr.Get("Retry-After") == "" {
		t.Fatal("429 response missing Retry-After header")
	}
}

// TestSecurityEncryptedAtRest app 装配层静态加密确认：DATA_ENC_KEY 开启后经
// NewWithClient 接线，HTTP push 的 sync 数据落库为 enc:v1: 密文且不含明文。
// （逐字段混存/解密/无密钥报错用例见 sync/recover 包 M7 测试。）
func TestSecurityEncryptedAtRest(t *testing.T) {
	cfg := e2eConfig()
	cfg.DataEncKey = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=" // 32 字节测试密钥
	e := startE2E(t, cfg,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }),
		"app-api.piximg.net", "app-api.pixiv.net")

	code, reg, _ := doJSON(t, http.MethodPost, e.srv.URL+"/auth/v1/register", "",
		map[string]any{"deviceName": "sec-dev"})
	if code != http.StatusOK {
		t.Fatalf("register = %d %v", code, reg)
	}
	token, _ := reg["accessToken"].(string)

	code, body, _ := doJSON(t, http.MethodPost, e.srv.URL+"/sync/v1/push", token,
		map[string]any{
			"domain": "history",
			"items": []map[string]any{
				{"key": "sec1", "data": map[string]any{"title": "明文敏感标记"}, "updatedAt": 1},
			},
		})
	if code != http.StatusOK {
		t.Fatalf("push = %d %v", code, body)
	}

	var data string
	if err := e.db.QueryRow(
		`SELECT data FROM sync_entries WHERE domain = 'history' AND "key" = 'sec1'`).Scan(&data); err != nil {
		t.Fatalf("query sync entry: %v", err)
	}
	if !strings.HasPrefix(data, crypto.Prefix) {
		t.Fatalf("sync data must carry %q prefix, got %q", crypto.Prefix, data)
	}
	if strings.Contains(data, "明文敏感标记") {
		t.Fatal("sync ciphertext leaks plaintext")
	}

	// 读回路径：pull 解密还原一致。
	code, pull, _ := doJSON(t, http.MethodGet, e.srv.URL+"/sync/v1/pull?domain=history", token, nil)
	if code != http.StatusOK {
		t.Fatalf("pull = %d %v", code, pull)
	}
	items := pull["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("pull items = %v", items)
	}
	itemData, _ := json.Marshal(items[0].(map[string]any)["data"])
	if !strings.Contains(string(itemData), "明文敏感标记") {
		t.Fatalf("pull data not decrypted: %s", itemData)
	}
}
