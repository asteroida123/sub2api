# 窗口猎手项目封顶记录（2026-09-26）

> 本文是该子系统的**收尾记录**：为什么停在这里、已经证明了什么、生产上留了什么。
> 前序全部文档：`window-hunter-gap-analysis.md`、`window-hunter-p0-implementation.md`、
> `window-hunter-p1-p3-implementation.md`、`window-hunter-p2-experiment.md`、
> `window-hunter-ws-survival-verdict.md`、`window-hunter-p1-field-verification.md`。

## 一、停止的原因（用户决策）

乐观窗口能被真实流量利用（P1 真实验证已证实，见下），但**利用窗口的必要动作是反复更换出口 IP**。
用户判断：持续轮换 IP 会提高 Cloudflare / 上游风控把账号标为异常的概率，
收益（每出口约 200s 满血）不值得账号风险。**据此决定不再推进自动化轮换，项目封顶。**

后续新发现印证了这个判断的风险侧：`chatgpt.com` 的 codex 端点对一个
**未做任何出口轮换**的 OAuth token（2474）也开始返回伪造的凭证错误
（详见第三节"事件记录"），说明该端点的风控敏感度确实很高。

因此以下两项**已设计但未实施**，留档备查：

1. **探针双发确认**（应对单发判定波动，见 `window-hunter-p1-field-verification.md` 第四节）：
   `WindowProbeSettings` 增加确认发数；`ProbeAccount` 命中后再发一发，
   两发一致才写 `full_power`，否则不迁移状态（只记 `last_probe_at`）。
   改动点：`internal/service/window_probe.go`（settings 结构 + ProbeAccount + applyProbeOutcome 分支）、
   `internal/handler/admin/window_probe_handler.go`（DTO）、`window_probe_settings.go`（Normalize）。
2. **跨出口轮换的长任务可行性**：需验证"换出口后重发上下文，能否连续满血"，
   以及 `previous_response_id` 在 OAuth + HTTP 下被拒（实测错误：
   `previous_response_id requires an OpenAI API-key account for HTTP requests`）
   带来的每轮全量重发成本。

## 二、已确证的事实（全部实测，可直接引用）

### 成立的部分

| 结论 | 证据 |
|---|---|
| 乐观窗口真实存在，时长 ~183-236s（快窗口可短至 +27s） | `window-hunter-ws-survival-verdict.md` |
| 新出口首发即满血，可被真实业务流量承接 | 同上 + `window-hunter-p1-field-verification.md` |
| **codex CLI 真实链路可搭窗口（9/9 满血）** | `window-hunter-p1-field-verification.md` 第三节 |
| 猎手命中即停 + /24 去重生效（44 代理 → 37 候选，首探即命中） | 同上第二节 |
| mihomo sidecar 方案可行（165 listener / 46MB / 绑 docker bridge） | 同上第一节 |

### 被证伪 / 需要注意的部分

| 结论 | 说明 |
|---|---|
| **满血 WS 会话不豁免节点标记**（"1h 满血供货"不成立） | wire 级实测：3/3 条在 T+265s 降智，而同刻同出口对照 HTTP 仍满血 |
| **单发判定有波动**（5 发中 1 发假阳性） | 同一出口同一请求体连打 5 次：石破茂/高市早苗/石破茂/石破茂/石破茂 |
| **HTTP + OAuth 不支持 previous_response_id** | 上游直接拒绝，codex 类客户端只能每轮全量重发上下文 |
| **codex CLI 带 web_search 时答案可能来自检索** | 不能单独作满血判据，必须配"HTTP 极简无 tools"对照 |

## 三、事件记录：2026-09-26 06:52 起的上游异常

**现象**：`chatgpt.com/backend-api/codex/responses` 对所有请求返回
`{"error":{"message":"Incorrect API key provided: sk-svcacct***...***fvMA", "code":"invalid_api_key"}}`。

**判定为上游侧伪造错误，非账号问题**，依据：

1. **两个不同账号（2474/2475）拿到逐字节相同的错误**，masked key 长度/前缀/后缀/星号数完全一致
   （167 字符 / `sk-svcac****` / `****fvMA` / 155 个星号）——真实 key 校验不可能对不同输入产生相同掩码。
2. **该 masked key 不属于本系统任何账号**：全库搜 `accounts.credentials`、`accounts.extra`、
   `api_keys` 均零命中。
3. **不带真实 token 时错误形态不同**：发 `sk-svcacct-SHORT` 得到
   `api_key_not_supported`，而不带 Authorization 得到 `{"detail":"Unauthorized"}`
   ——说明服务端能区分 token 形态，"Incorrect API key" 只在带合法 OAuth JWT 时出现。
