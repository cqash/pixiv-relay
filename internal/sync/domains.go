package sync

import "strings"

// 合法同步域（§7.1）。所有域冲突策略均为按 key 的 LWW（mute 净效果为集合并集）。
const (
	DomainHistory          = "history"
	DomainSearchHistory    = "search_history"
	DomainBookmarkSnapshot = "bookmark_snapshot"
	DomainMute             = "mute"
	DomainExifConfig       = "exif_config"
	DomainSettings         = "settings"
)

var validDomains = map[string]struct{}{
	DomainHistory:          {},
	DomainSearchHistory:    {},
	DomainBookmarkSnapshot: {},
	DomainMute:             {},
	DomainExifConfig:       {},
	DomainSettings:         {},
}

// validDomain 校验 domain 是否为 6 个合法域之一。
func validDomain(d string) bool {
	_, ok := validDomains[d]
	return ok
}

// containsSensitiveField 递归检查 JSON 对象字段名是否含 "token"（大小写不敏感）。
// 敏感字段排除本是客户端职责（§7.2），服务端做防御性拒写，防 Pixiv token 误上行。
func containsSensitiveField(v any) bool {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if strings.Contains(strings.ToLower(k), "token") {
				return true
			}
			if containsSensitiveField(val) {
				return true
			}
		}
	case []any:
		for _, e := range t {
			if containsSensitiveField(e) {
				return true
			}
		}
	}
	return false
}

// validateBookmarkSnapshot 校验收藏快照结构（§7.3）：
// illustId 必填（数字）；imageUrls 可缺省，若为数组则元素必须全是 string。
func validateBookmarkSnapshot(m map[string]any) bool {
	if _, ok := m["illustId"].(float64); !ok {
		return false
	}
	urls, ok := m["imageUrls"]
	if !ok || urls == nil {
		return true
	}
	arr, ok := urls.([]any)
	if !ok {
		return false
	}
	for _, u := range arr {
		if _, ok := u.(string); !ok {
			return false
		}
	}
	return true
}
