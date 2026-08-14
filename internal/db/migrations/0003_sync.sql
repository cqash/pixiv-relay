-- M5 数据同步（设计文档 §7）：同步域游标表与条目表。
-- syncToken = st_<seq>_<hex>，seq 与 token 时间戳同源、按（账号×域）单调递增，
-- 因此 pull 游标可直接由 token 解析出 seq 水位，无需额外审计表。
-- 时间字段统一 Unix 毫秒（§4.1）；deleted 为墓碑标记（0/1），保留 90 天。

CREATE TABLE sync_domains (
    account_id         INTEGER NOT NULL REFERENCES accounts(id),
    domain             TEXT NOT NULL,
    sync_token         TEXT NOT NULL DEFAULT '',
    latest_updated_at  INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (account_id, domain)
);

CREATE TABLE sync_entries (
    account_id INTEGER NOT NULL,
    domain     TEXT NOT NULL,
    "key"      TEXT NOT NULL,
    data       TEXT NOT NULL DEFAULT '{}',
    updated_at INTEGER NOT NULL,
    deleted    INTEGER NOT NULL DEFAULT 0,
    seq        INTEGER NOT NULL,
    PRIMARY KEY (account_id, domain, "key")
);

CREATE INDEX idx_sync_entries_seq ON sync_entries(account_id, domain, seq);
