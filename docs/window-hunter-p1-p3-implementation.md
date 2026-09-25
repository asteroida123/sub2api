# 窗口猎手 P1-P3 实施记录（猎手编排器 + 满血会话池 + 原生 SS）

> 分支：`design/window-hunter`。日期：2026-09-25。
> 前序：`docs/window-hunter-p0-implementation.md`（P0 已验收部署）。
> 状态：**代码完成，本地 build + unit 全绿（后端 56 包 / 前端 2484 用例）；
> 真实环境验收待部署（需用户批准）+ 多国代理订阅。P2 验收实验规程见
> `docs/window-hunter-p2-experiment.md`。**

## P1：窗口猎手编排器 + 代理拨号修复

### 猎手编排器 `service/window_hunter.go`
- `WindowHunterService`：输入 = 代理池 + 目标账号；**并发 1**（运行中再触发返回
  `ErrWindowHunterRunning`/HTTP 409）；逐出口经 `WindowProbeService.ProbeAccount` 指纹探测；
  **命中即停**；全员未命中 → 轮次退避（`HunterBackoffDelay`：base × 2^(n-1)，默认 30min 起、
  240min 封顶）。
- 出口去重：`DedupeHunterCandidatesBySubnet` 按 /24（IPv6 /64）网段去重——实测 22 个同段 IP
  = 同一节点；出口 IP 优先取代理质量检测快照（延迟缓存），回退代理 host。
- 排序：`OrderHunterCandidatesByRegionDiversity` 国家/区域交错（地域亲和让同区出口重复落
  同一节点簇）；单轮上限 `max_candidates_per_round`（默认 50，安全阀）。
- 运行日志：内存环形 30 轮（面板轮询展示）；探测事实本身持久化在 account_node_health。
- 自动模式：`auto_enabled` 时后台循环按退避节奏自动开轮；命中后停止排程，健康态过期
  （派生态不再满血）自动重新武装。
- 面板：账号工具菜单"窗口猎手"→ `WindowHunterModal.vue`（状态/立即狩猎/预建/会话池/
  运行日志，4s 轮询）。
- API：`GET|POST /admin/window-hunter/{status,run,settings}`。

### 代理拨号 InsecureSkipVerify 开关（实测必现 bug 修复）
- 背景：IP 直连代理节点证书无 IP SANs → Go 默认校验必失败（x509），等价 curl
  `--proxy-insecure`【实测，EXPERIMENT-LOG-292 §9】。
- 实现：settings key `proxy_dial_settings`（`{insecure_skip_verify: bool}`），
  `service/proxy_dial_settings.go` 提供 60s TTL 共享缓存（`SharedProxyTLSInsecureSkipVerify`，
  与既有 `SetCodexCanonicalUserAgentResolver` 全局解析器模式一致）。
- 生效点（**仅豁免代理 hop**，到上游目标的 TLS 校验不受影响）：
  - HTTP 普通 transport：`internal/pkg/proxytls.ApplyProxyHopInsecure`——利用 Go
    `net/http` 既定语义（带自定义 DialTLSContext 时代理 hop 以代理地址回调），按地址
    区分两跳；目标 hop 仍走标准校验。UTLS 指纹路径的 http 代理 CONNECT 为明文，不涉及。
  - WS 代理客户端（`openai_ws_client.go` proxyHTTPClient）：同一 helper。
- 缓存正确性：开关只在开启时追加入客户端缓存键（HTTP 上游 poolKey / WS 代理客户端键），
  翻转即触发重建；关闭态键与历史一致。
- API：`GET|PUT /admin/settings/proxy-dial`。

### P1 验收对照
- [x] 单测：`TestSubnetKeyOf`、`TestDedupeHunterCandidatesBySubnet`（含出口 IP 快照合并）、
  `TestOrderHunterCandidatesByRegionDiversity`、`TestHunterBackoffDelay`（倍增+封顶）、
  `TestWindowHunterRunRoundHitStops`（命中即停，第 3 出口未被探测）、
  `TestWindowHunterRunRoundAllMissBacksOff`、`TestWindowHunterConcurrencyOne`、
  `TestWindowHunterNoTarget`。
- [ ] 真实环境：多国代理导入+质量检测后 `POST /window-hunter/run`，面板日志可见命中出口。

## P2：满血会话池 + 调度集成

### 池条目扩展 `service/openai_ws_pool.go`
- `openAIWSConn` 新增：`proxyID`（劣化回写定位 (账号,出口)）、`fullPowerUntilNano`
  （满血标记，UnixNano）、`lastSampleAtNano`、`degradedFlag`、`lastSampleAnswer`、
  `sampleFailureCount`；`dialConn` 按请求打标。
- 满血寿命：`MarkFullPowerUntil(established + session_lifetime)`，默认 60min，同时受池
  60min 硬寿命（openAIWSConnMaxAge）约束。
