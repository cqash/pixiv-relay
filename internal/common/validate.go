package common

import (
	"encoding/json"
	"net/http"
)

// DecodeJSON 解析 JSON body（上限 1 MB，§6.1），失败返回 *APIError(400)。
// 大文件传输禁止走此函数（HDD 策略：流式，见 PLAN.md §0）。
func DecodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	const maxBody = 1 << 20
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return BadRequest("invalid JSON body")
	}
	return nil
}

// Required 校验必填字符串字段，缺失返回 *APIError(400)，通过返回 nil。
func Required(field, value string) error {
	if value == "" {
		return BadRequest("missing field: " + field)
	}
	return nil
}
