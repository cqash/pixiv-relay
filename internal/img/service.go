// Package img 实现 /img 图片中继（设计文档 §6.2）：
// 域名白名单、Referer/UA 注入、响应头透传、磁盘 LRU 缓存（§6.4 流式读写）。
package img

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/arkpix/relay/internal/cache"
	"github.com/arkpix/relay/internal/common"
)

const (
	// pixivReferer / pixivUA 出网时注入（pixiv 图片 CDN 校验 Referer，§6.2）。
	pixivReferer = "https://app-api.pixiv.net/"
	pixivUA      = "PixivIOSApp/5.8.0"
)

// defaultAllowedHosts 图片域名白名单（§6.2）；IMG_EXTRA_HOSTS 可追加
// （恢复模块第三方源域名必须显式声明，默认不放开任意域名防开放代理滥用）。
var defaultAllowedHosts = []string{
	"i.pximg.net",
	"i-f.pximg.net",
	"s.pximg.net",
}

// flight 并发同 key 去重：leader 出网，follower 等待完成后重查缓存。
type flight struct {
	done chan struct{}
}

// Service 图片中继服务。client 可注入：测试用自定义 Transport 把白名单域名
// 改写到 httptest 上游，生产复用 relay.NewUpstreamClient。
type Service struct {
	client *http.Client
	hosts  map[string]bool
	cache  *cache.DiskLRU

	mu       sync.Mutex
	inflight map[string]*flight
}

// NewService 创建图片中继服务；extraHosts 追加进域名白名单（IMG_EXTRA_HOSTS）。
func NewService(client *http.Client, c *cache.DiskLRU, extraHosts []string) *Service {
	hosts := make(map[string]bool, len(defaultAllowedHosts)+len(extraHosts))
	for _, h := range defaultAllowedHosts {
		hosts[h] = true
	}
	for _, h := range extraHosts {
		hosts[strings.ToLower(h)] = true
	}
	return &Service{
		client:   client,
		hosts:    hosts,
		cache:    c,
		inflight: make(map[string]*flight),
	}
}

// Fetch 处理 GET /img/v1/fetch?url=&disposition=。
func (s *Service) Fetch(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	rawURL := r.URL.Query().Get("url")
	if rawURL == "" {
		common.WriteError(w, r, common.BadRequest("url is required"))
		return
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		common.WriteError(w, r, common.BadRequest("invalid url"))
		return
	}
	if !strings.EqualFold(u.Scheme, "https") {
		common.WriteError(w, r, common.BadRequest("url scheme must be https"))
		return
	}
	host := strings.ToLower(u.Hostname())
	if !s.hosts[host] {
		common.WriteError(w, r, common.Forbidden("host not allowed: "+host))
		return
	}
	inline := r.URL.Query().Get("disposition") == "inline"
	key := cache.Key(rawURL)

	// 并发同 key 去重：leader 出网并落缓存，follower 等待后重查缓存；
	// 仍未命中（leader 失败）则由 follower 递补为 leader 重试一轮。
	for {
		if s.serveHit(w, r, key, inline, host, start) {
			return
		}
		f, leader := s.enterFlight(key)
		if !leader {
			<-f.done
			continue
		}
		s.fetchUpstream(w, r, rawURL, key, inline, host, start)
		s.leaveFlight(key, f)
		return
	}
}

// serveHit 尝试从缓存服务响应；命中返回 true。HIT 路径流式直出，不整读。
func (s *Service) serveHit(w http.ResponseWriter, r *http.Request, key string, inline bool, host string, start time.Time) bool {
	f, meta, err := s.cache.Get(r.Context(), key)
	if err != nil {
		// 缓存读取异常按未命中降级（直连上游兜底），不返回错误码。
		slog.WarnContext(r.Context(), "cache read degraded")
		return false
	}
	if f == nil {
		return false
	}
	defer func() { _ = f.Close() }()

	if meta.ContentType != "" {
		w.Header().Set("Content-Type", meta.ContentType)
	}
	w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
	w.Header().Set("X-Cache", "HIT")
	if inline {
		w.Header().Set("Content-Disposition", "inline")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)

	s.log(r.Context(), host, http.StatusOK, "HIT", start)
	return true
}

