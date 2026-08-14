-- M2 服务认证（设计文档 §5）：账号与设备表。
-- token 与 accountKey 只存 SHA-256 十六进制哈希，绝不落明文。
-- 时间字段统一 Unix 毫秒（§4.1）。

CREATE TABLE accounts (
    id               INTEGER PRIMARY KEY,
    account_key_hash TEXT UNIQUE NOT NULL,
    created_at       INTEGER NOT NULL
);

CREATE TABLE devices (
    id                  INTEGER PRIMARY KEY,
    account_id          INTEGER NOT NULL REFERENCES accounts(id),
    device_name         TEXT NOT NULL,
    access_token_hash   TEXT UNIQUE NOT NULL,
    refresh_token_hash  TEXT UNIQUE NOT NULL,
    invite_code         TEXT,
    created_at          INTEGER NOT NULL,
    access_expires_at   INTEGER NOT NULL,
    refresh_expires_at  INTEGER NOT NULL
);

CREATE INDEX idx_devices_account_id ON devices(account_id);
