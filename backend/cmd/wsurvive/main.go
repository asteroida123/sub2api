// wsurvive 是窗口猎手 P2 的 wire 级验收实验：满血 WebSocket 会话能否活过乐观窗口？
//
// 为什么要独立做这个实验：EXPERIMENT-LOG-292 §9 用 chat/completions（HTTP 入站）
// 打 sub2api，被 resolveOpenAIWSDecisionByClientTransport 强制成 HTTP SSE 上游，
// 从未真正建立过上游 WS 长连接。它的「+236s 翻车」只证明「同出口新请求会降智」，
// 与「已建立的 WS 会话是否被掐断」无关。
//
// 实验设计（对齐 docs/window-hunter-p2-experiment.md 的判定规则）：
//  1. 经指定出口发 HTTP 指纹 → 必须是满血（窗口开启证据）
//  2. 窗口内建立 N 条独立上游 WS 连接，各发一发指纹 → 记录答案与 response_id
//  3. 什么都不做，等到 T+250s 之后：同出口新发 HTTP 指纹 → 预期降智（窗口关闭对照）
//  4. 逐条在【已建立的 WS 连接】上再发指纹 → 这是判定核心
//  5. 继续按时间轴采样，直到首次 degraded 或达到观察上限
//
// 判定：≥2/3 条在窗口关闭后仍满血 → 满血供货成立；多数降智 → 会话豁免证伪。
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	xproxy "golang.org/x/net/proxy"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gorilla/websocket"
)

type held struct {
	id        int
	conn      *websocket.Conn
	answer    string
	respID    string
	builtAt   time.Time
	verdicts  map[int]string
	msgCh     chan []byte  // 常驻读循环 → 采样逻辑
	closedAt  atomic.Int64 // UnixNano，上游关闭帧/读错误时刻
	closedWhy atomic.Value
	pingsSent atomic.Int64
	pongsRecv atomic.Int64
	pingStops chan struct{}
}

type config struct {
	accountID int64
	token     string
	chAcc     string
	proxyURL  string // ss://method:password@host:port
	model     string
	question  string
	windowSec int
	count     int
	timeline  string
	outPath   string
	verbose   bool
}

var cfg config

