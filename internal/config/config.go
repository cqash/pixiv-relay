// Package config 解析并校验环境变量配置。
package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config 服务端运行配置。M2 含数据目录 / DB 路径与认证开关，
// 后续里程碑按 PLAN.md §10 扩充（缓存、代理、恢复源等）。
type Config struct {
	Port string

	DataDir string
	DBPath  string

	// InviteCodes 注册邀请码（§5.1）；空 = 开放注册（仅私有网络）。
	// StaticTokens 预置 token（§5.1），映射到内置共享账号、跳过注册流程。
	// 公网部署必须配置二者之一（§9）。
	InviteCodes  []string
	StaticTokens []string

	// RelayExtraHosts /relay 域名白名单追加项（§6.1）。
	// UpstreamProxy 上游出口代理（§10），空 = 直连。
	RelayExtraHosts []string
	UpstreamProxy   string

	// ImgExtraHosts /img 域名白名单追加项（§6.2，恢复模块第三方源在此声明）。
	ImgExtraHosts []string

	// 图片磁盘缓存（§6.4 HDD 策略）。CacheTmpDir 必须与 CacheDir 同卷（rename 原子落盘）。
	CacheDir           string
	CacheTmpDir        string
	CacheMaxBytes      int64
	CacheLayout        string
	CacheHighWatermark float64
	CacheEvictionBatch int
}

// Load 读取环境变量并应用默认值。
func Load() *Config {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "./data"
	}
	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = filepath.Join(dataDir, "relay.db")
	}
	cacheDir := os.Getenv("CACHE_DIR")
	if cacheDir == "" {
		cacheDir = filepath.Join(dataDir, "cache")
	}
	return &Config{
		Port:               port,
		DataDir:            dataDir,
		DBPath:             dbPath,
		InviteCodes:        splitCSV(os.Getenv("INVITE_CODES")),
		StaticTokens:       splitCSV(os.Getenv("STATIC_TOKENS")),
		RelayExtraHosts:    splitCSV(os.Getenv("RELAY_EXTRA_HOSTS")),
		UpstreamProxy:      strings.TrimSpace(os.Getenv("UPSTREAM_PROXY")),
		ImgExtraHosts:      splitCSV(os.Getenv("IMG_EXTRA_HOSTS")),
		CacheDir:           cacheDir,
		CacheTmpDir:        os.Getenv("CACHE_TMP_DIR"), // 空 = <CACHE_DIR>/tmp（cache.Open 规范化）
		CacheMaxBytes:      envInt64("CACHE_MAX_BYTES", 0),
		CacheLayout:        os.Getenv("CACHE_LAYOUT"),
		CacheHighWatermark: envFloat("CACHE_HIGH_WATERMARK", 0),
		CacheEvictionBatch: envInt("CACHE_EVICTION_BATCH", 0),
	}
}

// envInt64 解析整数环境变量，缺失或非法时返回 def。
func envInt64(name string, def int64) int64 {
	v, err := strconv.ParseInt(strings.TrimSpace(os.Getenv(name)), 10, 64)
	if err != nil {
		return def
	}
	return v
}

func envInt(name string, def int) int {
	return int(envInt64(name, int64(def)))
}

func envFloat(name string, def float64) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv(name)), 64)
	if err != nil {
		return def
	}
	return v
}

// splitCSV 解析逗号分隔配置项，去除空白项。
func splitCSV(s string) []string {
	var out []string
	for _, item := range strings.Split(s, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}
