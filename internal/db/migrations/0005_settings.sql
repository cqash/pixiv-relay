-- 管理端运行时设置热更新（设计文档 §14.2）：DB 覆盖项表。
-- 生效优先级 DB > 环境变量 > 内置默认；value 存文本形式（按键白名单解析校验），
-- updated_at 为 Unix 毫秒（§4.1）。

CREATE TABLE settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at INTEGER NOT NULL
);
