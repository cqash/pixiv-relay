// Package config 解析并校验环境变量配置。
package config

import (
	"os"
	"path/filepath"
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
	return &Config{
		Port:            port,
		DataDir:         dataDir,
		DBPath:          dbPath,
		InviteCodes:     splitCSV(os.Getenv("INVITE_CODES")),
		StaticTokens:    splitCSV(os.Getenv("STATIC_TOKENS")),
		RelayExtraHosts: splitCSV(os.Getenv("RELAY_EXTRA_HOSTS")),
		UpstreamProxy:   strings.TrimSpace(os.Getenv("UPSTREAM_PROXY")),
	}
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
