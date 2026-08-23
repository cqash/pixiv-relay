package admin

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/arkpix/relay/internal/cache"
	"github.com/arkpix/relay/internal/common"
)

// 可热改设置键白名单（§14.2）。生效优先级：DB 覆盖 > 环境变量 > 内置默认。
const (
	KeyCacheMaxBytes      = "cache_max_bytes"
	KeyCacheHighWatermark = "cache_high_watermark"
	KeyRecoverTTLDays     = "recover_ttl_days"
	KeyRecoverNegTTLDays  = "recover_negative_ttl_days"
	KeyRateWritePerMin    = "rate_write_per_min"
	KeyRateImgPerMin      = "rate_img_per_min"
)

// 限流器 burst 常量（与 app 层构造保持一致）。
const (
	writeBurst = 10
	imgBurst   = 30
)

// settingDef 单个可热改键的定义：env 名、内置默认值、文本解析校验。
// 解析函数同时用于 DB 覆盖项、环境变量与 PATCH 提交值（统一文本形式）。
type settingDef struct {
	envName string
	def     any // int64 或 float64
	parse   func(raw string) (any, error)
}

// parsePositiveInt 解析正整数（>0）。
func parsePositiveInt(raw string) (any, error) {
	v, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || v <= 0 {
		return nil, fmt.Errorf("must be a positive integer")
	}
	return v, nil
}

// parseWatermark 解析淘汰水位（0<x<1）。
func parseWatermark(raw string) (any, error) {
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || v <= 0 || v >= 1 {
		return nil, fmt.Errorf("must be a float in (0, 1)")
	}
	return v, nil
}

var settingDefs = map[string]settingDef{
	KeyCacheMaxBytes:      {"CACHE_MAX_BYTES", cache.DefaultMaxBytes, parsePositiveInt},
	KeyCacheHighWatermark: {"CACHE_HIGH_WATERMARK", cache.DefaultHighWatermark, parseWatermark},
	KeyRecoverTTLDays:     {"RECOVER_TTL_DAYS", int64(90), parsePositiveInt},
	KeyRecoverNegTTLDays:  {"RECOVER_NEGATIVE_TTL_DAYS", int64(7), parsePositiveInt},
	KeyRateWritePerMin:    {"RATE_WRITE_PER_MIN", int64(60), parsePositiveInt},
	KeyRateImgPerMin:      {"RATE_IMG_PER_MIN", int64(300), parsePositiveInt},
}

// SettingInfo GET 响应项：生效值 + 来源（db/env/default）。
type SettingInfo struct {
	Value  any    `json:"value"`
	Source string `json:"source"`
}

// EnvSnapshot 启动时的环境变量快照（仅含已设置且解析合法的项）。
type EnvSnapshot struct {
	values map[string]any
}

// EnvSnapshotFromEnv 读取六个可热改键对应的环境变量，构造 env 层快照。
// 非法值按未设置处理（与 config 包"非法回落默认"语义一致）。
func EnvSnapshotFromEnv() EnvSnapshot {
	snap := EnvSnapshot{values: make(map[string]any)}
	for key, def := range settingDefs {
		raw, ok := os.LookupEnv(def.envName)
		if !ok {
			continue
		}
		if v, err := def.parse(raw); err == nil {
			snap.values[key] = v
		}
	}
	return snap
}

// resolve 解析单个键的生效值与来源：DB > env > 默认。
// DB 内损坏值告警后按不存在回落。
func (s *Service) resolve(ctx context.Context, key string) (any, string) {
	def := settingDefs[key]
	var raw string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", key).Scan(&raw)
	if err == nil {
		if v, perr := def.parse(raw); perr == nil {
			return v, "db"
		}
		slog.WarnContext(ctx, "admin: invalid setting in db, falling back", "key", key)
	}
	if v, ok := s.env.values[key]; ok {
		return v, "env"
	}
	return def.def, "default"
}

// resolveAll 解析全部白名单键的生效值。
func (s *Service) resolveAll(ctx context.Context) map[string]SettingInfo {
	out := make(map[string]SettingInfo, len(settingDefs))
	for key := range settingDefs {
		v, src := s.resolve(ctx, key)
		out[key] = SettingInfo{Value: v, Source: src}
	}
	return out
}

// applyAll 按当前生效值调用全部热更挂钩（启动加载 DB 覆盖项 / PATCH 后立即生效）。
func (s *Service) applyAll(ctx context.Context) {
	m := s.resolveAll(ctx)
	s.cache.SetLimits(
		m[KeyCacheMaxBytes].Value.(int64),
		m[KeyCacheHighWatermark].Value.(float64))
	s.recoverSvc.SetTTLDays(
		int(m[KeyRecoverTTLDays].Value.(int64)),
		int(m[KeyRecoverNegTTLDays].Value.(int64)))
	s.writeLimiter.SetRate(float64(m[KeyRateWritePerMin].Value.(int64)), writeBurst)
	s.imgLimiter.SetRate(float64(m[KeyRateImgPerMin].Value.(int64)), imgBurst)
}

// validatePatch 校验 PATCH 提交的部分键值：未知键/非法值返回 400，
// 全部通过返回规范化的文本形式（待落库）。
func validatePatch(patch map[string]string) (map[string]string, error) {
	normalized := make(map[string]string, len(patch))
	for key, raw := range patch {
		def, ok := settingDefs[key]
		if !ok {
			return nil, common.BadRequest("unknown setting key: " + key)
		}
		if _, err := def.parse(raw); err != nil {
			return nil, common.BadRequest("invalid value for " + key + ": " + err.Error())
		}
		normalized[key] = strings.TrimSpace(raw)
	}
	return normalized, nil
}

// persistSettings 事务内落库全部覆盖项（updated_at 毫秒，§4.1）。
func (s *Service) persistSettings(ctx context.Context, kv map[string]string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin settings tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UnixMilli()
	for key, raw := range kv {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?) "+
				"ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at",
			key, raw, now); err != nil {
			return fmt.Errorf("upsert setting %s: %w", key, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit settings tx: %w", err)
	}
	return nil
}
