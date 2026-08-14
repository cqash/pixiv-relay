// Package sources 实现删除图恢复的数据源插件（设计文档 §8.2）：
// 按 RECOVER_SOURCES 配置的优先级组装；抓取并发每源 ≤ 2（信号量）、
// 单源探测间隔 ≥ 1 s（x/time/rate 每源限速器，礼貌策略，参数可注入）；
// 探测到的图片先落独立临时目录（RECOVER_TMP_DIR，与 DB 分卷的 HDD 策略），
// 通过内容校验（Content-Type 须 image/* 且 > 1 KB）后经 cache.Put 落入
// M4 磁盘缓存，全程流式不整读入内存。
package sources

import (
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif"  // DecodeConfig 格式注册
	_ "image/jpeg" // DecodeConfig 格式注册
	_ "image/png"  // DecodeConfig 格式注册
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/arkpix/relay/internal/cache"
)

const (
	// DefaultProbeEvery 单源探测间隔（§8.2：≥ 1 s 礼貌策略）。
	DefaultProbeEvery = time.Second
	// DefaultMaxConcurrent 每源并发上限（§8.2：≤ 2）。
	DefaultMaxConcurrent = 2
	// minImageBytes 内容校验下限：小于 1 KB 的响应不可能是有效原图。
	minImageBytes int64 = 1024
	// maxPages 单作品页数探测上限（§8.2：50 页）。
	maxPages = 50

	pixivUA      = "PixivIOSApp/5.8.0"
	pixivReferer = "https://app-api.pixiv.net/"
)

// ErrNotFound 源上不存在该作品（或内容校验失败），区别于网络/上游异常。
var ErrNotFound = errors.New("sources: not found")

