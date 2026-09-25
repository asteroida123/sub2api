# 窗口猎手 P0 实施记录（降智状态机 + 指纹探针 + 面板徽标）

> 分支：`design/window-hunter`。日期：2026-09-25。
> 上游文档：`docs/window-hunter-gap-analysis.md`（G1/G2/G7）、`~/work/cpa_plugin/EXPERIMENT-LOG-292.md`（§8/§9 实测）。
> 状态：**代码完成，本地 build + unit 全绿；真实环境验证待部署（需用户批准）。**

## 一、交付内容

### 1. 数据层
- 迁移 `backend/migrations/241_account_node_health.sql`：表 `account_node_health`，
  唯一键 `(account_id, proxy_id)`；`proxy_id=0` 表示直连出口（避免 NULL 唯一约束语义问题）。
  额外列 `probe_count`（题库轮换依据，不在原始 spec 中但必要）。
- Ent schema `backend/ent/schema/account_node_health.go`（已 `go generate ./ent`）。
- 仓储 `backend/internal/repository/account_node_health_repo.go`
  （`GetByAccountAndProxy` / `Upsert`（ON CONFLICT DO UPDATE）/ `List` / `ListByAccountIDs`）。

### 2. 探针服务 `backend/internal/service/window_probe.go`
- `WindowProbeService.ProbeAccount(ctx, accountID, proxyID *int64, force bool) → WindowProbeOutcome`。
  - 三态：`full_power | degraded | error`（+ 每态附 answer / latency / 所问题目 / 探测后状态）。
  - 出口选择：显式 proxy_id > 账号绑定代理 > 直连（P1 猎手直接复用）。
  - 上游请求与实验仪器逐项对齐（EXPERIMENT-LOG-292 §5）：
    `stream:true` POST `chatgpt.com/backend-api/codex/responses`，headers:
    Authorization / Session_id(fresh UUID) / Chatgpt-Account-Id / Version / User-Agent / Originator / OpenAI-Beta；
    模型默认 `gpt-6-astra`；SSE 解析取首个 `response.output_text.done`（= bash 版 `head -1`）。
    ~25 token/发。
  - 走既有 `HTTPUpstream.DoWithTLS`（utls 指纹 + 代理拨号，与真实转发同路径，插件路径除外）。
- **状态机（核心实验约束）**：
  - 探测即污染：任何探测（含失败）都刷新 `last_probe_at`；`force=false` 时若距上次探测
    不足 `min_probe_interval_seconds`（默认 120s）返回 `ErrWindowProbeTooFrequent`（HTTP 429）。
  - 命中满血 → `state=full_power` + `window_opened_at=now`，清冷却。
  - 未命中 → `state=degraded` + `degraded_at=now` + `cooldown_until=now+cooldown_minutes`（默认 240=4h）。
  - `error`（上游故障/无法分类）不改状态结论，只记探测时间与答案。
  - 派生态 `EffectiveState(now, windowDuration)`：full_power 超过 `window_duration_seconds`
    （默认 240s，实测窗口 183-236s）自动视为 degraded（探测即污染，不允许复探确认）；
    degraded 未过冷却期显示为 `cooldown`（徽标带剩余时间）。
- 题库与分类器：
  - 默认 5 道现任首脑题（日本首相/美国总统/韩国总统/德国总理/加拿大总理），按 `probe_count` 轮换；
  - 分类器=归一化（小写/去空白标点）后的新旧关键词子串对照；双方同时命中或都不命中 →
    不判定（诚实优于猜测），原始答案入库供人工判读。
  - 题库/冷却/间隔/窗口时长/超时/模型全部可配：settings 表 key `window_probe_settings`
    （`service/window_probe_settings.go`，未配置回退默认值，读写都过 `Normalize()` 校验）。

### 3. Admin API（`backend/internal/handler/admin/window_probe_handler.go`）
| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/admin/window-probe/health?account_ids=1,2` | 徽标数据源（含派生态+剩余秒数） |
| POST | `/admin/accounts/:id/window-probe` | 手动探测一次 `{proxy_id?, force?}`；间隔不足→429 |
| GET | `/admin/window-probe/settings` | 读配置 |
| PUT | `/admin/window-probe/settings` | 写配置 |

### 4. 面板（Vue3）
- 账号列表 status 列下方新增窗口健康徽标 `WindowProbeBadge.vue`：
  满血(绿，窗口剩余倒计时)/冷却中(琥珀，冷却剩余倒计时)/降智(红)/未探测(灰)；
  点击徽标=立即探测（带污染提示 tooltip），探测结果 toast 显示答案原文。
- API 模块 `frontend/src/api/admin/windowProbe.ts`；i18n zh/en。

## 二、验收对照（P0）
- [x] 探测三态结果入库：单测 `TestProbeAccount*` 全链路覆盖（fake 上游 SSE + fake 仓储）。
- [x] 状态机语义：`TestApplyProbeOutcome*`（满血/降智/error 不迁移）、`TestEffectiveState*`（窗口过期/冷却派生）、复探纪律 `TestProbeAccountEndToEndDegradedAndGuard`。
- [x] 分类器：`TestClassifyProbeAnswer`（中/英/混合/歧义/空）、`TestNormalizeProbeAnswer`。
- [x] SSE 解析：`TestParseCodexProbeAnswer*`（done/error/failed/无答案）。
- [x] `go build ./...` 全绿；`go test -tags=unit ./...` 全绿（57 包）。
- [ ] **真实环境验证（待用户批准部署后）**：对 2474/2475 指定代理发一发探测，验证三态入库与面板显示。
  注意两号当前均为降智态（09-27 实验消耗），静默满 4h 后首探才有判定意义。

## 三、已知边界 / P1-P3 备注
- 探针绕过插件管理器（`pluginManager.RoundTripOpenAIOAuth`），直走 `DoWithTLS`；
  本部署未用 OpenAI OAuth 插件，如未来启用需重审此路径。
- OAuth token 沿用 sub2api 存量 access_token（红线 #4：不手动刷新）；过期→401→`error` 态，
  真实业务流量经网关刷新后可再探。
- `region` 尽力从代理延迟缓存填充（需先跑过质量检测），空字符串表示未知。
- P1（猎手编排器 + 代理 InsecureSkipVerify 开关）、P2（WS 会话池，前置：跨 250s 存活验证实验，
  §9 已实测证伪"会话豁免"，P2 按 1h 强制寿命实现）、P3（原生 SS scheme）待续。
