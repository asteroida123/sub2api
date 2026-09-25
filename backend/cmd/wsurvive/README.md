# cmd/wsurvive — 满血 WebSocket 会话存活判定工具

窗口猎手 P2 的 wire 级验收工具。存在理由：sub2api 的 HTTP 入站
（`/v1/chat/completions`、`/v1/responses` POST）会被
`resolveOpenAIWSDecisionByClientTransport` 强制成 HTTP SSE 上游，
**从不建立上游 WS 长连接**，因此对「WS 会话是否豁免节点标记」这一命题零信息量。
本工具绕过网关，直连上游 `wss://chatgpt.com/backend-api/codex/responses`。

## 用法

```bash
# 经指定出口跑完整判定（HTTP 确认窗口 → 窗口内建 N 条 WS → 跨窗口逐条判定）
export OAUTH_TOKEN=<OAuth access_token>
export CHATGPT_ACCOUNT_ID=<chatgpt_account_id>
go run ./cmd/wsurvive \
  -account 2475 \
  -proxy "socks5h://127.0.0.1:24096" \
  -model gpt-6-astra -n 3 -window 250 \
  -timeline "0,250,700,1500,2400,3300" \
  -out /tmp/wsurvive.jsonl
```

- `-proxy` 支持 `ss://method:password@host:port` 与 `socks5h://host:port`；
  拨号与 TLS 握手均复用生产的 `internal/pkg/tlsfingerprint` 拨号器，
  与 `http_upstream.go` 的 `buildUpstreamStrategyWithTLSFingerprint` 分支同构。
- 出口必须是**该账号从未见过**的节点（乐观窗口前提；探测即污染，命中前一发即消耗窗口）。
- `-timeline` 是相对 T0 的判定时刻；每个时刻都先发一发对照 HTTP（新请求），
  再逐条在已建立的 WS 上采样 —— **对照是判定的关键**。

## 判定要点（血泪教训）

1. **必须保持常驻读循环**。gorilla/websocket 只在 `ReadMessage` 调用栈内处理控制帧；
   读循环一停就不回 pong，上游会以 `close 1011 keepalive ping timeout` 主动关闭，
   看起来像「上游掐断」——那是工具缺陷，不是被测行为。
2. **utls ALPN 只能含 `http/1.1`**。带 h2 会与 `ForceAttemptHTTP2=false` 冲突，
   报 `malformed HTTP response "\x00\x00\x12\x04..."`。
3. **SSE 解析前剥 `data:` 前缀**，否则永远匹配不到 `response.output_text.done`。
4. **结论必须配同出口同时刻的 HTTP 对照**，否则无法排除「窗口本来就关了」。

## 实测结论

见 `docs/window-hunter-ws-survival-verdict.md`：**会话豁免证伪**。
窗口内建的 3 条 WS 在 T+265s 全部降智，而同时刻同出口 HTTP 新请求仍满血。
