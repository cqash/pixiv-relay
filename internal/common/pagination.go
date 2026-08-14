package common

import (
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
)

// 游标分页（§4.3）：不透明 cursor = base64url("<ts_ms>:<id>")。

// EncodeCursor 生成游标。
func EncodeCursor(ts int64, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(ts, 10) + ":" + id))
}

// DecodeCursor 解析游标；非法游标返回 *APIError(400)。
func DecodeCursor(s string) (ts int64, id string, err error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return 0, "", BadRequest("invalid cursor")
	}
	tsStr, id, ok := strings.Cut(string(raw), ":")
	if !ok || id == "" {
		return 0, "", BadRequest("invalid cursor")
	}
	ts, err = strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return 0, "", BadRequest("invalid cursor")
	}
	return ts, id, nil
}

// ParseLimit 解析并钳制 limit（§4.3：1..100，默认 30；非法值回落默认）。
func ParseLimit(r *http.Request) int {
	s := r.URL.Query().Get("limit")
	if s == "" {
		return 30
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 30
	}
	if n > 100 {
		return 100
	}
	return n
}