- **逐出保护（关键工程修复，实测踩坑后加固）**：`connEvictionRank`（未标记 0 < 过窗/降智 1
  < 在窗满血 2）应用到全部三条逐出路径（容量满腾位 ×2、maxIdle 冗余回收）——满血连接最后
  被回收；当唯一空闲是在窗满血连接时允许瞬时超容量拨新（否则满血预建会被后台空闲填充
  循环挤掉——单测过程中实测命中该竞态）。
- `lastAcquire` 模板净化：`recordLastSuccessfulAcquire` 剥离 `ForceNewConn/RequireFullPower/
  MarkFullPowerUntil`，防预建请求污染后台空闲补建（否则补建出的连接全部带满血标记/强制新建）。

### 采样与劣化处置 `service/window_session_pool.go`
- 采样循环：`SamplingEnabled` 时每 30s 巡检，对到期（`last_sample_at + interval`）的在窗
  空闲会话逐条注入指纹 turn（`response.create` 帧，字段与 HTTP /responses 一致；
  ~25 token/发）。
- 判定处置：满血→记录答案；**降智→清除标记 + `MarkBroken` + `RetireDegradedConn` 退役 +
  `applyProbeOutcome(degraded)` 回写 (账号,出口) 健康状态机**（进 4h 冷却）；连续失败
  ≥`sample_fail_threshold`（默认 2）撤销满血标记但不判降智。
- 预建：`POST /admin/window-session-pool/prewarm {account_id, count}`——复用账号
  `lastAcquire` 握手头（需账号近期有过业务流量）拨 N 条新连接并打满血标。
- 调度集成：`Acquire` 挑选优先在窗满血条目（`pickLeastBusy*` 引入 rank）；门控模型
  （`gate_models`，默认 `gpt-6-astra`）在 `reject_gated_when_no_full_power=true` 时
  `RequireFullPower`——拿不到满血直接报错（`openAIWSFullPowerUnavailableError`），
  **不走 HTTP 回退**（回退=接受降智供货，与门控语义相悖）。默认该开关关闭。
- API：`GET /admin/window-session-pool`（快照+采样记录）、`POST {prewarm,sample}`、
  `GET|PUT settings`（key `window_session_pool_settings`）。
- 配置注意：启用会话池的账号其 `MaxIdlePerAccount` 应 ≥ prewarm_count，否则清理周期会
  回收"超出空闲上限"的满血连接（按 rank 仍最后回收）。

### P2 验收对照
- [x] 单测：`TestWindowSessionPoolPrewarmMarksFullPower`（预建打标）、
  `TestRequireFullPowerGate`（无满血→哨兵；有满血→优先选中）、`TestRetireDegradedConn`、
  `TestSampleFailureRevokesFullPower`、`TestWindowSessionPoolSettingsGateModel`、
  `MatchesGateModel` 边界（`gpt-6-astrox` 不误匹配）。
- [ ] **验收实验（原样执行）**：`docs/window-hunter-p2-experiment.md`——窗口命中→预建 3 条
  →跨 ~250s→逐条判定；结果决定池语义标注（存活=满血供货 / 劣化=1h 强制寿命）。
  实验消耗预估 ~800-1300 token。

## P3：原生 Shadowsocks 拨号

- 协议白名单：`proxy_handler.go` 三处 `oneof` 加 `ss`（约定 username=加密方法，
  password=密码，端口必填）。
- 拨号器：`internal/pkg/tlsfingerprint/ss.go`——`DialSSContext`（TCP→cipher.StreamConn→
  写 SOCKS5 地址格式目标）+ `SSProxyDialer.DialTLSContext`（ss 隧道 + utls 指纹握手，
  供 `buildUpstreamTransportWithTLSFingerprint` 新增 `case "ss"`）；依赖
  `github.com/shadowsocks/go-shadowsocks2` v0.1.5（core+socks）。
- 普通路径：`proxyutil.ConfigureTransportProxy` 新增 `case "ss"`（transport.DialContext =
  ss 隧道，TLS 由调用方完成）；WS 代理客户端同路径支持（wss 的 TLS 由 Transport 在隧道上
  完成，标准库指纹）。
- 代理跳 TLS 豁免：ss 为对称加密隧道、无 TLS 握手，不涉及该开关（P1 已全量覆盖）。
- 单测：`TestParseSSProxyURL`（凭据/端口校验）、`TestSSProxyDialerRejectsBadConfig`
  （坏方法/非 tcp 即失败）、`TestConfigureTransportProxySS`（DialContext 安装、Proxy 为空）。
- 已知边界：ss 出口的 TLS 指纹在普通路径为标准库；需要指纹伪装走带 profile 的
  DoWithTLS 路径（case "ss" → utls）。

## 部署与验证（待用户批准）

1. 交叉编译：`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags embed -o sub2api-custom ./cmd/server`
2. 部署（二进制挂载覆盖，历史做法）→ 启动观察迁移 241 已在 P0 阶段应用。
3. 按任务书流程：P1 真实验证（多国代理狩猎）→ P2 验收实验（见专文）→ 结果回写文档。
4. 消耗告知：P2 实验约 800-1300 token + 狩猎探测每发 ~25 token；P3 无消耗（纯拨号层）。
