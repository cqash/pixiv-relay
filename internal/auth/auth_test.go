package auth

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/arkpix/relay/internal/common"
	"github.com/arkpix/relay/internal/db"
)

// setup 构建测试用 HTTP handler：临时库（t.TempDir）+ auth 路由 + 挂鉴权中间件的探针端点。
// 外层包 RequestID 使错误响应带 requestId。
func setup(t *testing.T, inviteCodes, staticTokens []string) (http.Handler, *sql.DB) {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := db.Migrate(context.Background(), database); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	mux := http.NewServeMux()
	RegisterRoutes(mux, NewService(database, inviteCodes))
	mw, err := NewMiddleware(database, staticTokens)
	if err != nil {
		t.Fatalf("new middleware: %v", err)
	}
	mux.Handle("GET /probe", mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accountID, _ := AccountIDFrom(r.Context())
		deviceID, _ := DeviceIDFrom(r.Context())
		common.WriteJSON(w, r, http.StatusOK, map[string]int64{
			"accountId": accountID,
			"deviceId":  deviceID,
		})
	})))
	return common.RequestID(mux), database
}

// doJSON 发起 JSON 请求。每次调用使用唯一 RemoteAddr，避免触发注册端点
// 10 次/小时/IP 的限流（burst 3）干扰用例本身。
var requestSeq int

func doJSON(t *testing.T, h http.Handler, method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	requestSeq++
	req := httptest.NewRequest(method, path, reader)
	req.RemoteAddr = fmt.Sprintf("192.0.2.%d:12345", requestSeq%250+1)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v (raw: %s)", err, rec.Body.String())
	}
	return body
}

