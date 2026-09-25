-- 窗口猎手（Window Hunter）P0：账号×出口节点 降智健康状态机
--
-- 语义（来自 292 实验档案 docs/window-hunter-gap-analysis.md）：
--   - 上游风控标记按 (账号 × 计算节点) 维度缓存在节点上，节点静默 2-4h 后滚动清除；
--   - 探测即污染：对 (账号,节点) 的每次探测都会刷新节点上的标记时钟（窗口归零），
--     因此 last_probe_at 是排程纪律的依据，禁止高频复探；
--   - 命中满血 → full_power + window_opened_at；未命中 → degraded + cooldown_until（默认 4h）。
--
-- proxy_id = 0 表示"直连出口"（不经过代理表），使 (account_id, proxy_id) 唯一约束无需
-- 处理 NULL 语义。region 为出口地理信息快照（国家/区域，尽力填充，来自代理质量检测）。

CREATE TABLE IF NOT EXISTS account_node_health (
    id                BIGSERIAL PRIMARY KEY,
    account_id        BIGINT       NOT NULL,
    proxy_id          BIGINT       NOT NULL DEFAULT 0,
    region            VARCHAR(64)  NOT NULL DEFAULT '',
    state             VARCHAR(20)  NOT NULL DEFAULT 'unknown',
    window_opened_at  TIMESTAMPTZ  NULL,
    degraded_at       TIMESTAMPTZ  NULL,
    cooldown_until    TIMESTAMPTZ  NULL,
    probe_count       BIGINT       NOT NULL DEFAULT 0,
    last_probe_at     TIMESTAMPTZ  NULL,
    last_probe_answer TEXT         NOT NULL DEFAULT '',
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_account_node_health_account_proxy UNIQUE (account_id, proxy_id)
);

CREATE INDEX IF NOT EXISTS idx_account_node_health_account_id ON account_node_health(account_id);
CREATE INDEX IF NOT EXISTS idx_account_node_health_state ON account_node_health(state);
