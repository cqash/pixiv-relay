package web

import (
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"strings"
	"time"
)

// staticCacheControl 静态资源缓存策略（§6.5）：内容按文件名指纹可由前端工程控制，
// 这里给一天保守缓存；index.html 不缓存，保证发版即时生效。
const staticCacheControl = "public, max-age=86400"

// contentTypes 常见静态资源 Content-Type 兜底表（不依赖宿主 OS 的 mime 数据库，
// Windows 注册表缺项时 mime.TypeByExtension 可能落空）。
var contentTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".mjs":   "text/javascript; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".json":  "application/json; charset=utf-8",
	".map":   "application/json; charset=utf-8",
	".png":   "image/png",
	".jpg":   "image/jpeg",
	".jpeg":  "image/jpeg",
	".gif":   "image/gif",
	".webp":  "image/webp",
	".svg":   "image/svg+xml",
	".ico":   "image/x-icon",
	".txt":   "text/plain; charset=utf-8",
	".woff":  "font/woff",
	".woff2": "font/woff2",
	".ttf":   "font/ttf",
	".wasm":  "application/wasm",
}

// SPAHandler 返回 SPA 静态托管处理器：命中的静态文件直接服务，
// 未命中路径回退 index.html（前端路由）。webDir 非空时从磁盘目录服务。
func SPAHandler(webDir string) (http.Handler, error) {
	var root fs.FS
	if webDir != "" {
		st, err := os.Stat(webDir)
		if err != nil {
			return nil, err
		}
		if !st.IsDir() {
			return nil, &fs.PathError{Op: "stat", Path: webDir, Err: fs.ErrInvalid}
		}
		root = os.DirFS(webDir)
	} else {
		sub, err := embeddedDist()
		if err != nil {
			return nil, err
		}
		root = sub
	}
	return &spaHandler{root: root}, nil
}

type spaHandler struct {
	root fs.FS
}

func (h *spaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")

	// URL path → FS 内相对路径；拒绝对 .. 等越界成分（ServeMux 已清洗，双保险）。
	name := path.Clean(strings.TrimPrefix(r.URL.Path, "/"))
	if name == "." || name == "" {
		name = "index.html"
	}
	if strings.HasPrefix(name, "..") {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	if name == "index.html" { // index 永不缓存，直接请求也一样
		h.serveIndex(w, r)
		return
	}

	f, err := h.root.Open(name)
	if err == nil {
		st, statErr := f.Stat()
		if statErr == nil && !st.IsDir() {
			h.serveFile(w, r, name, f, st)
			return
		}
		_ = f.Close()
	}
	// 前端路由 fallback：index.html 不缓存。
	h.serveIndex(w, r)
}

// serveFile 服务静态资源：按扩展名 Content-Type + 一天缓存，全程流式。
func (h *spaHandler) serveFile(w http.ResponseWriter, r *http.Request, name string, f fs.File, st fs.FileInfo) {
	defer func() { _ = f.Close() }()
	rs, ok := f.(io.ReadSeeker)
	if !ok { // embed / os 文件均可 Seek，兜底直接拷贝
		w.Header().Set("Content-Type", contentType(name))
		w.Header().Set("Cache-Control", staticCacheControl)
		_, _ = io.Copy(w, f)
		return
	}
	w.Header().Set("Content-Type", contentType(name))
	w.Header().Set("Cache-Control", staticCacheControl)
	http.ServeContent(w, r, name, st.ModTime(), rs)
}

// serveIndex 输出 index.html（不存在时 404 纯文本，仅占位 dist 缺失时触发）。
func (h *spaHandler) serveIndex(w http.ResponseWriter, r *http.Request) {
	f, err := h.root.Open("index.html")
	if err != nil {
		http.Error(w, "index.html not found", http.StatusNotFound)
		return
	}
	defer func() { _ = f.Close() }()
	st, statErr := f.Stat()
	var modTime time.Time
	if statErr == nil {
		modTime = st.ModTime()
	}
	w.Header().Set("Content-Type", contentTypes[".html"])
	w.Header().Set("Cache-Control", "no-cache")
	if rs, ok := f.(io.ReadSeeker); ok {
		http.ServeContent(w, r, "index.html", modTime, rs)
		return
	}
	_, _ = io.Copy(w, f)
}

// contentType 按扩展名取 Content-Type：内置表优先，回落 mime 数据库，再回落二进制流。
func contentType(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if ct, ok := contentTypes[ext]; ok {
		return ct
	}
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct
	}
	return "application/octet-stream"
}