// Page 恢复产物单页。URL 为第三方源原始地址（响应时包装 /img/v1/fetch）；
// Width/Height 尽力从图片头解码，失败为 0。
type Page struct {
	Page   int    `json:"page"`
	URL    string `json:"url"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// Snapshot bookmark_snapshot 同步域中该 pid 的快照数据（§7.3）。
// Meta 为快照元数据（title/userName 等，已剔除 imageUrls），随恢复结果透传给客户端。
type Snapshot struct {
	ImageURLs []string
	Width     int
	Height    int
	Meta      map[string]any
}

// Source 恢复数据源。Fetch 成功返回已落入磁盘缓存的页面列表；
// 源上无此作品返回 ErrNotFound，其余错误为网络/上游异常（均换下一源重试）。
type Source interface {
	Name() string
	Fetch(ctx context.Context, pid string, snap *Snapshot) ([]Page, error)
}

// Deps 源公共依赖。Client 为出网客户端（生产用 relay.NewUpstreamClient，
// UPSTREAM_PROXY 生效；测试注入改写 Transport 到 httptest）。
type Deps struct {
	Client        *http.Client
	Cache         *cache.DiskLRU
	TmpDir        string        // 抓取临时目录（RECOVER_TMP_DIR）
	ProbeEvery    time.Duration // 单源探测间隔，零值取 DefaultProbeEvery
	MaxConcurrent int           // 每源并发上限，零值取 DefaultMaxConcurrent
}

// Build 按 RECOVER_SOURCES 条目顺序组装源列表（优先级 = 配置顺序）。
// 条目支持 `name` 或 `name=host`（覆盖镜像源默认域名，如 pixiv_cat=i.pixiv.cat）。
// 返回源列表与镜像源域名清单（供 IMG_EXTRA_HOSTS 启动校验，§6.2）。
func Build(names []string, deps Deps) ([]Source, []string) {
	var out []Source
	var hosts []string
	for _, entry := range names {
		name, host, _ := strings.Cut(entry, "=")
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "":
		case "snapshot":
			out = append(out, NewSnapshot(deps))
		case "pixiv_cat":
			if host == "" {
				host = "i.pixiv.cat"
			}
			out = append(out, NewMirror("pixiv_cat", host, deps))
			hosts = append(hosts, host)
		case "pixiv_re":
			if host == "" {
				host = "i.pixiv.re"
			}
			out = append(out, NewMirror("pixiv_re", host, deps))
			hosts = append(hosts, host)
		default:
			slog.Warn("unknown recover source ignored", "source", name)
		}
	}
	return out, hosts
}

// sourceBase 源公共基建：每源限速器 + 并发信号量 + 探测/下载/落盘。
// referer 为源声明的出网 Referer（§8.2：需要 Pixiv 风格 Referer 的源自行声明）。
type sourceBase struct {
	deps    Deps
	lim     *rate.Limiter
	sem     chan struct{}
	referer string
}

func newBase(deps Deps, referer string) sourceBase {
	every := deps.ProbeEvery
	if every <= 0 {
		every = DefaultProbeEvery
	}
	mc := deps.MaxConcurrent
	if mc <= 0 {
		mc = DefaultMaxConcurrent
	}
	return sourceBase{
		deps:    deps,
		lim:     rate.NewLimiter(rate.Every(every), 1),
		sem:     make(chan struct{}, mc),
		referer: referer,
	}
}

// acquire 出网前取限速令牌与并发配额（探测间隔 + 每源并发约束在此统一生效）。
func (b *sourceBase) acquire(ctx context.Context) (release func(), err error) {
	if err := b.lim.Wait(ctx); err != nil {
		return nil, err
	}
	select {
	case b.sem <- struct{}{}:
		return func() { <-b.sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// do 发出网请求（统一注入 Referer/UA；调用方负责 Close 响应体）。
func (b *sourceBase) do(ctx context.Context, method, rawURL, rangeHeader string) (*http.Response, error) {
	release, err := b.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("sources: build request: %w", err)
	}
	if b.referer != "" {
		req.Header.Set("Referer", b.referer)
	}
	req.Header.Set("User-Agent", pixivUA)
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	resp, err := b.deps.Client.Do(req)
	if err != nil {
		// err 含 URL，绝不进日志（沿用 §6.1 约定）。
		return nil, fmt.Errorf("sources: request: %w", err)
	}
	return resp, nil
}

// probeAlive 探测 URL 是否仍在 CDN 存活期：HEAD 优先，
// 上游不支持（405/501）时退化 GET Range bytes=0-0（§8.2 快照源探测）。
func (b *sourceBase) probeAlive(ctx context.Context, rawURL string) bool {
	for _, p := range []struct {
		method string
		rangeH string
	}{
		{http.MethodHead, ""},
		{http.MethodGet, "bytes=0-0"},
	} {
		resp, err := b.do(ctx, p.method, rawURL, p.rangeH)
		if err != nil {
			return false
		}
		code := resp.StatusCode
		ct := resp.Header.Get("Content-Type")
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if p.method == http.MethodHead &&
			(code == http.StatusMethodNotAllowed || code == http.StatusNotImplemented) {
			continue
		}
		alive := code == http.StatusOK || code == http.StatusPartialContent
		return alive && strings.HasPrefix(strings.ToLower(ct), "image/")
	}
	return false
}

// fetchImage 探测并拉取一张图落盘：缓存命中（键 = URL SHA-256，跨用户共享，
// §8.2 可见性分层）直接返回；否则限速出网 GET，校验 Content-Type 为 image/*
// 且 > 1 KB，先流式落恢复临时目录再 cache.Put 原子入 M4 缓存。
// 上游 404 / 内容校验失败返回 ErrNotFound；返回值为解码尺寸（失败为 0）。
func (b *sourceBase) fetchImage(ctx context.Context, rawURL string) (w, h int, err error) {
	key := cache.Key(rawURL)
	if f, _, gerr := b.deps.Cache.Get(ctx, key); gerr == nil && f != nil {
		defer func() { _ = f.Close() }()
		w, h, _ := decodeDims(f)
		return w, h, nil
	}

	resp, err := b.do(ctx, http.MethodGet, rawURL, "")
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return 0, 0, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return 0, 0, fmt.Errorf("sources: upstream status %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(strings.ToLower(ct), "image/") {
		return 0, 0, ErrNotFound // 内容校验：非图片按未找到处理
	}

	// 先落恢复临时目录（独立于缓存目录，HDD 分卷策略），校验通过后再入缓存。
	tmp, err := os.CreateTemp(b.deps.TmpDir, "rc-*")
	if err != nil {
		return 0, 0, fmt.Errorf("sources: create temp: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()

	maxFile := b.deps.Cache.Config().MaxFileBytes
	n, err := io.Copy(tmp, io.LimitReader(resp.Body, maxFile+1))
	if err != nil {
		_ = tmp.Close()
		return 0, 0, fmt.Errorf("sources: download: %w", err)
	}
	if n > maxFile || n <= minImageBytes {
		_ = tmp.Close()
		return 0, 0, ErrNotFound // 尺寸校验失败：超缓存上限或小于 1 KB
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		_ = tmp.Close()
		return 0, 0, fmt.Errorf("sources: rewind temp: %w", err)
	}
	w, h, _ = decodeDims(tmp)
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		_ = tmp.Close()
		return 0, 0, fmt.Errorf("sources: rewind temp: %w", err)
	}
	if _, err := b.deps.Cache.Put(ctx, key, ct, tmp); err != nil {
		_ = tmp.Close()
		return 0, 0, fmt.Errorf("sources: cache put: %w", err)
	}
	_ = tmp.Close()
	return w, h, nil
}

// decodeDims 尽力从图片头解码尺寸（jpeg/png/gif），失败返回 0。
func decodeDims(r io.Reader) (int, int, bool) {
	cfg, _, err := image.DecodeConfig(r)
	if err != nil {
		return 0, 0, false
	}
	return cfg.Width, cfg.Height, true
}