func main() {
	flag.Int64Var(&cfg.accountID, "account", 2475, "OAuth 账号 id（仅用于日志）")
	flag.StringVar(&cfg.token, "token", os.Getenv("OAUTH_TOKEN"), "OAuth access_token")
	flag.StringVar(&cfg.chAcc, "chatgpt-account", os.Getenv("CHATGPT_ACCOUNT_ID"), "Chatgpt-Account-Id")
	flag.StringVar(&cfg.proxyURL, "proxy", os.Getenv("SS_PROXY"), "出口代理（ss://method:password@host:port）")
	flag.StringVar(&cfg.model, "model", "gpt-6-astra", "模型")
	flag.StringVar(&cfg.question, "q", "日本现在的首相是谁？只回答姓名", "指纹问题")
	flag.IntVar(&cfg.windowSec, "window", 250, "等待窗口关闭的秒数")
	flag.IntVar(&cfg.count, "n", 3, "窗口内建立的 WS 连接数")
	flag.StringVar(&cfg.timeline, "timeline", "0,250,600,1800,3600", "判定时刻（相对 T0 的秒数，逗号分隔）")
	flag.StringVar(&cfg.outPath, "out", "/tmp/wsurvive.jsonl", "结果 JSONL 输出路径")
	flag.BoolVar(&cfg.verbose, "v", false, "打印原始 SSE/WS 事件（调试用）")
	flag.Parse()

	if strings.TrimSpace(cfg.token) == "" {
		fatal("需要 -token 或 OAUTH_TOKEN")
	}
	logf("=== 窗口猎手 P2 wire 级验收实验 ===")
	logf("账号=%d 出口=%s 模型=%s", cfg.accountID, redactProxy(cfg.proxyURL), cfg.model)

	res := &results{out: cfg.outPath}

	// ---- 步骤 1：HTTP 指纹确认窗口开启 ----
	httpAns, err := httpProbe(context.Background())
	if err != nil {
		fatal("步骤1 HTTP 指纹失败: %v", err)
	}
	verdict := classify(httpAns)
	res.record("http_window_check", map[string]any{"answer": httpAns, "verdict": verdict})
	logf("步骤1 HTTP 指纹: 「%s」 → %s", trim(httpAns), verdict)
	if verdict != "full_power" {
		logf("!! 出口当前非满血，窗口未开启。按探测即污染纪律，本次实验终止（不复探）。")
		res.finish("aborted_no_window", nil)
		return
	}
	t0 := time.Now()
	logf("T0 = %s（窗口计时起点）", t0.Format("15:04:05"))

	// ---- 步骤 2：窗口内建立 N 条独立 WS 连接 ----
	var (
		conns []*held
		mu    sync.Mutex
	)
	for i := 1; i <= cfg.count; i++ {
		h, err := dialWS(context.Background())
		if err != nil {
			logf("WS#%d 建连失败: %v", i, err)
			res.record("ws_dial_failed", map[string]any{"index": i, "error": err.Error()})
			continue
		}
		h2 := &held{id: i, conn: h, builtAt: time.Now(),
			verdicts: map[int]string{}, pingStops: make(chan struct{}), msgCh: make(chan []byte, 256)}
		startReader(t0, h2)
		ans, respID, err := wsTurn(h2, cfg.question)
		if err != nil {
			logf("WS#%d 建连后首轮失败: %v", i, err)
			_ = h.Close()
			res.record("ws_first_turn_failed", map[string]any{"index": i, "error": err.Error()})
			continue
		}
		h2.answer, h2.respID = ans, respID
		h2.verdicts[0] = classify(ans)
		if i < cfg.count || cfg.count == 1 {
			startPing(t0, h2)
		} else {
			logf("  (WS#%d 作为无 ping 对照组)", i)
		}
		conns = append(conns, h2)
		res.record("ws_established", map[string]any{
			"index": i, "answer": ans, "verdict": h2.verdicts[0],
			"response_id": respID, "at_t_plus_s": int(time.Since(t0).Seconds()),
		})
		logf("WS#%d 建立 (+%ds) 首轮「%s」 → %s", i, int(time.Since(t0).Seconds()), trim(ans), h2.verdicts[0])
	}
	if len(conns) == 0 {
		res.finish("no_ws_established", nil)
		return
	}
	logf("窗口内已建立 %d 条 WS 连接，总计消耗 %.0fs", len(conns), time.Since(t0).Seconds())

	// ---- 步骤 3~5：按时间轴判定 ----
	timeline := parseTimeline(cfg.timeline)
	for _, off := range timeline {
		if off == 0 {
			continue
		}
		waitUntil(t0, off)
		logf("--- T+%ds ---", int(time.Since(t0).Seconds()))

		// 对照：同出口新发 HTTP 指纹（窗口是否已关闭）
		httpAns2, herr := httpProbe(context.Background())
		if herr != nil {
			logf("对照 HTTP 指纹失败: %v", herr)
			res.record("http_control_failed", map[string]any{"at_t_plus_s": off, "error": herr.Error()})
		} else {
			v := classify(httpAns2)
			res.record("http_control", map[string]any{"at_t_plus_s": off, "answer": httpAns2, "verdict": v})
			logf("对照 HTTP（新请求）「%s」 → %s", trim(httpAns2), v)
		}

		// 判定核心：在已建立的 WS 连接上发指纹
		mu.Lock()
		for _, h := range conns {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			ans, _, err := wsTurnCh(ctx, h, cfg.question)
			cancel()
			if err != nil {
				h.verdicts[off] = "error"
				res.record("ws_sample_error", map[string]any{
					"index": h.id, "at_t_plus_s": off, "error": err.Error(),
					"dead_at_t_plus_s": int(time.Since(t0).Seconds()),
				})
				logf("WS#%d 采样失败: %v", h.id, err)
				continue
			}
			v := classify(ans)
			h.verdicts[off] = v
			res.record("ws_sample", map[string]any{
				"index": h.id, "at_t_plus_s": int(time.Since(t0).Seconds()),
				"answer": ans, "verdict": v,
			})
			logf("WS#%d 「%s」 → %s", h.id, trim(ans), v)
		}
		mu.Unlock()
	}

	// ---- 汇总判定 ----
	summary := map[string]any{}
	for _, h := range conns {
		close(h.pingStops)
		deadAt := h.closedAt.Load()
		entry := map[string]any{"verdicts_by_offset": h.verdicts,
			"pings_sent": h.pingsSent.Load(), "pongs_recv": h.pongsRecv.Load()}
		if deadAt != 0 {
			entry["closed_at_t_plus_s"] = int(time.Duration(deadAt - t0.UnixNano()).Seconds())
			if why, ok := h.closedWhy.Load().(string); ok {
				entry["closed_reason"] = why
			}
		} else {
			entry["alive_at_finish"] = true
			entry["idle_seconds"] = int(time.Since(h.builtAt).Seconds())
		}
		summary[fmt.Sprintf("conn_%d", h.id)] = entry
	}
	res.record("summary", summary)
	res.finish("done", summary)
}

