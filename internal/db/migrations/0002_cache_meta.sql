-- M4 图片中继磁盘缓存元数据（设计文档 §6.2 / §6.4）。
-- 图片 blob 绝不入 DB（HDD 策略），本表仅存元数据：
-- key = URL 的 SHA-256 十六进制，atime 供 LRU 淘汰排序（不依赖文件系统 atime）。
-- 时间字段统一 Unix 毫秒（§4.1）。

CREATE TABLE cache_meta (
    key          TEXT PRIMARY KEY,
    size         INTEGER NOT NULL,
    atime        INTEGER NOT NULL,
    created_at   INTEGER NOT NULL,
    content_type TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_cache_meta_atime ON cache_meta(atime);
