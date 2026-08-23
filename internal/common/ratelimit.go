package common

import (
	"math"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Limiter 按 key 的令牌桶限流（§9）。空转超过 ttl 的桶惰性回收。
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	r       rate.Limit
	burst   int
	ttl     time.Duration
}

type bucket struct {
	lim  *rate.Limiter
	last time.Time
}

// NewLimiter 创建限流器：perMinute 次/分钟，burst 为桶容量（瞬时突增上限）。
func NewLimiter(perMinute float64, burst int) *Limiter {
	return &Limiter{
		buckets: make(map[string]*bucket),
		r:       rate.Limit(perMinute / 60),
		burst:   burst,
		ttl:     10 * time.Minute,
	}
}

// allow 尝试取一个令牌；拒绝时返回 Retry-After 秒数（按补一个令牌所需时间估算）。
func (l *Limiter) allow(key string) (ok bool, retryAfterSec int) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if len(l.buckets) > 1024 {
		l.sweep()
	}
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{lim: rate.NewLimiter(l.r, l.burst)}
		l.buckets[key] = b
	}
	b.last = time.Now()
	if b.lim.Allow() {
		return true, 0
	}
	retry := int(math.Ceil(1 / float64(l.r)))
	if retry < 1 {
		retry = 1
	}
	return false, retry
}

// SetRate 热更新速率与桶容量（管理端 §14.2）：更新默认参数并同步调整存量桶，
// 立即全量生效。
func (l *Limiter) SetRate(perMinute float64, burst int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.r = rate.Limit(perMinute / 60)
	l.burst = burst
	for _, b := range l.buckets {
		b.lim.SetLimit(l.r)
		b.lim.SetBurst(burst)
	}
}

// sweep 回收空转桶，调用方需持锁。
func (l *Limiter) sweep() {
	cutoff := time.Now().Add(-l.ttl)
	for k, b := range l.buckets {
		if b.last.Before(cutoff) {
			delete(l.buckets, k)
		}
	}
}

// Middleware 返回限流中间件；keyFn 取限流键（IP / accountID / token）。
// 拒绝时 429 + Retry-After 头 + 统一错误体（§4.2）。
func (l *Limiter) Middleware(keyFn func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ok, retry := l.allow(keyFn(r))
			if !ok {
				w.Header().Set("Retry-After", strconv.Itoa(retry))
				WriteError(w, r, RateLimited())
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ClientIP 取客户端 IP（直连部署）。反代部署待 M7 再按可信代理头扩展。
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
