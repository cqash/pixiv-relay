package common

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// M1 验收：401 响应体含 error.code=INVALID_TOKEN，requestId 与响应头一致。
func TestWriteError_Unauthorized(t *testing.T) {
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, r, Unauthorized("token expired"))
	}))

	req := httptest.NewRequest(http.MethodGet, "/relay/v1/request", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
	var body struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"requestId"`
		} `json:"error"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Error.Code != "INVALID_TOKEN" {
		t.Fatalf("want code INVALID_TOKEN, got %q", body.Error.Code)
	}
	hdrID := rec.Header().Get("X-Request-Id")
	if hdrID == "" || body.Error.RequestID != hdrID {
		t.Fatalf("requestId mismatch: header %q, body %q", hdrID, body.Error.RequestID)
	}
}

func TestWriteJSON_InjectsRequestID(t *testing.T) {
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, r, http.StatusOK, map[string]any{"items": []string{}, "nextCursor": ""})
	}))

	req := httptest.NewRequest(http.MethodGet, "/sync/v1/pull", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["requestId"] != rec.Header().Get("X-Request-Id") {
		t.Fatalf("requestId not injected or mismatched: %v", body["requestId"])
	}
	if _, ok := body["items"]; !ok {
		t.Fatal("payload field items missing")
	}
}

func TestWriteError_UnknownErrDegradesTo500(t *testing.T) {
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, r, http.ErrHijacked)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "Hijacked") {
		t.Fatal("internal error detail leaked to response body")
	}
}

func TestCursorRoundTrip(t *testing.T) {
	c := EncodeCursor(1718000000000, "abc123")
	ts, id, err := DecodeCursor(c)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ts != 1718000000000 || id != "abc123" {
		t.Fatalf("roundtrip mismatch: ts=%d id=%q", ts, id)
	}
	if _, _, err := DecodeCursor("!!!not-base64!!!"); err == nil {
		t.Fatal("want error for invalid cursor")
	} else {
		var ae *APIError
		if !errors.As(err, &ae) || ae.Status != http.StatusBadRequest {
			t.Fatal("invalid cursor should yield 400 APIError")
		}
	}
}

func TestParseLimitClamp(t *testing.T) {
	cases := map[string]int{"": 30, "1": 1, "100": 100, "101": 100, "0": 30, "abc": 30, "-5": 30}
	for q, want := range cases {
		req := httptest.NewRequest(http.MethodGet, "/x?limit="+q, nil)
		if got := ParseLimit(req); got != want {
			t.Fatalf("limit %q: want %d, got %d", q, want, got)
		}
	}
}

func TestRateLimiter(t *testing.T) {
	l := NewLimiter(60, 1) // 1/s，桶容量 1
	if ok, _ := l.allow("k1"); !ok {
		t.Fatal("first request should pass")
	}
	ok, retry := l.allow("k1")
	if ok {
		t.Fatal("second request should be limited")
	}
	if retry < 1 {
		t.Fatalf("retryAfter should be >= 1, got %d", retry)
	}
	if ok, _ := l.allow("k2"); !ok {
		t.Fatal("different key should have independent bucket")
	}
}

func TestRateLimitMiddleware(t *testing.T) {
	l := NewLimiter(60, 1)
	h := RequestID(l.Middleware(ClientIP)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, r, http.StatusOK, map[string]string{"ok": "1"})
	})))

	req := httptest.NewRequest(http.MethodPost, "/sync/v1/push", nil)
	req.RemoteAddr = "1.2.3.4:5678"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first request want 200, got %d", rec.Code)
	}

	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req.Clone(req.Context()))
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second request want 429, got %d", rec2.Code)
	}
	if rec2.Header().Get("Retry-After") == "" {
		t.Fatal("429 response must carry Retry-After header")
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(rec2.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Error.Code != "RATE_LIMITED" {
		t.Fatalf("want RATE_LIMITED, got %q", body.Error.Code)
	}
}

func TestClientIPTrustedProxy(t *testing.T) {
	newReq := func(remoteAddr string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = remoteAddr
		return req
	}

	// 直连部署：代理头不被信任（防伪造）
	r := newReq("9.9.9.9:1234")
	r.Header.Set("X-Forwarded-For", "1.1.1.1")
	if got := ClientIP(r); got != "9.9.9.9" {
		t.Fatalf("direct peer must ignore X-Forwarded-For, got %q", got)
	}

	// 回环反代：取 X-Forwarded-For 首个 IP
	r = newReq("127.0.0.1:5678")
	r.Header.Set("X-Forwarded-For", " 2.2.2.2 , 10.0.0.1")
	if got := ClientIP(r); got != "2.2.2.2" {
		t.Fatalf("loopback peer should use first X-Forwarded-For IP, got %q", got)
	}

	// 回环反代：X-Forwarded-For 缺失时退到 X-Real-IP
	r = newReq("[::1]:5678")
	r.Header.Set("X-Real-IP", "3.3.3.3")
	if got := ClientIP(r); got != "3.3.3.3" {
		t.Fatalf("loopback peer should fall back to X-Real-IP, got %q", got)
	}

	// 回环反代：两个头都缺失时仍用 RemoteAddr
	if got := ClientIP(newReq("127.0.0.1:5678")); got != "127.0.0.1" {
		t.Fatalf("no proxy headers should keep RemoteAddr, got %q", got)
	}
}

func TestRedactHandler(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(NewRedactHandler(slog.NewJSONHandler(&buf, nil)))
	logger.Info("relay",
		"authorization", "Bearer secret-token",
		"password", "hunter2",
		"method", "GET",
		slog.Group("nested", "refresh_token", "rt-secret"),
	)

	out := buf.String()
	for _, secret := range []string{"secret-token", "hunter2", "rt-secret"} {
		if strings.Contains(out, secret) {
			t.Fatalf("secret %q leaked into log: %s", secret, out)
		}
	}
	if !strings.Contains(out, "[REDACTED]") || !strings.Contains(out, "GET") {
		t.Fatalf("redaction or passthrough broken: %s", out)
	}
}

func TestDecodeJSONLimit(t *testing.T) {
	type payload struct {
		Name string `json:"name"`
	}
	// 正常 body
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"x"}`))
	rec := httptest.NewRecorder()
	var p payload
	if err := DecodeJSON(rec, req, &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.Name != "x" {
		t.Fatalf("want name x, got %q", p.Name)
	}
	// 超限 body（>1MB）
	big := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"`+strings.Repeat("a", 1<<20)+`"}`))
	rec2 := httptest.NewRecorder()
	if err := DecodeJSON(rec2, big, &p); err == nil {
		t.Fatal("want error for oversized body")
	}
	// 非法 JSON
	bad := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{oops`))
	rec3 := httptest.NewRecorder()
	if err := DecodeJSON(rec3, bad, &p); err == nil {
		t.Fatal("want error for invalid JSON")
	}
}