4. **三条完全独立的网络路径返回同一结果**：服务器机房 IP、mihomo SG/DE 出口、韩国住宅节点。
   连本地家用 IP 经 Clash 也一样——排除单点 IP 被拦。
5. **账号本身完好**：`api.openai.com/v1/me` 返回 200，带
   `"email":"elleveg594@gmail.com"`、`object:"user"`；
   token JWT 可正常解析（`iss=auth.openai.com`、`aud=api.openai.com/v1`、
   `plan_type=pro`、有效期至 2026-09-30）；`refresh_token` 完好（196 字符）；
   配额未耗尽（5h 0% / 7d 72%）。
6. **`chatgpt.com` 根路径返回 403 + `cf-mitigated: challenge`**（Cloudflare 挑战）。

**结论**：账号未被封、凭据未失效，**不需要刷新凭证**（手动刷新反而会因轮转机制作废
sub2api 存的那份 refresh_token——项目红线 #4）。

### 事后确认（2026-09-26 07:41）

**官方确认本次为 Codex backend 全局故障**，事件跟踪：
<https://status.openai.com/incidents/01M3DCNWMW57HYK8FJ5FBFPA39>
（"Issues with Codex"，Full outage，影响 Codex Web / Codex API / CLI / VS Code 扩展；
07:34 官方更新 "root cause identified, mitigation underway"）。

**与本文第 3 节判定一致**：账号侧无故障、换 IP 无效、错误是上游伪造。
**上游于 07:41:57 恢复**（`response.created` 正常返回），总故障时长约 50 分钟。
恢复后已清除 401 残留的运行时状态（`status=error` / `temp_unschedulable_*` / `error_message`），
两个账号回到 `active` + 直连，指纹探针与真实业务流量均验证正常。

**端点级判定的最终确认证据**（同一 token、同一域名、同一时刻）：
| 端点 | 结果 |
|---|---|
| `GET /backend-api/codex/models?client_version=...` | **200**（正常返回模型列表） |
| `api.openai.com/v1/me` | **200**（正常返回账号信息） |
| `POST /backend-api/codex/responses` | **401**（伪造的 `Incorrect API key ... fvMA`） |

同一 token 在 `/codex/models` 被接受、在 `/codex/responses` 被拒——故障在端点级而非账号级，
这是判定该次事故性质的**决定性证据**。

**副产品结论**：该端点对同一账号的响应在数小时内从"正常工作"变为"统一拒绝"，
且未伴随任何本系统的异常请求量（2474 在故障前 12h 共 585 请求，属正常实验量），
说明**该端点的风控对请求模式高度敏感**——这正是用户决定停止轮换的实证支撑。

## 四、生产上保留了什么

| 项 | 状态 | 说明 |
|---|---|---|
| sub2api | 运行中，含 P0-P3 + ss 白名单修复 | md5 `57b73eac64b11e1abaea37473d0d82e8` |
| mihomo sidecar | systemd active，165 listener 绑 `172.20.0.1` | 44 个代理依赖它；停用则这些代理不可用 |
| 44 个代理（proxies id 9-52） | `status=active`，名称前缀 `wh-` | 未删除；如需清理直接删这些行即可 |
| 账号 2474/2475 | `proxy_id=NULL`（直连），临时组/密钥已删除 | 无残留手工覆盖 |
| 猎手 / 会话池 | `auto_enabled=false`，白名单已清空 | 不会自行发起探测 |
| 备份 | `backup-db-20260926-020024.sql.gz`(134MB) + 两个二进制备份 | 保留在 `/root/sub2api-deploy/` |

**账号当前状态**：2474 `schedulable=false`、2475 `status=error`——这是 sub2api 在 401 之后的
自我保护（OAuth 401 冷却 10 分钟 + 连续失败禁用）。**上游恢复后**，把
`accounts.status/proxy_id` 归位、清 `temp_unschedulable_*` 即可恢复调度；
sub2api 的 `token_refresh_service` 也会在冷却后自行尝试。

## 五、若要重启本项目

1. 先确认 `chatgpt.com/backend-api/codex/responses` 恢复正常（用 `window-probe` 单发即可）。
2. 优先做**探针双发确认**（第一节第 1 项）——没有它，任何命中判定都不可信。
3. 若仍要自动化轮换，**务必先解决账号隔离**：代理是账号级的，
   在共享账号上轮换会影响到该账号的全部业务流量。
4. 参考 `backend/cmd/wsurvive/README.md` 的四条测量陷阱，避免重复踩坑。