// startReader 常驻读循环。**必须持续读到连接关闭**：gorilla/websocket 只在
// ReadMessage 调用栈内处理控制帧，读循环一停就不会自动回 pong，
// 上游会以 close 1011 "keepalive ping timeout" 主动关闭连接
// （首轮实验因此误判——那是工具缺陷，不是上游行为）。
func startReader(t0 time.Time, h *held) {
	h.conn.SetPongHandler(func(string) error {
		h.pongsRecv.Add(1)
		return nil
	})
	h.conn.SetPingHandler(func(data string) error {
		// 显式回 pong，且要通过 WriteControl 立即发，不与数据帧竞争写锁。
		_ = h.conn.WriteControl(websocket.PongMessage,
			[]byte(data), time.Now().Add(10*time.Second))
		return nil
	})
	go func() {
		defer close(h.msgCh)
		for {
			_, msg, err := h.conn.ReadMessage()
			if err != nil {
				if h.closedAt.CompareAndSwap(0, time.Now().UnixNano()) {
					h.closedWhy.Store(err.Error())
					logf("  ⚠ WS#%d 上游关闭 @T+%ds: %v",
						h.id, int(time.Since(t0).Seconds()), err)
				}
				return
			}
			h.msgCh <- msg
		}
	}()
}

// startPing 每 20s 发一个协议层 ping（零 token 消耗），用于观测连接活性。
func startPing(t0 time.Time, h *held) {
	go func() {
		tk := time.NewTicker(20 * time.Second)
		defer tk.Stop()
		for {
			select {
			case <-h.pingStops:
				return
			case <-tk.C:
				if h.closedAt.Load() != 0 {
					return
				}
				if err := h.conn.WriteControl(websocket.PingMessage, nil,
					time.Now().Add(10*time.Second)); err != nil {
					return
				}
				h.pingsSent.Add(1)
			}
		}
	}()
}

// ============================ HTTP 指纹（对照） ============================

func httpProbe(ctx context.Context) (string, error) {
	body := map[string]any{
		"model": cfg.model, "store": false, "stream": true,
		"input": []any{map[string]any{
			"role":    "user",
			"content": []any{map[string]any{"type": "input_text", "text": cfg.question}},
		}},
	}
	buf, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://chatgpt.com/backend-api/codex/responses", strings.NewReader(string(buf)))
	if err != nil {
		return "", err
	}
	applyUpstreamHeaders(req)

	client, err := proxyClient()
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
		return "", fmt.Errorf("HTTP %d: %s (cf-ray=%s)", resp.StatusCode, trim(string(raw)), resp.Header.Get("cf-ray"))
	}
	ans, err := parseSSEAnswer(resp.Body)
	if err != nil && cfg.verbose {
		logf("  [sse] 未命中 done 事件（cf-ray=%s cf-cache=%s）", resp.Header.Get("cf-ray"), resp.Header.Get("cf-cache-status"))
	}
	return ans, err
}

// ============================ 上游 WS ============================

func dialWS(ctx context.Context) (*websocket.Conn, error) {
	d := &websocket.Dialer{HandshakeTimeout: 45 * time.Second}
	if strings.TrimSpace(cfg.proxyURL) != "" {
		u, err := url.Parse(cfg.proxyURL)
		if err != nil {
			return nil, err
		}
		d.NetDialTLSContext = productionTLSDialer(u)
		d.NetDialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialViaProxy(ctx, u, network, addr)
		}
	} else {
		d.NetDialContext = netDialThroughProxy
	}
	hdr := http.Header{}
	applyUpstreamWSHeaders(hdr)
	conn, resp, err := d.DialContext(ctx, "wss://chatgpt.com/backend-api/codex/responses", hdr)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		return nil, fmt.Errorf("ws dial (http %d): %w", status, err)
	}
	return conn, nil
}

// applyUpstreamWSHeaders 为上游 WS 握手写身份头（与 HTTP 指纹同构）。
func applyUpstreamWSHeaders(hdr http.Header) {
	hdr.Set("Authorization", "Bearer "+cfg.token)
	hdr.Set("Session_id", newUUID())
	hdr.Set("Originator", "codex_cli_rs")
	hdr.Set("Version", "0.155.1")
	hdr.Set("User-Agent", "codex_cli_rs/0.155.1")
	hdr.Set("OpenAI-Beta", "responses=experimental")
	if cfg.chAcc != "" {
		hdr.Set("Chatgpt-Account-Id", cfg.chAcc)
	}
}

func wsTurn(h *held, question string) (string, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	return wsTurnCh(ctx, h, question)
}

