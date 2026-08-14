package sources

import (
	"context"
	"errors"
	"fmt"
)

// Mirror 第三方镜像源（§8.2，如 pixiv.cat / pixiv.re）：
// 按 `https://<host>/<pid>.jpg` 探测首页，再 `<pid>_p1.jpg`、`<pid>_p2.jpg`...
// 递增探测多页，直到连续 2 个 404 或达 50 页上限。每次探测即 GET 拉回，
// 内容校验（image/* 且 > 1 KB）通过后经 cache.Put 落盘。
// host 模板可配置（RECOVER_SOURCES 条目 `name=host` 覆盖默认域名）。
type Mirror struct {
	sourceBase
	name string
	host string
}

// NewMirror 创建镜像源；镜像一般不要求 Referer，需要的源在此声明。
func NewMirror(name, host string, deps Deps) *Mirror {
	return &Mirror{sourceBase: newBase(deps, ""), name: name, host: host}
}

func (m *Mirror) Name() string { return m.name }

// Host 返回镜像域名（供 IMG_EXTRA_HOSTS 启动校验，§6.2）。
func (m *Mirror) Host() string { return m.host }

// Fetch 按 pixiv.cat 规则探测：`<pid>.jpg` 为第 0 页（不存在则整源未找到），
// `<pid>_p<N>.jpg`（N 从 1 起）为后续页，连续 2 个 404 或满 50 页停止。
// 网络/上游异常（非 404）直接失败换下一源，不烧掉整轮探测配额。
func (m *Mirror) Fetch(ctx context.Context, pid string, _ *Snapshot) ([]Page, error) {
	base := "https://" + m.host + "/" + pid

	first := base + ".jpg"
	w, h, err := m.fetchImage(ctx, first)
	if err != nil {
		return nil, err // 含 ErrNotFound：首页不在则该源无此作品
	}
	pages := []Page{{Page: 0, URL: first, Width: w, Height: h}}

	miss := 0
	for n := 1; n < maxPages && miss < 2; n++ {
		u := fmt.Sprintf("%s_p%d.jpg", base, n)
		w, h, err := m.fetchImage(ctx, u)
		switch {
		case err == nil:
			miss = 0
			pages = append(pages, Page{Page: n, URL: u, Width: w, Height: h})
		case errors.Is(err, ErrNotFound):
			miss++
		default:
			return nil, err
		}
	}
	return pages, nil
}
