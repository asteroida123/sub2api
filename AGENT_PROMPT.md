# 任务提示词：实现"窗口猎手"子系统

> 本文件是给实现 agent 的完整任务书。工作目录：`~/work/sub2api-asteroida`。
> 开工前必读两份文档（仓库内与本机）：
> 1. `docs/window-hunter-gap-analysis.md`（本仓库 `design/window-hunter` 分支）——功能缺口分析 G1-G7 与分期 P0-P3
> 2. `~/work/cpa_plugin/EXPERIMENT-LOG-292.md` —— 完整实验档案（§8/§9 是原理验证的实测数据与判读）

---

## 一、背景（已被实验证实的机制）

对 OpenAI 网关（`chatgpt.com/backend-api/codex/responses`）的实测结论：

1. 服务侧对账号有**风控标记**，标记缓存在**计算节点**上，按 `(账号 × 节点)` 维度存储；
2. 出口 IP 决定地域亲和，从而决定落到哪个节点。节点没见过该账号 = **乐观窗口**：首请求起 ~200 秒内出满血模型（实测满血持续 183-236s）；
3. 节点拉取到标记后**立即对一切请求降智**，包括已建立的连接（+236s 实测翻车）——所以"裸传输连接存活"无意义；
4. 节点标记有滚动清除：静默 2-4 小时后该节点恢复"陌生"，再次获得窗口；
5. **未验证**：带上下文绑定的 WS 会话（previous_response_id / prompt_cache_key）能否活过窗口——这是 P2 前必须做的验证实验；
6. 检测仪器：早苗式指纹问题（现任首脑类，知识截止后可判定），经 `stream:true` 的 codex/responses 端点，~25 token/发，答案二分类=满血/降智。

**注意**：官方代码库中原有的 292 ticket 子系统已被上游移除且票格式已变（现为 780 字符 Fernet，不可读），**不要**基于旧 ticket 代码构建，一切判定走指纹探针。

## 二、分期任务

> **执行授权（2026-09-28 更新）**：P0 已验收通过并部署。**现在授权连续实施 P1 → P2 → P3**，不要停：P2 的"前置验证"不再是拦路门槛——验收实验就是 P2 交付的一部分（先建池跑实验，实验结果决定池的语义：存活=满血供货；劣化=退化为 1h 强制寿命）。窗口依赖：多国代理订阅由用户提供（近期到位），P1 完成后即可在真实窗口上跑 P2 验收实验。

### P0（已完成并验收 ✅ 2026-09-28，commit 25429cd8a）：降智状态机 + 指纹探针 + 面板徽标

- 新表 `account_node_health`：
  `id, account_id, proxy_id, region, state(unknown|full_power|degraded|cooldown), window_opened_at, degraded_at, cooldown_until, last_probe_at, last_probe_answer, created_at, updated_at`
- 新服务 `backend/internal/service/window_probe.go`：
  - `ProbeAccount(ctx, account, proxy) → {result: full_power|degraded|error, answer, latency}`
  - 探测实现：stream:true POST 上游 responses 端点（headers 参考 `EXPERIMENT-LOG-292.md` §5 的 probe 函数：Authorization/Session_id/Chatgpt-Account-Id/Version/User-Agent/Originator/OpenAI-Beta）
  - 题库可配置（settings 或 yaml），默认 3-5 道现任首脑类问题轮换；分类器=关键词新旧对照（可配置映射表）
- 状态机语义（必须实现，这是实验得出的核心约束）：
  - **探测即污染**：一次探测 = 该 (账号,节点) 的窗口时钟归零。状态机记录每次探测时间，禁止对同一节点高频复探
  - 命中满血 → `full_power` + `window_opened_at`；未命中 → `degraded` + 冷却计时（默认 4h，可配）
- 面板：账号列表健康徽标（满血/降智/冷却+剩余时间）+ 手动"探测一次"按钮
- **验收**：对指定账号+代理返回三态结果并入库；面板可见；单测覆盖分类器与状态转移

### P1：窗口猎手编排器 + 代理拨号修复

- 新服务 `backend/internal/service/window_hunter.go`：
  - 输入：代理池（多国）+ 目标账号
  - 行为：**并发 1**；按出口 IP+ASN 去重（同 /24 视为同一节点——实测 22 个同段 IP 是同一节点）；逐个指纹探测；**命中即停**；全部未命中 → 本轮结束进入退避（轮次退避：间隔逐轮加倍）
