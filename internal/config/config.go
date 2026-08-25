// Package config 解析并校验环境变量配置。
package config

import (
	"net"
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

	// 恢复模块（§8）。RecoverTmpDir 为抓取临时目录，独立于缓存目录（HDD 分卷策略）；
	// RecoverShared 放开恢复产物的跨账号共享（默认按账号隔离，§8.2）。
	RecoverSources         []string
	RecoverTTLDays         int
	RecoverNegativeTTLDays int
	RecoverShared          bool
	RecoverTmpDir          string

	// Web 托管（§6.5）。WebDir 非空时从磁盘目录服务 SPA（前端开发期用），
	// 空 = embed 内嵌产物；CORSOrigins 跨域白名单，空 = 完全关闭跨域（同源部署无需）。
	WebDir      string
	CORSOrigins []string

	// DataEncKey 同步/恢复数据静态加密密钥（§9，AES-256-GCM，base64 编码 32 字节）。
	// 空 = 不加密（向后兼容存量明文）；格式错误启动直接报错（internal/crypto.Load）。
	DataEncKey string

	// AdminToken 管理端 Bearer token（§14.1）。空 = 管理端完全关闭，
	// 不注册任何 /admin/ 路由；与服务账号体系完全解耦。
	AdminToken string

	// 限流配额（§9，次/分钟；注册端点为次/小时）。<= 0 取默认值。
	RateWritePerMin     float64 // 写端点（relay/sync/recover），默认 60
	RateImgPerMin       float64 // 图片端点，默认 300
	RateRegisterPerHour float64 // 注册端点（按 IP），默认 10

	// TrustedProxies 可信反代网段（CIDR，逗号分隔）：直接对端命中时按
	// X-Forwarded-For / X-Real-IP 取真实客户端 IP 限流。回环地址始终可信；
	// Docker 发布端口经 docker-proxy 转发时来源被改写为网关地址，需显式声明。
	TrustedProxies []*net.IPNet
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
	recoverSources := splitCSV(os.Getenv("RECOVER_SOURCES"))
	if len(recoverSources) == 0 {
		recoverSources = []string{"snapshot", "pixiv_cat", "pixiv_re"}
	}
	recoverTmpDir := os.Getenv("RECOVER_TMP_DIR")
	if recoverTmpDir == "" {
		recoverTmpDir = filepath.Join(dataDir, "recover-tmp")
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

		RecoverSources:         recoverSources,
		RecoverTTLDays:         envInt("RECOVER_TTL_DAYS", 90),
		RecoverNegativeTTLDays: envInt("RECOVER_NEGATIVE_TTL_DAYS", 7),
		RecoverShared:          envBool("RECOVER_SHARED"),
		RecoverTmpDir:          recoverTmpDir,

		WebDir:      strings.TrimSpace(os.Getenv("WEB_DIR")),
		CORSOrigins: splitCSV(os.Getenv("CORS_ORIGINS")),
		DataEncKey:  strings.TrimSpace(os.Getenv("DATA_ENC_KEY")),
		AdminToken:  strings.TrimSpace(os.Getenv("ADMIN_TOKEN")),

		RateWritePerMin:     envFloatDef("RATE_WRITE_PER_MIN", 60),
		RateImgPerMin:       envFloatDef("RATE_IMG_PER_MIN", 300),
		RateRegisterPerHour: envFloatDef("RATE_REGISTER_PER_HOUR", 10),
		TrustedProxies:      parseCIDRs(os.Getenv("TRUSTED_PROXIES")),
	}
}

// parseCIDRs 解析逗号分隔的 CIDR 列表（裸 IP 自动补 /32 或 /128），非法项忽略。
func parseCIDRs(s string) []*net.IPNet {
	var out []*net.IPNet
	for _, item := range splitCSV(s) {
		if !strings.Contains(item, "/") {
			ip := net.ParseIP(item)
			if ip == nil {
				continue
			}
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			item = item + "/" + strconv.Itoa(bits)
		}
		if _, n, err := net.ParseCIDR(item); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// envFloatDef 解析正浮点配额环境变量，缺失/非法/<=0 时返回 def（防误配置锁死端点）。
func envFloatDef(name string, def float64) float64 {
	if v := envFloat(name, 0); v > 0 {
		return v
	}
	return def
}

// envBool 解析布尔环境变量（1/true/yes 为真，其余为假）。
func envBool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes":
		return true
	}
	return false
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
