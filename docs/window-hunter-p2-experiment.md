# 窗口猎手 P2 验收实验：满血会话池生死判定

> 任务书 AGENT_PROMPT.md §P2："这是本期的存在意义，必须原样执行"。
> 执行前提：生产部署（需用户批准）+ 多国代理订阅到位 + 一个可消耗的 OpenAI OAuth 账号。
> 预计配额消耗：~30-50 发指纹 turn（约 800-1300 token）+ 每条会话建立流量，总量 KB 级。

## 一、实验问题

窗口命中时建立的 WS 会话，能否在节点拉取到风控标记（~250s）之后继续满血？

- **能** → "满血时多建、后面用"成立：会话池 = 窗口命中后的 ~1h 满血供货机制；
- **不能** → §9 结论（+236s 翻车）同样适用于池中会话：池退化为"established+1h 强制寿命"实现，
  满血供货只能靠"猎手频繁找窗口"（当前代码结构两种语义通用，仅结论标注不同）。

## 二、前置条件

1. sub2api 已部署本分支构建（含 window-hunter P0-P2）；
2. 代理表中导入多国代理并**跑过一次质量检测**（填充出口 IP/国家，供猎手去重排序）；
3. 账号 settings：`window_probe_settings`（默认即可）、`window_session_pool_settings`
   （实验期建议：`sampling_enabled=true`、`sample_interval_seconds=60`、`session_lifetime_minutes=60`）；
4. 目标账号当前处于降智态或未知态（冷却期已过），且该账号有账号绑定代理或直连可用。

## 三、实验步骤（面板 API 全程可执行）

```bash
# 0. 建立隧道后面板 API 基址
BASE=http://localhost:8080/api/v1/admin   # 需管理员 JWT

# 1. 找窗口：手动开一轮狩猎（并发1，逐出口探测，命中即停）
curl -X POST $BASE/window-hunter/run -d '{"account_id": 2474}'
# 轮询状态直到某轮 hit=true，记录命中出口 proxy_id 与时间 T0
curl $BASE/window-hunter/status

# 2. 窗口开启（T0 起 ~3 分钟内）：立即预建 ≥3 条独立满血会话
curl -X POST $BASE/window-session-pool/prewarm -d '{"account_id": 2474, "count": 3}'
# 记录返回的 conn_ids；确认满血视图
curl $BASE/window-session-pool

# 3. 什么都不做，等窗口关闭：T0+250s 后同出口发一发直连指纹应已降智（对照证据）
#    （用账号面板"探测一次"按钮对该出口 force 探测一发，预期 degraded）

# 4. 逐条从池里取连接发指纹（这是判定核心，每条分别记录）
curl -X POST $BASE/window-session-pool/sample -d '{"account_id": 2474, "conn_id": "<conn_id_1>"}'
curl -X POST $BASE/window-session-pool/sample -d '{"account_id": 2474, "conn_id": "<conn_id_2>"}'
curl -X POST $BASE/window-session-pool/sample -d '{"account_id": 2474, "conn_id": "<conn_id_3>"}'
# 每条返回 {result: full_power|degraded|error, answer, ...}

# 5. 时间轴采样（可选，测寿命边界）：命中满血的会话每 10 分钟重复 step 4，
#    直到首次 degraded 或达到 60min 强制寿命
```

注意：`sample` 本身是真实指纹消耗且计为一次探测（探测即污染），实验中逐条判定的次数
计入总消耗；采样间隔默认 300s，实验期调短到 60s 仅用于多采几个时间点。

## 四、判定与记录表

| 会话 | 建立时间 | 判定时刻 (T0+) | result | answer 原文 | 备注 |
|---|---|---|---|---|---|
| conn_id_1 | T0+__s | +__s | | | |
| conn_id_2 | T0+__s | +__s | | | |
| conn_id_3 | T0+__s | +__s | | | |

判定规则：
- **≥2/3 条在 T0+250s 后仍 full_power** → 会话池满血供货成立【实测】；
- **≥2/3 条 degraded** → 会话豁免证伪（与 §9 一致）【实测】，池按 1h 强制寿命实现（现状即满足）；
- **error 居多** → 仪器/执行问题（超时、token 过期、429），修因后重跑，不作判定。

## 五、实验执行检查单

- [ ] 用户批准部署与实验
- [ ] 多国代理导入 + 质量检测完成
- [ ] 猎手一轮命中（记录出口/国家/时刻）
- [ ] 3 条预建成功（conn_ids 记录）
- [ ] 窗口关闭对照证据（同出口直连一发 degraded）
- [ ] 逐条判定（表格填写）
- [ ] 结论写回本文档并标注【实测】/【推断】