// wsTurnCh 通过常驻读循环的 msgCh 发送一轮 response.create 并收集答案。
// 所有控制帧（含上游 ping）都由 startReader 的 ReadMessage 栈处理。
func wsTurnCh(ctx context.Context, h *held, question string) (string, string, error) {
	frame := map[string]any{
		"type": "response.create", "model": cfg.model, "store": false, "stream": true,
		"input": []any{map[string]any{
			"role":    "user",
			"content": []any{map[string]any{"type": "input_text", "text": question}},
		}},
	}
	payload, _ := json.Marshal(frame)
	if err := h.conn.WriteMessage(websocket.TextMessage, payload); err != nil {
		return "", "", fmt.Errorf("write frame: %w", err)
	}
	var answer, respID string
	for {
		select {
		case <-ctx.Done():
			return answer, respID, fmt.Errorf("turn timeout: %w", ctx.Err())
		case msg, ok := <-h.msgCh:
			if !ok {
				why, _ := h.closedWhy.Load().(string)
				return answer, respID, fmt.Errorf("connection closed by upstream: %s", why)
			}
			var ev map[string]any
			if json.Unmarshal(msg, &ev) != nil {
				continue
			}
			if cfg.verbose {
				logf("  [ws] %v", ev["type"])
			}
			switch ev["type"] {
			case "response.output_text.done":
				if t, ok := ev["text"].(string); ok {
					answer = t
				}
			case "response.created", "response.done", "response.completed":
				if r, ok := ev["response"].(map[string]any); ok && respID == "" {
					if id, ok := r["id"].(string); ok {
						respID = id
					}
				}
			case "error", "response.failed", "response.incomplete":
				raw, _ := json.Marshal(ev)
				return answer, respID, fmt.Errorf("upstream %v: %s", ev["type"], trim(string(raw)))
			}
			if answer != "" {
				return answer, respID, nil
			}
		}
	}
}

// ============================ 出口与头 ============================

func netDialThroughProxy(ctx context.Context, network, addr string) (net.Conn, error) {
	if strings.TrimSpace(cfg.proxyURL) == "" {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	u, err := url.Parse(cfg.proxyURL)
	if err != nil {
		return nil, err
	}
	return dialViaProxy(ctx, u, network, addr)
}

// dialViaProxy 按 scheme 建洞：ss 走原生隧道，socks5h/socks5 走 x/net/proxy。
func dialViaProxy(ctx context.Context, u *url.URL, network, addr string) (net.Conn, error) {
	switch strings.ToLower(u.Scheme) {
	case "ss":
		return tlsfingerprint.DialSSContext(ctx, u, addr)
	case "socks5", "socks5h":
		var auth *xproxy.Auth
		if u.User != nil {
			pw, _ := u.User.Password()
			auth = &xproxy.Auth{User: u.User.Username(), Password: pw}
		}
		host := u.Host
		if u.Port() == "" {
			host = net.JoinHostPort(u.Hostname(), "1080")
		}
		d, err := xproxy.SOCKS5("tcp", host, auth, &net.Dialer{Timeout: 20 * time.Second})
		if err != nil {
			return nil, err
		}
		type ctxDialer interface {
			DialContext(context.Context, string, string) (net.Conn, error)
		}
		if cd, ok := d.(ctxDialer); ok {
			return cd.DialContext(ctx, network, addr)
		}
		return d.Dial(network, addr)
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q (支持 ss / socks5h)", u.Scheme)
	}
}

func proxyClient() (*http.Client, error) {
	tr := &http.Transport{
		ForceAttemptHTTP2:     false,
		MaxIdleConnsPerHost:   2,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}
	if strings.TrimSpace(cfg.proxyURL) != "" {
		u, err := url.Parse(cfg.proxyURL)
		if err != nil {
			return nil, err
		}
		// 直接用生产的 utls 指纹拨号器：与 http_upstream.go 的 socks5/ss 分支同款，
		// 确保实验路径与真实转发路径一致（标准库 TLS 指纹会被 Cloudflare 拒）。
		tr.Proxy = nil
		tr.DialTLSContext = productionTLSDialer(u)
	}
	return &http.Client{Transport: tr, Timeout: 120 * time.Second}, nil
}

// utlsTLSClient 在已建好的隧道上完成 utls 指纹握手。
// 通过 tlsfingerprint 的导出拨号器实现（socks5h 用 NewSOCKS5ProxyDialer 会重复建洞，
// 所以这里用 UClient 直接握手）。
// productionTLSDialer 复用生产的 tlsfingerprint 拨号器（SS / SOCKS5 / HTTP 三种）。
// 与 http_upstream.go 的 buildUpstreamTransportWithTLSFingerprint 分支一一对应，
// 保证实验观测的就是真实转发所走的握手路径。
func productionTLSDialer(u *url.URL) func(context.Context, string, string) (net.Conn, error) {
	switch strings.ToLower(u.Scheme) {
	case "ss":
		return tlsfingerprint.NewSSProxyDialer(utlsProfile(), u).DialTLSContext
	case "socks5", "socks5h":
		return tlsfingerprint.NewSOCKS5ProxyDialer(utlsProfile(), u).DialTLSContext
	case "http", "https":
		return tlsfingerprint.NewHTTPProxyDialer(utlsProfile(), u).DialTLSContext
	default:
		return nil
	}
}

// utlsProfile 与生产 tlsfingerprint 默认档位同构（Node.js 24.x，ALPN 仅 http/1.1）。
func utlsProfile() *tlsfingerprint.Profile {
	return &tlsfingerprint.Profile{
		Name:              "wsurvive_node_v24",
		EnableGREASE:      true,
		ALPNProtocols:     []string{"http/1.1"},
		SupportedVersions: []uint16{0x0304, 0x0303},
	}
}

func applyUpstreamHeaders(req *http.Request) {
	req.Host = "chatgpt.com"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+cfg.token)
	req.Header.Set("Session_id", newUUID())
	req.Header.Set("Originator", "codex_cli_rs")
	req.Header.Set("Version", "0.155.1")
	req.Header.Set("User-Agent", "codex_cli_rs/0.155.1")
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	if cfg.chAcc != "" {
		req.Header.Set("Chatgpt-Account-Id", cfg.chAcc)
	}
}