func register(t *testing.T, h http.Handler, body map[string]any) map[string]any {
	t.Helper()
	rec := doJSON(t, h, http.MethodPost, "/auth/v1/register", body, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("register want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	return decodeBody(t, rec)
}

func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

func TestFullFlow(t *testing.T) {
	h, _ := setup(t, nil, nil)

	reg := register(t, h, map[string]any{"deviceName": "test-device"})
	access, _ := reg["accessToken"].(string)
	refresh, _ := reg["refreshToken"].(string)
	if !strings.HasPrefix(access, "rl_at_") || !strings.HasPrefix(refresh, "rl_rt_") {
		t.Fatalf("token prefixes wrong: %q / %q", access, refresh)
	}
	if !strings.HasPrefix(reg["accountKey"].(string), "rk_") {
		t.Fatalf("accountKey prefix wrong: %v", reg["accountKey"])
	}
	if reg["expiresIn"].(float64) != 2592000 {
		t.Fatalf("expiresIn want 2592000, got %v", reg["expiresIn"])
	}
	if reg["serverVersion"] != "1.1.0" {
		t.Fatalf("serverVersion want 1.1.0, got %v", reg["serverVersion"])
	}
	if reg["requestId"] == nil || reg["requestId"] == "" {
		t.Fatal("register response missing requestId")
	}

	// 带 access token 访问探针端点
	rec := doJSON(t, h, http.MethodGet, "/probe", nil, bearer(access))
	if rec.Code != http.StatusOK {
		t.Fatalf("probe want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// refresh 换新对
	rec = doJSON(t, h, http.MethodPost, "/auth/v1/refresh",
		map[string]any{"refreshToken": refresh}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	ref := decodeBody(t, rec)
	newAccess, _ := ref["accessToken"].(string)
	newRefresh, _ := ref["refreshToken"].(string)
	if newAccess == access || newRefresh == refresh {
		t.Fatal("refresh must rotate to a new token pair")
	}

	// 新 access 可用
	rec = doJSON(t, h, http.MethodGet, "/probe", nil, bearer(newAccess))
	if rec.Code != http.StatusOK {
		t.Fatalf("probe with new access want 200, got %d", rec.Code)
	}

	// 旧 access 立即失效
	rec = doJSON(t, h, http.MethodGet, "/probe", nil, bearer(access))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("old access want 401, got %d", rec.Code)
	}
	// 旧 refresh 单次使用
	rec = doJSON(t, h, http.MethodPost, "/auth/v1/refresh",
		map[string]any{"refreshToken": refresh}, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("old refresh want 401, got %d", rec.Code)
	}
}

func TestAccountKeyJoin(t *testing.T) {
	h, database := setup(t, nil, nil)

	regA := register(t, h, map[string]any{"deviceName": "device-A"})
	accountKey, _ := regA["accountKey"].(string)
	accessA, _ := regA["accessToken"].(string)

	// B 设备带 accountKey 加入同一账号
	regB := register(t, h, map[string]any{"deviceName": "device-B", "accountKey": accountKey})
	accessB, _ := regB["accessToken"].(string)
	if regB["accountKey"] != accountKey {
		t.Fatalf("join should return same accountKey, got %v", regB["accountKey"])
	}

	// 两设备同 account_id（通过 DB 断言）
	var accA, accB int64
	if err := database.QueryRow(
		"SELECT account_id FROM devices WHERE access_token_hash = ?", HashToken(accessA)).Scan(&accA); err != nil {
		t.Fatalf("lookup device A: %v", err)
	}
	if err := database.QueryRow(
		"SELECT account_id FROM devices WHERE access_token_hash = ?", HashToken(accessB)).Scan(&accB); err != nil {
		t.Fatalf("lookup device B: %v", err)
	}
	if accA != accB {
		t.Fatalf("devices should share account, got %d vs %d", accA, accB)
	}

	// 无效 accountKey → 400，不静默新建
	rec := doJSON(t, h, http.MethodPost, "/auth/v1/register",
		map[string]any{"deviceName": "device-C", "accountKey": "rk_nonexistent"}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid accountKey want 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestInviteCodes(t *testing.T) {
	h, _ := setup(t, []string{"invite-1"}, nil)

	// 无 inviteCode → 403
	rec := doJSON(t, h, http.MethodPost, "/auth/v1/register",
		map[string]any{"deviceName": "d1"}, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing inviteCode want 403, got %d", rec.Code)
	}
	// 错误 inviteCode → 403
	rec = doJSON(t, h, http.MethodPost, "/auth/v1/register",
		map[string]any{"deviceName": "d1", "inviteCode": "wrong"}, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong inviteCode want 403, got %d", rec.Code)
	}
	// 正确 inviteCode → 200
	rec = doJSON(t, h, http.MethodPost, "/auth/v1/register",
		map[string]any{"deviceName": "d1", "inviteCode": "invite-1"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid inviteCode want 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestStaticTokens(t *testing.T) {
	h, _ := setup(t, nil, []string{"preset-static-token"})

	rec := doJSON(t, h, http.MethodGet, "/probe", nil, bearer("preset-static-token"))
	if rec.Code != http.StatusOK {
		t.Fatalf("static token want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	if body["accountId"].(float64) == 0 {
		t.Fatal("static token should map to builtin shared account")
	}

	rec = doJSON(t, h, http.MethodGet, "/probe", nil, bearer("not-configured"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown token want 401, got %d", rec.Code)
	}
}

func TestMissingAuthorization(t *testing.T) {
	h, _ := setup(t, nil, nil)

	rec := doJSON(t, h, http.MethodGet, "/probe", nil, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
	// 统一错误格式 {error:{code,message,requestId}}（§4.2）
	body := decodeBody(t, rec)
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("missing error object: %s", rec.Body.String())
	}
	if errObj["code"] != "INVALID_TOKEN" {
		t.Fatalf("error.code want INVALID_TOKEN, got %v", errObj["code"])
	}
	if errObj["requestId"] == nil || errObj["requestId"] == "" {
		t.Fatal("error.requestId missing")
	}
	if got := rec.Header().Get("X-Request-Id"); got == "" || got != errObj["requestId"] {
		t.Fatalf("requestId mismatch: header %q vs body %v", got, errObj["requestId"])
	}
}
