// Package config 解析并校验环境变量配置。
package config

import "os"

// Config 服务端运行配置。M0 仅含监听端口，后续里程碑按 PLAN.md §10 扩充。
type Config struct {
	Port string
}

// Load 读取环境变量并应用默认值。
func Load() *Config {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	return &Config{Port: port}
}
