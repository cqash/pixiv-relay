// Package relay 实现 /relay 网络中继（设计文档 §6.1）：
// 域名/头白名单、body 与超时钳制、UA 注入、UPSTREAM_PROXY 出网。
package relay

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/arkpix/relay/internal/common"
)

const (
	// defaultUA 客户端未传 User-Agent 时注入（Pixiv 服务端要求，§6.1）。
	defaultUA = "PixivIOSApp/5.8.0"
	// maxBodyBytes 请求/响应 body 上限 1 MB（§6.1）。
	maxBodyBytes     = 1 << 20
	defaultTimeoutMs = 30000
	maxTimeoutMs     = 60000
)

// defaultAllowedHosts 域名白名单（§6.1）；RELAY_EXTRA_HOSTS 可追加。
var defaultAllowedHosts = []string{
	"app-api.pixiv.net",
	"oauth.secure.pixiv.net",
	"www.pixiv.net",
}

// forwardHeaders 请求头转发白名单（小写，大小写不敏感匹配），其余丢弃（§6.1）。
var forwardHeaders = map[string]bool{
	"authorization":   true,
	"accept-language": true,
	"content-type":    true,
	"user-agent":      true,
	"referer":         true,
}

// passthroughHeaders 响应头透传白名单（§6.1），响应体中一律小写键。
var passthroughHeaders = []string{"content-type", "cache-control", "retry-after"}

// allowedMethods 允许中继的方法（§6.1 客户端仅需这四种）。
var allowedMethods = map[string]bool{
	http.MethodGet:    true,
	http.MethodPost:   true,
	http.MethodPut:    true,
	http.MethodDelete: true,
}

// Request /relay/v1/request 请求体（§6.1）。
type Request struct {
	Method     string            `json:"method"`
	URL        string            `json:"url"`
	Headers    map[string]string `json:"headers"`
	BodyBase64 string            `json:"bodyBase64"`
	TimeoutMs  int               `json:"timeoutMs"`
}

// Response 中继响应：status + 白名单内响应头 + bodyBase64（§6.1）。
type Response struct {
	Status     int               `json:"status"`
	Headers    map[string]string `json:"headers"`
	BodyBase64 string            `json:"bodyBase64"`
}

// Service 中继服务。client 可注入：测试用自定义 Transport 把白名单域名改写到
// httptest 上游，生产用 NewUpstreamClient。
type Service struct {
	client *http.Client
	hosts  map[string]bool
}

// NewService 创建中继服务；extraHosts 追加进域名白名单（RELAY_EXTRA_HOSTS）。
func NewService(client *http.Client, extraHosts []string) *Service {
	hosts := make(map[string]bool, len(defaultAllowedHosts)+len(extraHosts))
	for _, h := range defaultAllowedHosts {
		hosts[h] = true
	}
	for _, h := range extraHosts {
		hosts[strings.ToLower(h)] = true
	}
	return &Service{client: client, hosts: hosts}
}

// NewUpstreamClient 构建出网 http.Client（自定义 Transport）；
// proxyURL 非空时经该代理出网（UPSTREAM_PROXY，§10）。
func NewUpstreamClient(proxyURL string) (*http.Client, error) {
	tr := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	if proxyURL != "" {
		u, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("parse UPSTREAM_PROXY: %w", err)
		}
		tr.Proxy = http.ProxyURL(u)
	}
	return &http.Client{Transport: tr}, nil
}

// clampTimeout 钳制超时：缺省 30 s，上限 60 s（§6.1）。
func clampTimeout(ms int) time.Duration {
	if ms <= 0 {
		ms = defaultTimeoutMs
	}
	if ms > maxTimeoutMs {
		ms = maxTimeoutMs
	}
	return time.Duration(ms) * time.Millisecond
}

// Do 执行一次中继：校验 → 头过滤 → 出网 → 透传响应。
// 返回的 error 一律为 *common.APIError（400 参数 / 403 域名 / 502 上游）。
func (s *Service) Do(ctx context.Context, req *Request) (*Response, error) {
	start := time.Now()

	method := strings.ToUpper(req.Method)
	if !allowedMethods[method] {
		return nil, common.BadRequest("method not allowed: " + req.Method)
	}
	u, err := url.Parse(req.URL)
	if err != nil || u.Host == "" {
		return nil, common.BadRequest("invalid url")
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return nil, common.BadRequest("url scheme must be https")
	}
	host := strings.ToLower(u.Hostname())
	if !s.hosts[host] {
		return nil, common.Forbidden("host not allowed: " + host)
	}

	var body io.Reader
	if req.BodyBase64 != "" {
		raw, err := base64.StdEncoding.DecodeString(req.BodyBase64)
		if err != nil {
			return nil, common.BadRequest("bodyBase64 decode failed")
		}
		if len(raw) > maxBodyBytes {
			return nil, common.BadRequest("body exceeds 1MB limit")
		}
		body = bytes.NewReader(raw)
	}

	ctx, cancel := context.WithTimeout(ctx, clampTimeout(req.TimeoutMs))
	defer cancel()

	upReq, err := http.NewRequestWithContext(ctx, method, req.URL, body)
	if err != nil {
		return nil, common.BadRequest("invalid url")
	}
	hasUA := false
	for k, v := range req.Headers {
		lk := strings.ToLower(k)
		if !forwardHeaders[lk] {
			continue
		}
		if lk == "user-agent" && v != "" {
			hasUA = true
		}
		upReq.Header.Set(k, v)
	}
	if !hasUA {
		upReq.Header.Set("User-Agent", defaultUA)
	}

	upResp, err := s.client.Do(upReq)
	if err != nil {
		// 连接失败/超时统一 502（§4.2）；err 含 URL query，绝不进日志（§6.1）。
		s.log(ctx, method, host, http.StatusBadGateway, start)
		return nil, common.BadGateway("upstream unreachable")
	}
	defer upResp.Body.Close()

	// 流式读但限 1 MB：多读 1 字节判断是否超限（HDD 策略：禁止整读入内存）。
	raw, err := io.ReadAll(io.LimitReader(upResp.Body, maxBodyBytes+1))
	if err != nil {
		s.log(ctx, method, host, http.StatusBadGateway, start)
		return nil, common.BadGateway("upstream response read failed")
	}
	if len(raw) > maxBodyBytes {
		s.log(ctx, method, host, http.StatusBadGateway, start)
		return nil, common.BadGateway("upstream response exceeds 1MB limit")
	}

	s.log(ctx, method, host, upResp.StatusCode, start)

	headers := make(map[string]string, len(passthroughHeaders))
	for _, k := range passthroughHeaders {
		if v := upResp.Header.Get(k); v != "" {
			headers[k] = v
		}
	}
	return &Response{
		Status:     upResp.StatusCode,
		Headers:    headers,
		BodyBase64: base64.StdEncoding.EncodeToString(raw),
	}, nil
}

// log 中继访问日志：仅 method + 上游 host + 状态码 + 耗时 + requestId，
// 绝不落 URL query / headers / body（§6.1、§9）。
func (s *Service) log(ctx context.Context, method, host string, status int, start time.Time) {
	slog.InfoContext(ctx, "relay",
		"method", method,
		"host", host,
		"status", status,
		"durMs", time.Since(start).Milliseconds(),
		"requestId", common.RequestIDFrom(ctx),
	)
}
