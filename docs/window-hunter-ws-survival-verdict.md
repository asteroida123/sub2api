# 窗口猎手 P2 wire 级判定：满血 WS 会话能否活过乐观窗口

> 日期：2026-09-26。实验工具：`backend/cmd/wsurvive`（本仓库）。
> 前序：`docs/window-hunter-p2-experiment.md`（验收规程）、`~/work/cpa_plugin/EXPERIMENT-LOG-292.md` §9。
> **结论一句话：满血 WS 会话不豁免节点标记，预建多连接同样失效。§9 的结论正确，但 §9 的实验从未真正测过这件事——本文是第一次。**

## 一、为什么必须重做这个实验

§9 记录「节点标记到达后立即生效于已建立的会话——满血 WS 会话存活 1h【实测证伪】」，据此宣布会话池方案终结。复核原始脚本（生产机 `/tmp/surv_2475.py`、`/tmp/surv_test*.py`、`/tmp/chain_hunter.py`）后发现该实验存在**设计缺陷**：

1. **全部走 HTTP 入站**：脚本一律 `POST http://127.0.0.1:8080/v1/chat/completions`。而
   `resolveOpenAIWSDecisionByClientTransport`（`internal/service/openai_client_transport.go:62`）对
   HTTP 入站强制返回 `client_protocol_http`，**上游一律 HTTP SSE，从不 acquire WS 池连接**。
2. 脚本注释里的「WS 桥接模式已开」指的是 sub2api 的 `http_bridge` 模式（客户端 WS 进、**上游 HTTP 出**），
   上游根本没有长连接。名字误导了后续判读。
3. 因此 §9 的 `+236s 石破茂` 只证明「同出口的后续新请求会降智」——这在节点已拉取标记后是必然的，
   与「已建立的 WS 长连接是否被掐断」无关。
4. `chain_hunter.py` 的链式结果也不可用：晚期判定点为 `??`（分类器不认意大利/法国首脑），
   且 FINAL 判定逻辑把首条降智也计入，输出 `❌` 是错的；链式由 sub2api 内部 replay 维持，
   不代表上游会话状态存活。

## 二、实验方法与工具缺陷修正

工具 `backend/cmd/wsurvive` 的观测路径：**直连上游 `wss://chatgpt.com/backend-api/codex/responses`**，
经指定出口（`ss://` 或 `socks5h://`），用生产同一套 utls 指纹拨号器
（`tlsfingerprint.NewSSProxyDialer` / `NewSOCKS5ProxyDialer`），与真实转发同构。

流程：HTTP 指纹确认窗口开启 → 窗口内建 N 条独立 WS（各发一发指纹）→ 等到窗口关闭 →
**在已建立的 WS 连接上再发指纹** → 按时间轴重复。

### 工具缺陷修正记录（首轮误判的根因）

首轮实验中 3 条连接在 idle 后全部 `broken pipe`，一度疑似「上游掐断」。实为工具缺陷：
gorilla/websocket **只在 `ReadMessage` 调用栈内处理控制帧**，我的首版实现把读循环停在判定逻辑之外，
导致不回 pong，上游以 `close 1011 (internal server error): keepalive ping timeout` 主动关闭。
修正为常驻读循环 + `msgCh` 投递后，连接全程存活（`pings_sent`/`pongs_recv` 正常），
才具备判定资格。**这个缺陷本身也是一条教训：任何「idle WS 存活」观测都必须保持读循环。**

## 三、判定实验（决定性，2026-09-26 01:41，出口=SG 独立 /24）

账号 2475，出口 SG（`ldc` 订阅 vless，出口 IP `163.128.99.57`，该出口此前从未见过 2475）。

| 时刻 | 事件 | 结果 |
|---|---|---|
| T0=01:41:24 | HTTP 指纹（窗口开启证据） | 高市早苗 **满血** |
| T+3s | WS#1 建立并首轮 | 高市早苗 满血 |
| T+7s | WS#2 建立并首轮 | 高市早苗 满血 |
| T+11s | WS#3 建立并首轮（无 ping 对照组） | 高市早苗 满血 |
| T+250s | **对照 HTTP（新请求）** | 高市早苗 **仍满血**（节点尚未拉取到标记） |
| T+265s | **WS#1 判定** | 石破茂 **降智** |
| T+266s | **WS#2 判定** | 石破茂 **降智** |
| T+268s | **WS#3 判定** | 石破茂 **降智** |
| T+700s | 对照 HTTP（新请求） | 石破茂 降智 |
| T+704/705/707s | 三条 WS 复判 | 全部 石破茂 降智 |

原始记录：`/tmp/keep-surv_v3.jsonl`（生产机与本地各存一份）。

### 判定

按 `window-hunter-p2-experiment.md` 的规则：**3/3 条在窗口关闭后 degraded → 会话豁免证伪【实测】**。

