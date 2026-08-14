// Package common 通用协议基础设施（设计文档 §4）：错误格式、requestId、分页、限流、日志脱敏。
package common

import "net/http"

// APIError 统一错误（§4.2）：响应体 {error:{code,message,requestId}}。
type APIError struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *APIError) Error() string { return e.Code + ": " + e.Message }

// NewError 构造指定 HTTP 状态码与业务码的错误。
func NewError(status int, code, message string) *APIError {
	return &APIError{Status: status, Code: code, Message: message}
}

// 常用错误构造（状态码语义对齐 §4.2）。
func BadRequest(msg string) *APIError {
	return NewError(http.StatusBadRequest, "VALIDATION_FAILED", msg)
}

func Unauthorized(msg string) *APIError {
	return NewError(http.StatusUnauthorized, "INVALID_TOKEN", msg)
}

func Forbidden(msg string) *APIError {
	return NewError(http.StatusForbidden, "FORBIDDEN", msg)
}

func NotFound(msg string) *APIError {
	return NewError(http.StatusNotFound, "NOT_FOUND", msg)
}

func RateLimited() *APIError {
	return NewError(http.StatusTooManyRequests, "RATE_LIMITED", "rate limit exceeded")
}

func BadGateway(msg string) *APIError {
	return NewError(http.StatusBadGateway, "UPSTREAM_UNREACHABLE", msg)
}