// fetchUpstream 出网拉取并透传；200 时边下边存（tee 到缓存临时文件，
// 写完 rename 落盘）。失败映射（§4.2）：上游 404 → 404，其余非 200 / 超时 /
// 连接失败 → 502；磁盘缓存写失败仅降级不缓存，不影响透传。
func (s *Service) fetchUpstream(w http.ResponseWriter, r *http.Request, rawURL, key string, inline bool, host string, start time.Time) {
	ctx := r.Context()
	upReq, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		common.WriteError(w, r, common.BadRequest("invalid url"))
		return
	}
	upReq.Header.Set("Referer", pixivReferer)
	upReq.Header.Set("User-Agent", pixivUA)

	upResp, err := s.client.Do(upReq)
	if err != nil {
		// err 含 URL，绝不进日志（§6.1 约定沿用）。
		s.log(ctx, host, http.StatusBadGateway, "MISS", start)
		common.WriteError(w, r, common.BadGateway("upstream unreachable"))
		return
	}
	defer func() { _ = upResp.Body.Close() }()

	w.Header().Set("X-Upstream-Status", strconv.Itoa(upResp.StatusCode))
	switch {
	case upResp.StatusCode == http.StatusNotFound:
		s.log(ctx, host, http.StatusNotFound, "MISS", start)
		common.WriteError(w, r, common.NotFound("image not found"))
		return
	case upResp.StatusCode != http.StatusOK:
		// 其余非 200 统一 502（§4.2：502 = 上游不可达/异常），不入缓存。
		s.log(ctx, host, upResp.StatusCode, "MISS", start)
		common.WriteError(w, r, common.BadGateway("upstream returned status "+strconv.Itoa(upResp.StatusCode)))
		return
	}

	for _, h := range []string{"Content-Type", "Content-Length", "Cache-Control"} {
		if v := upResp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.Header().Set("X-Cache", "MISS")
	if inline {
		w.Header().Set("Content-Disposition", "inline")
	}

	// 已知长度超单图上限 → 直连透传不缓存；未知长度走 tee，超限由 Writer 降级兜底。
	var cw *cache.Writer
	if upResp.ContentLength < 0 || upResp.ContentLength <= s.cache.Config().MaxFileBytes {
		var err error
		cw, err = s.cache.NewWriter(key, upResp.Header.Get("Content-Type"))
		if err != nil {
			slog.WarnContext(ctx, "cache writer degraded")
		}
	}

	w.WriteHeader(http.StatusOK)
	if cw == nil {
		_, _ = io.Copy(w, upResp.Body)
	} else {
		// TeeReader 单趟流式：边透传给客户端边写临时文件，全程不整读（HDD 策略）。
		_, _ = io.Copy(w, io.TeeReader(upResp.Body, cw))
		if _, err := cw.Commit(ctx); err != nil {
			// 落盘失败仅降级不缓存（不返回错误码，§4.2），透传内容不受影响。
			slog.WarnContext(ctx, "cache commit degraded")
		}
	}

	s.log(ctx, host, upResp.StatusCode, "MISS", start)
}

// enterFlight 进入同 key 去重：首个请求成为 leader，其余拿到既有 flight 等待。
func (s *Service) enterFlight(key string) (*flight, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f, ok := s.inflight[key]; ok {
		return f, false
	}
	f := &flight{done: make(chan struct{})}
	s.inflight[key] = f
	return f, true
}

func (s *Service) leaveFlight(key string, f *flight) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inflight, key)
	close(f.done)
}

// log 图片中继访问日志：仅 host + 状态码 + 命中 + 耗时 + requestId，
// 绝不落完整 URL（含作品路径，半敏感，§6.2 / §9）。
func (s *Service) log(ctx context.Context, host string, status int, hit string, start time.Time) {
	slog.InfoContext(ctx, "img",
		"host", host,
		"status", status,
		"cache", hit,
		"durMs", time.Since(start).Milliseconds(),
		"requestId", common.RequestIDFrom(ctx),
	)
}