**核心结论**：窗口内建立的上游 WS 连接，在 T+265s 已降智。若「会话豁免 1h」成立，它们此刻应仍为满血。
**不成立。** 这直接否定了口子 2 的核心假设（「满血时多建、1 小时内在 websocket 会话内保持满血」）。

**仪器偏置有利于该结论**：本项目的单发判定误差倾向是「把降智误报为满血」（`window-hunter-p1-field-verification.md`
第四节实测 5 发中 1 发假阳性）。本次 3/3 一致报 degraded，误差方向与结论相反，故结论可信。

**一处需要更正的过度解读**（原文写于本次复核前）：
对照 HTTP 在 **T+250s** 满血、WS 判定在 **T+265s** 降智，二者相距 15 秒而非同一时刻。
因此数据**只能**证明「窗口在 T+250 与 T+265 之间关闭，且 WS 会话未获豁免」，
**不能**证明「WS 比新请求劣化得更早」——无法区分「会话更早劣化」与「窗口恰好在 15 秒内关闭」。
原表述「劣化得比新请求还早 / 不存在会话缓冲」属于过度解读，已更正。
（`docs/window-hunter-p1-field-verification.md` 与记忆文件中的同款表述一并按此更正。）

### 附：窗口时长是易变量（同批次另一出口）

01:31 在 FR 出口（`24021`）的同类实验：

| 时刻 | 事件 | 结果 |
|---|---|---|
| T0 | HTTP 指纹 | 高市早苗 满血 |
| T+4s | WS#1 建立并首轮 | 高市早苗 满血 |
| T+27s | WS#2 建立并首轮 | 石破茂 **降智** |
| T+39s | WS#3 建立并首轮 | 石破茂 **降智** |

即该次窗口在 **+4s ~ +27s 之间就关闭了**，远短于实测 183-236s 的典型值。
**工程含义**：预建动作必须在命中后秒级完成；「窗口内建 3 条」在慢窗口可行、在快窗口根本来不及。
这进一步削弱了「窗口内多建」的可行性。

## 四、对代码与文档的影响

1. **P2 池语义确定**：`window_session_pool` 的「满血标记寿命 60min」**不是供货机制**，
   退化为「established + 1h 强制寿命」（池的硬寿命 `openAIWSConnMaxAge` 本就是 60min）。
   `docs/window-hunter-p1-p3-implementation.md` 中「满血预建=满血供货」的表述需按此更正为
   「检测机制」——`RequireFullPower` 门控 + 采样仍有价值：它们能**识别**某条在池里的连接是否还满血，
   只是拿不到「预建即长期满血」的收益。
2. **满血供货的唯一来源是窗口本身**：窗口 ~200s（快窗口可到 ~30s），
   想做「供货」只能在命中后**立即承接真实业务**，而不是预建囤积。
3. **猎手仍然有用**：把窗口命中当作「当前时刻有一个可用的满血出口」的信号，
   在窗口内把真实请求路由过去（P0 状态机 + P1 编排器已具备），这是唯一成立的用法。
4. **不要基于本分支部署 P2 的池**：其核心假设已被证伪；部署收益只剩检测与门控。

## 五、顺带产出（P3 真实验证 + 白名单修复）

- **P3 原生 Shadowsocks 首次真实验证通过**：韩国 KT 家宽节点
  `ss://aes-256-gcm:***@ce1fc2d8.krkt.hkssip.com:49642`，
  经 `tlsfingerprint.NewSSProxyDialer` 成功打到 `chatgpt.com/cdn-cgi/trace`
  （`tls=TLSv1.3 / loc=KR / colo=ICN`）与 `backend-api/codex/responses`（405，预期）。
- **修复 ss 不可达的真实 bug**：`internal/pkg/proxyurl/parse.go` 的 `allowedSchemes` 缺 `ss`，
  导致主网关 HTTP 上游 / 探针 / 质量检测全部在 `proxyurl.Parse` 处失败
  （`http_upstream.go:1495` 的 `case "ss"` 永远走不到；只有 WS 客户端路径因为直接用 `url.Parse` 侥幸可用）。
  同步补齐 `handler/admin/account_data.go` 的 `validateDataProxy` 协议白名单。
- 该修复使韩国家宽节点可用于**探针与猎手**（本次实验经它跑通了完整窗口命中 + 3 条 WS 建立）。

## 六、方法论教训（写给下一个 agent）

1. **「上游掐断」与「工具不回 pong」必须区分**：idle 存活观测必须保持 `ReadMessage` 循环。
2. **任何声称「测过 WS 会话」的实验，先验证它真的建了 WS**：查 `SetOpenAIClientTransport` 标记、
   或直接看上游连接是否存在。HTTP 入站的实验对 WS 命题零信息量。
3. **对照必须同出口同账号同时刻**：本次的「HTTP 对照仍满血而 WS 已降智」才是判定的关键证据，
   单看 WS 侧无法排除「窗口本来就关了」。
