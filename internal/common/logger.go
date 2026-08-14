package common

import (
	"context"
	"log/slog"
	"strings"
)

// sensitiveKeys 日志脱敏字段清单（§9：日志不落 Pixiv token / 服务凭据）。
var sensitiveKeys = map[string]bool{
	"authorization": true,
	"password":      true,
	"refresh_token": true,
	"refreshtoken":  true,
	"accesstoken":   true,
	"access_token":  true,
	"token":         true,
	"accountkey":    true,
	"account_key":   true,
	"invitecode":    true,
	"invite_code":   true,
	"body":          true,
	"bodybase64":    true,
}

// RedactHandler 包装 slog.Handler，命中敏感键的值替换为 [REDACTED]（含嵌套 group）。
type RedactHandler struct {
	inner slog.Handler
}

// NewRedactHandler 创建脱敏 Handler。
func NewRedactHandler(inner slog.Handler) *RedactHandler {
	return &RedactHandler{inner: inner}
}

// Enabled 透传底层 Handler。
func (h *RedactHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// Handle 脱敏后透传。
func (h *RedactHandler) Handle(ctx context.Context, rec slog.Record) error {
	out := slog.NewRecord(rec.Time, rec.Level, rec.Message, rec.PC)
	rec.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(redactAttr(a))
		return true
	})
	return h.inner.Handle(ctx, out)
}

// WithAttrs 脱敏后透传。
func (h *RedactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		out = append(out, redactAttr(a))
	}
	return &RedactHandler{inner: h.inner.WithAttrs(out)}
}

// WithGroup 透传底层 Handler。
func (h *RedactHandler) WithGroup(name string) slog.Handler {
	return &RedactHandler{inner: h.inner.WithGroup(name)}
}

func redactAttr(a slog.Attr) slog.Attr {
	if sensitiveKeys[strings.ToLower(a.Key)] {
		return slog.String(a.Key, "[REDACTED]")
	}
	if a.Value.Kind() == slog.KindGroup {
		grp := a.Value.Group()
		out := make([]slog.Attr, 0, len(grp))
		for _, g := range grp {
			out = append(out, redactAttr(g))
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(out...)}
	}
	return a
}