- 代理拨号修复（实测必现 bug）：`internal/tlsfingerprint/` 或代理拨号层增加 **InsecureSkipVerify 可配置开关**——IP 直连形式的代理节点其 TLS 证书无 IP SANs，默认校验必失败（x509: cannot validate certificate for <IP>），curl 加 --proxy-insecure 即通
- 面板：猎手运行日志 + "立即狩猎"按钮
- **验收**：给定多国代理列表，自动找到满血出口并在面板可见；单测覆盖去重与退避

### P2：满血会话池 + 调度集成

- WS 池条目扩展（`openai_ws_forwarder_v2.go` 的池结构）：`established_at, full_power_until(≈established+1h，实测待定), last_sample_at, degraded_flag`
- 采样：每会话每 N 分钟注入一发指纹 turn；劣化 → 退役该条 + 回写状态机 degraded
- 调度：业务请求 Acquire 时优先取 full_power 会话；无可用 → 按配置（走常规 or 拒绝门控模型）

**P2 验收实验（这是本期的存在意义，必须原样执行）**：
模拟真实用法——
1. 窗口开启时（新出口首发指纹=满血），**立即建立 ≥3 条独立上游 WS 连接**存入池中；
2. **什么都不做，等窗口关闭**（~250s 后同出口新请求已降智为证）；
3. 然后逐条从池里取连接发指纹请求：**若池中连接在窗口关闭后依然返回满血 → “满血时多建、后面用”成立，会话池就是 1 小时供货机制；若返回降智 → 该机制证伪**，P2 只保留“1h 强制寿命”实现并在文档标注理论未证实。
每条连接的判定结果必须分别记录。

### P3（按需）：原生 Shadowsocks 拨号

- `proxy_handler.go` 协议白名单加 `ss`；引入 `github.com/shadowsocks/go-shadowsocks2` 实现 Dialer，注册进 `buildUpstreamTransportWithTLSFingerprint` 的 switch
- 顺带：代理拨号 InsecureSkipVerify 用户级开关（P1 已做则此条只剩 ss 侧接线）

## 三、硬性约束（红线，违反即返工）

1. **生产服务器**：`ssh -p 50990 root@70.39.193.249`，sub2api 跑 docker compose（`/root/sub2api-deploy/`）。**任何部署/重启/DB 写入前必须征得用户同意**；官方镜像为主，产物用"二进制挂载覆盖"方式部署（历史做法见 `~/work/sub2api` 仓库的经验）
2. **测试消耗真实账号配额**：每次指纹 ~25 token；批量验证前告知用户规模
3. **读路由/用量必须过滤 api_key**：服务器上有其他用户并发流量，`usage_logs` 直接读最后一行会读到别人的请求；必须 `WHERE api_key_id=<测试key>` 过滤
4. **OAuth token 刷新只能靠发真实流量让 sub2api 自己做**；手动用 refresh_token 会因轮转机制作废 sub2api 存的那份，号直接废
5. **git 推送**：remote origin 已配专用 deploy key（ssh config 别名 `github-asteroida`），推 feature 分支，**不要动 main**
6. 服务器 SSH 偶发抽风（多次快速连接会 255），失败就等 30 秒重试；远程命令里**禁止 pkill -f 带脚本名**（会自匹配杀掉自己的 shell），需要时用正则括号技巧 `pkill -f "surv_test[3].py"`
7. `docker exec -i` 在 heredoc 场景会吞 stdin；psql 多语句用单 `-c "A; B"`
8. 结论必须区分【实测】/【推断】——本项目的实验标准

## 四、验证流程（每期通用）

1. 本地 `go build ./... && go test ./...` 全绿
2. 交叉编译：`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags embed -o <产物> ./cmd/server`
3. 部署：先征得用户同意 → scp 到服务器 `/root/sub2api-deploy/custom/`（或按当期约定）→ `docker compose up -d`
4. 真实环境小流量验证（规模先告知用户）→ 结果写回本仓库实验文档 → 推送分支

## 五、当前环境快照（2026-09-27）

- 生产 sub2api：官方 0.2.7 镜像，健康；两个 OAuth 账号（openai2/2474、ya/2475）绑定 IPRoyal 静态住宅（纽约）；krill/云之声等为中转溢出账号
- 账号当前均处于降智状态（今日实验消耗了窗口）；按衰减模型，静默 4h 后的第一次探测即可验证恢复
- 面板访问：`ssh -p 50990 -L 8080:127.0.0.1:8080 root@70.39.193.249` 后浏览器开 `http://localhost:8080`
