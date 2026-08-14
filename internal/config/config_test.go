package config

import "testing"

func TestLoad_Defaults(t *testing.T) {
	cfg := Load()
	if cfg.Port != "8080" {
		t.Fatalf("default port got %q", cfg.Port)
	}
	if cfg.RateWritePerMin != 60 || cfg.RateImgPerMin != 300 || cfg.RateRegisterPerHour != 10 {
		t.Fatalf("default rates got %v/%v/%v", cfg.RateWritePerMin, cfg.RateImgPerMin, cfg.RateRegisterPerHour)
	}
	if cfg.WebDir != "" || cfg.DataEncKey != "" || len(cfg.CORSOrigins) != 0 {
		t.Fatal("web/cors/enc must default to empty (embed SPA, no CORS, no encryption)")
	}
}

func TestLoad_M7Knobs(t *testing.T) {
	t.Setenv("WEB_DIR", " /var/www/arkpix ")
	t.Setenv("CORS_ORIGINS", "https://a.example.com, https://b.example.com ,")
	t.Setenv("DATA_ENC_KEY", " key ")
	t.Setenv("RATE_WRITE_PER_MIN", "120")
	t.Setenv("RATE_IMG_PER_MIN", "-5") // 非法负值 → 回退默认
	cfg := Load()
	if cfg.WebDir != "/var/www/arkpix" {
		t.Fatalf("WebDir got %q", cfg.WebDir)
	}
	if len(cfg.CORSOrigins) != 2 || cfg.CORSOrigins[1] != "https://b.example.com" {
		t.Fatalf("CORSOrigins got %v", cfg.CORSOrigins)
	}
	if cfg.DataEncKey != "key" {
		t.Fatalf("DataEncKey got %q", cfg.DataEncKey)
	}
	if cfg.RateWritePerMin != 120 {
		t.Fatalf("RateWritePerMin got %v", cfg.RateWritePerMin)
	}
	if cfg.RateImgPerMin != 300 {
		t.Fatalf("negative rate must fall back to default, got %v", cfg.RateImgPerMin)
	}
}
