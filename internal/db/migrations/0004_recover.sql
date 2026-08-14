-- M6 删除图恢复（设计文档 §8）：恢复缓存表。
-- pages 为 JSON 数组 [{page,url,width,height}]，url 存第三方源原始地址，
-- 响应时按请求基址包装为 /img/v1/fetch?url=...（§8.1）；
-- meta 为 JSON 对象（快照元数据 title/userName 等，无快照时为 {}）；
-- status ∈ ready / fetching / not_found；expire 为 Unix 毫秒：
--   ready 正缓存 90 天（RECOVER_TTL_DAYS），not_found 负缓存 7 天
--   （RECOVER_NEGATIVE_TTL_DAYS），fetching 仅占位（15 分钟守卫窗口，
--   防 worker 崩溃残留，过期后允许重新入队）。
-- 图片 blob 绝不入 DB（HDD 策略），实际内容落 M4 磁盘缓存（cache.Put）。

CREATE TABLE recover_cache (
    account_id INTEGER NOT NULL,
    pid        TEXT    NOT NULL,
    pages      TEXT    NOT NULL DEFAULT '[]',
    source     TEXT    NOT NULL DEFAULT '',
    meta       TEXT    NOT NULL DEFAULT '{}',
    status     TEXT    NOT NULL,
    expire     INTEGER NOT NULL,
    PRIMARY KEY (account_id, pid)
);

CREATE INDEX idx_recover_cache_pid ON recover_cache(pid, status, expire);