// ============================ 工具 ============================

func parseSSEAnswer(r io.Reader) (string, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	seen := 0
	for sc.Scan() {
		line := sc.Text()
		if cfg.verbose && seen < 25 && strings.TrimSpace(line) != "" {
			logf("  [sse] %s", trim(line))
			seen++
		}
		payload := strings.TrimSpace(line)
		// SSE 行格式为 "data: {json}"，解析前必须剥掉 data: 前缀。
		if strings.HasPrefix(payload, "data:") {
			payload = strings.TrimSpace(strings.TrimPrefix(payload, "data:"))
		}
		if !strings.Contains(payload, `"type":"response.output_text.done"`) {
			continue
		}
		var ev struct {
			Text string `json:"text"`
		}
		if json.Unmarshal([]byte(payload), &ev) == nil && ev.Text != "" {
			return ev.Text, nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("SSE 未找到 output_text.done")
}

func classify(answer string) string {
	switch {
	case strings.Contains(answer, "高市"):
		return "full_power"
	case strings.Contains(answer, "石破"):
		return "degraded"
	default:
		return "unknown"
	}
}

func parseTimeline(s string) []int {
	var out []int
	for _, p := range strings.Split(s, ",") {
		var v int
		if _, err := fmt.Sscanf(strings.TrimSpace(p), "%d", &v); err == nil {
			out = append(out, v)
		}
	}
	return out
}

func waitUntil(t0 time.Time, offsetSec int) {
	target := t0.Add(time.Duration(offsetSec) * time.Second)
	if d := time.Until(target); d > 0 {
		time.Sleep(d)
	}
}

type results struct {
	mu  sync.Mutex
	f   *os.File
	out string
}

func (r *results) record(kind string, fields map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		f, err := os.OpenFile(r.out, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return
		}
		r.f = f
	}
	rec := map[string]any{"kind": kind, "at": time.Now().Format(time.RFC3339)}
	for k, v := range fields {
		rec[k] = v
	}
	b, _ := json.Marshal(rec)
	_, _ = r.f.Write(append(b, '\n'))
}

func (r *results) finish(reason string, summary map[string]any) {
	r.record("finish", map[string]any{"reason": reason, "summary": summary})
	logf("=== 实验结束: %s ===", reason)
}

func trim(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}

func redactProxy(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return "(直连)"
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "(unparsable)"
	}
	return fmt.Sprintf("%s://%s", u.Scheme, u.Host)
}

func logf(format string, args ...any) {
	fmt.Printf("%s "+format+"\n", append([]any{time.Now().Format("15:04:05")}, args...)...)
}

func fatal(format string, args ...any) {
	logf(format, args...)
	os.Exit(1)
}

// newUUID 生成 OAuth 探测所需的 Session_id（无外部依赖的 v4 实现）。
func newUUID() string {
	var b [16]byte
	seed := time.Now().UnixNano()
	for i := range b {
		seed = seed*6364136223846793005 + 1442695040888963407
		b[i] = byte(seed >> 33)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
