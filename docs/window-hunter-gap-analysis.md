# 窗口猎手（Window Hunter）功能缺口分析

> 目标：假设"节点标记 + 乐观窗口"原理成立（实测：窗口 ~200s、节点按地域亲和、标记按 (账号×节点) 缓存、衰减 2-4h），
> 让 sub2api 具备 **自动寻找满血窗口 → 建立满血 WS 会话 → 会话池持续供货** 的完整能力。
> 基线：本仓库 main（v0.2.8, a3eb7ef3）。日期：2026-09-27。

---

## 一、目标架构（数据流）

```
多国代理池(用户提供的多国订阅)
        │
        ▼
[窗口猎手 Window Hunter]  并发1、逐出口指纹探测(早苗题, ~25 token)
        │ 命中满血
        ▼
[会话建立] 经命中代理拨上游 WS（sub2api 现有 WS 池）
        │
        ▼
[满血会话池 Session Pool]  每条: 账号×代理×建立时间×剩余寿命(~1h)×健康采样
        │ 业务调度优先取用
        ▼
[调度器]  账号健康状态: 满血窗口中 > 冷却中(降智,需静默) > 未知
        │ 指纹采样发现劣化
        ▼
自动退役 → 等待衰减 → 回到窗口猎手
```

## 二、现状盘点（可直接复用的设施）

| 设施 | 位置 | 说明 |
|---|---|---|
| 代理质量检测 | `service/admin_proxy.go: CheckProxyQuality` | 出口 IP/国家/延迟/评分+快照持久化，**连通性检测已内置** |
| WS 上游连接池 | `service/openai_ws_forwarder_v2.go: getOpenAIWSConnPool().Acquire` | 支持 PreferredConnID / ForceNewConn |
| utls TLS 指纹拨号 | `repository/http_upstream.go: buildUpstreamTransportWithTLSFingerprint` | 直连/socks5/http CONNECT 三种，抗 CF 指纹 |
| WS 入站协议 | `service/openai_ws_forwarder_ingress.go` | 帧级支持 `prompt_cache_key` / `previous_response_id` / continuation probe |
| apikey 健康熔断 | `service/openai_apikey_health_breaker.go` | 状态机模式可复制到 OAuth 降智标记 |
| 代理表 | proxies 表 + proxy_expiry_service / proxy_fallback / proxy_latency_cache | 有底座，缺轮换语义 |

## 三、功能缺口

### G1 降智状态标记（账号×节点健康状态机）【核心，无任何现成实现】
- 现状: OAuth 账号只有 active/disabled，没有"满血/降智/冷却"维度
- 方案: 新表 `account_node_health`（account_id, proxy_id, region, state[full_power|degraded|cooldown|unknown], window_opened_at, degraded_at, cooldown_until, last_fingerprint_at, last_fingerprint_answer）
- 探测器: 复用 `account_test_service` 骨架 → 早苗式指纹问题（可配置题库+答案分类器；备选启发式: reasoning_tokens>0）
- **关键语义**: 探测即使用 = 刷新节点标记。探测后该 (账号,节点) 的窗口时钟归零——状态机必须记录并据此排程
- 落点: `service/account_node_health.go`（新）+ `handler/admin/` 面板徽标

### G2 指纹探针服务【核心】
- probe(account, proxy) → {full_power | degraded | error}，一次 ~25 token
- 题库设计: 现任首脑/体育赛事结果等"知识截止后可判定"题；分类器: 关键词匹配（高市/石破茂式新旧对照）
- 调度纪律: 并发 1；同一 (账号,节点) 探测间隔 ≥ 窗口时长；命中即停
- 落点: `service/window_probe.go`（新），HTTP 直连上游 `backend-api/codex/responses`（stream:true）

### G3 满血会话池（WS 会话带生命周期标记）【核心】
- 现状: WS 连接池只有传输语义，无"满血"标记、无独立寿命
- 方案: 池条目扩展 `{conn_id, account_id, proxy_id, established_at, full_power_until(≈established+1h), last_sample_answer, degraded_flag}`
- 采样: 每会话每 N 分钟注入一发指纹 turn；劣化 → 立即退役 + 回写 G1 状态机
- 调度: 业务请求 Acquire 时优先 full_power 条目；无可用 → 走降智常规路径（或按配置拒绝）
- **验收实验（存在意义所在）**: 窗口内建 ≥3 条独立上游 WS 入池 → 等窗口关闭（同出口新请求已降智为证）→ 逐条取用发指纹：池中连接仍满血 = "满血时多建、后面用"成立；降智 = 证伪，只保留 1h 强制寿命实现
- 落点: `openai_ws_forwarder_v2.go` 池条目扩展 + `service/window_session_pool.go`（新）

### G4 窗口猎手（轮换编排器）【核心】
- 输入: 代理池（多国）+ 待补账号；行为: 并发 1、逐出口 G2 探测；命中 → G3 建会话 + 写 G1 状态 → 停止
- 策略: 出口去重（同 /24 同 ASN 视为同节点——实测教训: 22 个同段 IP = 1 个节点）；国家多样性优先
- 节流: 全员未命中即停止本轮（理论: 二轮 40、三轮 5——窗口是稀缺资源），等待衰减周期
- 落点: `service/window_hunter.go`（新）+ 面板"立即狩猎"按钮

### G5 代理协议扩展（原生 Shadowsocks + 证书校验开关）
- 现状: 仅 http/https/socks5/socks5h（`proxy_handler.go:31` oneof 校验 + `http_upstream.go` 拨号）；https 代理走普通 net/http（无指纹）
- 方案: 原生 shadowsocks scheme——引入 `shadowsocks/go-shadowsocks2` 实现 Dialer，注册进 `buildUpstreamTransportWithTLSFingerprint` 的 switch
- **附带必修**: 代理拨号增加 InsecureSkipVerify 开关（实测教训: IP 直连节点的证书无 IP SANs → x509 校验必失败，curl --proxy-insecure 可通而 sub2api 不通）
- 落点: `handler/admin/proxy_handler.go`（协议白名单）、`internal/tlsfingerprint/`（ss dialer）

### G6 调度集成（按健康状态路由）
- 账号选择: 满血窗口中的账号优先承接 astra/sol 等门控模型；cooldown 账号默认跳过（可配置是否接受降智供货）
- 静默排程: 冷却账号强制休息（不入轮换），到 `cooldown_until` 自动回探
- 落点: 账号选择器（scheduler）读 G1 状态

### G7 可观测性
- 面板: 账号健康徽标（满血/降智/冷却+倒计时）、会话池视图、猎手运行日志、指纹历史
- 指标: 命中率、窗口时长分布、会话平均满血时长

## 四、分期实施

| 期 | 内容 | 效果 |
|---|---|---|
| P0 | G1 状态机 + G2 探针 + G7 徽标 | "标记当前是否降智"直接可用；为一切后续实验提供仪器 |
| P1 | G4 猎手 + 代理 Insecure 开关 | 多国订阅即插即用，自动找窗口 |
| P2 | G3 会话池扩展 + G6 调度集成 | 满血供货闭环 |
| P3 | 原生 shadowsocks scheme（按需） | 补全协议矩阵 |

## 五、前置风险提示

1. 理论待验证层: "带上下文的 WS 会话能否活过窗口"（窗口本身已实测成立）。P2 动工前应先做该验证（一次窗口命中 + 桥接长对话测试）
2. 窗口是稀缺资源: 猎手必须并发 1、命中即停、全员未中即退避——探测本身就是消耗
3. 伦理/合规: 仅供自担风险的私人研究使用
