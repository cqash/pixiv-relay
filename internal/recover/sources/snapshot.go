package sources

import (
	"context"
)

// SnapshotSource 快照源（§8.2 第一优先级）：使用客户端 push 的收藏快照里
// 保存的原图 URL（可能仍在 CDN 存活期）。逐 URL HEAD/Range 探测存活，
// 存活的拉回落盘；出网带 Pixiv 风格 Referer（pximg CDN 校验，§6.2）。
type SnapshotSource struct {
	sourceBase
}

// NewSnapshot 创建快照源。
func NewSnapshot(deps Deps) *SnapshotSource {
	return &SnapshotSource{sourceBase: newBase(deps, pixivReferer)}
}

func (s *SnapshotSource) Name() string { return "snapshot" }

// Fetch 从快照恢复：无快照或全部 URL 已失效返回 ErrNotFound。
// 单页探测后下载失败按该页失效处理（竞态：HEAD 存活但 GET 时已过期）。
func (s *SnapshotSource) Fetch(ctx context.Context, _ string, snap *Snapshot) ([]Page, error) {
	if snap == nil || len(snap.ImageURLs) == 0 {
		return nil, ErrNotFound
	}
	var pages []Page
	for i, u := range snap.ImageURLs {
		if !s.probeAlive(ctx, u) {
			continue
		}
		w, h, err := s.fetchImage(ctx, u)
		if err != nil {
			continue
		}
		if w == 0 {
			w = snap.Width // 解码失败回退快照尺寸
		}
		if h == 0 {
			h = snap.Height
		}
		pages = append(pages, Page{Page: i, URL: u, Width: w, Height: h})
	}
	if len(pages) == 0 {
		return nil, ErrNotFound
	}
	return pages, nil
}
