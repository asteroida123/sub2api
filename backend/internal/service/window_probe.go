package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/google/uuid"
)

// ============================================================================
// 窗口猎手 P0：指纹探针 + 账号×节点降智状态机
//
// 原理（292 实验，见 docs/window-hunter-gap-analysis.md 与 EXPERIMENT-LOG-292）：
//   - 上游风控标记按 (账号 × 计算节点) 缓存，节点静默 2-4h 后滚动清除；
//   - 节点没见过该账号 = 乐观窗口：首请求起 ~200s 内出满血模型【实测 183-236s】；
//   - 探测即污染：每次探测都会刷新节点上的标记时钟，窗口归零——状态机必须记录
//     last_probe_at 并禁止对同一 (账号,节点) 高频复探；
//   - 判定仪器：早苗式指纹问题（知识截止后可判定），经 stream:true 的
//     chatgpt.com/backend-api/codex/responses 端点，~25 token/发，答案二分类。
// ============================================================================

// 账号×节点健康状态。
// cooldown 不是落库状态：它是 degraded 且 now < cooldown_until 的派生显示态
// （见 AccountNodeHealth.EffectiveState），落库只记最后一次探测的真实结论。
const (
	NodeHealthStateUnknown   = "unknown"
	NodeHealthStateFullPower = "full_power"
	NodeHealthStateDegraded  = "degraded"
	NodeHealthStateCooldown  = "cooldown"
)

// DirectExitProxyID 表示"直连出口"（无代理）在 account_node_health 表中的 proxy_id 哨兵值。
const DirectExitProxyID int64 = 0

// lastProbeAnswerMaxBytes 限制落库的答案长度（答案应当只是姓名，超长说明异常）。
const lastProbeAnswerMaxBytes = 512

// WindowProbeResult 探测三态结论。
type WindowProbeResult string

const (
	WindowProbeFullPower WindowProbeResult = "full_power"
	WindowProbeDegraded  WindowProbeResult = "degraded"
	WindowProbeError     WindowProbeResult = "error"
)

// AccountNodeHealth 账号×出口节点的降智健康状态。
type AccountNodeHealth struct {
	ID              int64
	AccountID       int64
	ProxyID         int64 // 0 = 直连出口
	Region          string
	State           string
	WindowOpenedAt  *time.Time
	DegradedAt      *time.Time
	CooldownUntil   *time.Time
	ProbeCount      int64
	LastProbeAt     *time.Time
	LastProbeAnswer string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// EffectiveState 计算当前时点的派生状态（面板徽标与后续调度统一使用）：
//   - full_power 超过乐观窗口时长后视为降智（节点大概率已拉取标记，且探测即污染不允许复探确认）；
//   - degraded 且未过 cooldown_until 显示为 cooldown（剩余时间见 CooldownUntil）。
func (h *AccountNodeHealth) EffectiveState(now time.Time, windowDuration time.Duration) string {
	if h == nil {
		return NodeHealthStateUnknown
	}
	switch h.State {
	case NodeHealthStateFullPower:
		if h.WindowOpenedAt != nil && now.Sub(*h.WindowOpenedAt) >= windowDuration {
			return NodeHealthStateDegraded
		}
		return NodeHealthStateFullPower
	case NodeHealthStateDegraded:
		if h.CooldownUntil != nil && now.Before(*h.CooldownUntil) {
			return NodeHealthStateCooldown
		}
		return NodeHealthStateDegraded
	default:
		return NodeHealthStateUnknown
	}
}

// applyProbeOutcome 将一次探测结论应用到健康状态机（纯函数，单测覆盖）。
//
// 探测即污染：无论结论如何，只要探测发生就刷新 last_probe_at（节点侧窗口时钟归零）。
//   - full_power：窗口开启（window_opened_at=now），清除冷却；
//   - degraded：进入冷却（cooldown_until=now+冷却时长）；window_opened_at 保留作窗口统计；
//   - error：不改状态结论、不计题库轮换，仅记录探测时间。
func applyProbeOutcome(h *AccountNodeHealth, now time.Time, result WindowProbeResult, answer string, cooldown time.Duration) {
	if h == nil {
		return
	}
	h.LastProbeAt = &now
	if result == WindowProbeError {
		return
	}
	h.ProbeCount++
	h.LastProbeAnswer = truncateProbeAnswer(answer)
	switch result {
	case WindowProbeFullPower:
		h.State = NodeHealthStateFullPower
		h.WindowOpenedAt = &now
		h.DegradedAt = nil
		h.CooldownUntil = nil
	case WindowProbeDegraded:
		h.State = NodeHealthStateDegraded
		h.DegradedAt = &now
		cooldownUntil := now.Add(cooldown)
		h.CooldownUntil = &cooldownUntil
	}
}

func truncateProbeAnswer(answer string) string {
	if len(answer) <= lastProbeAnswerMaxBytes {
		return answer
	}
	trimmed := answer[:lastProbeAnswerMaxBytes]
	// 回退到最近的合法 UTF-8 边界，避免切断多字节字符
	for len(trimmed) > 0 {
		if r, size := utf8.DecodeLastRuneInString(trimmed); r != utf8.RuneError || size != 1 {
			break
		}
		trimmed = trimmed[:len(trimmed)-1]
	}
	return trimmed
}

// ============================================================================
// 题库与分类器
// ============================================================================

// WindowProbeQuestion 一道指纹题。
// 判定原则：知识截止后可判定（现任首脑类），答案二分类=满血（新知识）/降智（旧快照）。
type WindowProbeQuestion struct {
	ID                string   `json:"id"`
	Text              string   `json:"text"`
	FullPowerKeywords []string `json:"full_power_keywords"`
	DegradedKeywords  []string `json:"degraded_keywords"`
}

// DefaultWindowProbeQuestions 默认题库（3-5 道现任首脑类轮换）。
// 关键词按 2026-09 时点的现任/前任对照；分类器做归一化子串匹配，可被 settings 覆盖。
func DefaultWindowProbeQuestions() []WindowProbeQuestion {
	return []WindowProbeQuestion{
		{
			ID:                "jp_pm",
			Text:              "日本现在的首相是谁？只回答姓名",
			FullPowerKeywords: []string{"高市早苗", "高市", "たかいち", "takaichi"},
			DegradedKeywords:  []string{"石破茂", "石破", "いしば", "ishiba"},
		},
		{
			ID:                "us_president",
			Text:              "美国现在的总统是谁？只回答姓名",
			FullPowerKeywords: []string{"特朗普", "川普", "trump"},
			DegradedKeywords:  []string{"拜登", "biden"},
		},
		{
			ID:                "kr_president",
			Text:              "韩国现在的总统是谁？只回答姓名",
			FullPowerKeywords: []string{"李在明", "이재명", "leejaemyung", "leejae"},
			DegradedKeywords:  []string{"尹锡悦", "윤석열", "yoon"},
		},
		{
			ID:                "de_chancellor",
			Text:              "德国现在的总理是谁？只回答姓名",
			FullPowerKeywords: []string{"默茨", "merz"},
			DegradedKeywords:  []string{"朔尔茨", "舒尔茨", "肖尔茨", "scholz"},
		},
		{
			ID:                "ca_pm",
			Text:              "加拿大现在的总理是谁？只回答姓名",
			FullPowerKeywords: []string{"卡尼", "carney"},
			DegradedKeywords:  []string{"特鲁多", "特鲁多", "trudeau"},
		},
	}
}

// normalizeProbeAnswer 归一化答案/关键词：小写、去空白与常见中西文标点，
// 使 "Lee Jae-myung。" 与 "leejaemyung" 等价。
func normalizeProbeAnswer(s string) string {
	lower := strings.ToLower(s)
	var b strings.Builder
	b.Grow(len(lower))
	for _, r := range lower {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r < 0x80:
			// ASCII 其余（空格、标点、符号）全部丢弃
			continue
		default:
			// 非 ASCII：仅剔除常见中西文标点，文字（汉字/假名/韩文等）保留
			switch r {
			case '。', '、', '，', '．', '！', '？', '；', '：', '　', '·', '－', '—', '―', '‘', '’', '“', '”', '（', '）':
				continue
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

// classifyProbeAnswer 按题面关键词对照分类答案。
// 返回 WindowProbeFullPower / WindowProbeDegraded；无法判定时返回 ""（调用方按 error 处理，
// 不做状态转移——诚实优于猜测，原始答案已入库供人工判读）。
// 同时命中新旧两方关键词视为歧义，同样返回 ""。
func classifyProbeAnswer(answer string, question WindowProbeQuestion) WindowProbeResult {
	normalized := normalizeProbeAnswer(answer)
	if normalized == "" {
		return ""
	}
	fullHit := containsAnyProbeKeyword(normalized, question.FullPowerKeywords)
	degradedHit := containsAnyProbeKeyword(normalized, question.DegradedKeywords)
	switch {
	case fullHit && !degradedHit:
		return WindowProbeFullPower
	case degradedHit && !fullHit:
		return WindowProbeDegraded
	default:
		return ""
	}
}

func containsAnyProbeKeyword(normalized string, keywords []string) bool {
	for _, kw := range keywords {
		k := normalizeProbeAnswer(kw)
		if k != "" && strings.Contains(normalized, k) {
			return true
		}
	}
	return false
}

// ============================================================================
// 可配置项
// ============================================================================

// WindowProbeSettings 窗口探针配置（settings 表 JSON，key 见 window_probe_settings.go）。
type WindowProbeSettings struct {
	Model                   string                `json:"model"`
	Questions               []WindowProbeQuestion `json:"questions"`
	CooldownMinutes         int                   `json:"cooldown_minutes"`
	MinProbeIntervalSeconds int                   `json:"min_probe_interval_seconds"`
	WindowDurationSeconds   int                   `json:"window_duration_seconds"`
	ProbeTimeoutSeconds     int                   `json:"probe_timeout_seconds"`
}

// DefaultWindowProbeSettings 默认配置。
// 冷却默认 4h：实验测得节点标记静默 2-4h 滚动清除，取保守值。
func DefaultWindowProbeSettings() *WindowProbeSettings {
	return &WindowProbeSettings{
		Model:                   "gpt-6-astra",
		Questions:               DefaultWindowProbeQuestions(),
		CooldownMinutes:         240,
		MinProbeIntervalSeconds: 120,
		WindowDurationSeconds:   240,
		ProbeTimeoutSeconds:     90,
	}
}

// Normalize 校正非法配置值（读与写共用），保证后续取值方法安全。
func (s *WindowProbeSettings) Normalize() {
	if s.Model == "" {
		s.Model = "gpt-6-astra"
	}
	if len(s.Questions) == 0 {
		s.Questions = DefaultWindowProbeQuestions()
	}
	// 过滤缺关键词的题目
	filtered := s.Questions[:0]
	for _, q := range s.Questions {
		if strings.TrimSpace(q.Text) == "" || len(q.FullPowerKeywords) == 0 || len(q.DegradedKeywords) == 0 {
			continue
		}
		filtered = append(filtered, q)
	}
	if len(filtered) == 0 {
		filtered = DefaultWindowProbeQuestions()
	}
	s.Questions = filtered
	if s.CooldownMinutes < 1 {
		s.CooldownMinutes = 240
	}
	if s.CooldownMinutes > 7*24*60 {
		s.CooldownMinutes = 7 * 24 * 60
	}
	if s.MinProbeIntervalSeconds < 0 {
		s.MinProbeIntervalSeconds = 0
	}
	if s.MinProbeIntervalSeconds > 24*3600 {
		s.MinProbeIntervalSeconds = 24 * 3600
	}
	if s.WindowDurationSeconds < 30 {
		s.WindowDurationSeconds = 240
	}
	if s.WindowDurationSeconds > 3600 {
		s.WindowDurationSeconds = 3600
	}
	if s.ProbeTimeoutSeconds < 10 {
		s.ProbeTimeoutSeconds = 90
	}
	if s.ProbeTimeoutSeconds > 300 {
		s.ProbeTimeoutSeconds = 300
	}
}

// Cooldown 降智冷却时长。
func (s *WindowProbeSettings) Cooldown() time.Duration {
	return time.Duration(s.CooldownMinutes) * time.Minute
}

// WindowDuration 乐观窗口时长（满血态的自动失效时长）。
func (s *WindowProbeSettings) WindowDuration() time.Duration {
	return time.Duration(s.WindowDurationSeconds) * time.Second
}

// MinProbeInterval 同一 (账号,节点) 的最小复探间隔（探测即污染纪律）。
func (s *WindowProbeSettings) MinProbeInterval() time.Duration {
	return time.Duration(s.MinProbeIntervalSeconds) * time.Second
}

// ProbeTimeout 单次探测的上游超时。
func (s *WindowProbeSettings) ProbeTimeout() time.Duration {
	return time.Duration(s.ProbeTimeoutSeconds) * time.Second
}

// PickQuestion 按探测次数轮换选题。
func (s *WindowProbeSettings) PickQuestion(probeCount int64) WindowProbeQuestion {
	return s.Questions[int(probeCount%int64(len(s.Questions)))]
}

// ============================================================================
// Repository 接口（实现见 repository 包）
// ============================================================================

type WindowProbeRepository interface {
	// GetByAccountAndProxy 取 (账号,节点) 健康行；不存在返回 ErrNodeHealthNotFound。
	GetByAccountAndProxy(ctx context.Context, accountID, proxyID int64) (*AccountNodeHealth, error)
	// Upsert 按 (account_id, proxy_id) 唯一键写入/更新。
	Upsert(ctx context.Context, health *AccountNodeHealth) error
	// List 全量列出（表规模 = 账号×绑定出口，量小）。
	List(ctx context.Context) ([]*AccountNodeHealth, error)
	// ListByAccountIDs 按账号过滤。
	ListByAccountIDs(ctx context.Context, accountIDs []int64) ([]*AccountNodeHealth, error)
}

// ============================================================================
// 探针服务
// ============================================================================

// ErrWindowProbeTooFrequent 违反"探测即污染"复探纪律（同一节点间隔未到）。
var ErrWindowProbeTooFrequent = errors.New("window probe too frequent: probing pollutes the node's window clock")

// ErrNodeHealthNotFound 健康行不存在。
var ErrNodeHealthNotFound = errors.New("account node health not found")

// WindowProbeOutcome 单次探测的完整结论（含探测后的状态机快照）。
type WindowProbeOutcome struct {
	Result     WindowProbeResult  `json:"result"`
	Answer     string             `json:"answer"`
	LatencyMs  int64              `json:"latency_ms"`
	QuestionID string             `json:"question_id"`
	Question   string             `json:"question"`
	Message    string             `json:"message,omitempty"`
	State      string             `json:"state"`
	Health     *AccountNodeHealth `json:"health,omitempty"`
}

// WindowProbeService 指纹探针服务。
type WindowProbeService struct {
	accountRepo         AccountRepository
	proxyRepo           ProxyRepository
	healthRepo          WindowProbeRepository
	httpUpstream        HTTPUpstream
	tlsFPProfileService *TLSFingerprintProfileService
	settingService      *SettingService
	proxyLatencyCache   ProxyLatencyCache
}

// NewWindowProbeService 构造窗口探针服务。
func NewWindowProbeService(
	accountRepo AccountRepository,
	proxyRepo ProxyRepository,
	healthRepo WindowProbeRepository,
	httpUpstream HTTPUpstream,
	tlsFPProfileService *TLSFingerprintProfileService,
	settingService *SettingService,
	proxyLatencyCache ProxyLatencyCache,
) *WindowProbeService {
	return &WindowProbeService{
		accountRepo:         accountRepo,
		proxyRepo:           proxyRepo,
		healthRepo:          healthRepo,
		httpUpstream:        httpUpstream,
		tlsFPProfileService: tlsFPProfileService,
		settingService:      settingService,
		proxyLatencyCache:   proxyLatencyCache,
	}
}

// GetSettings 读取探针配置（未配置或配置损坏时回退默认值）。
func (s *WindowProbeService) GetSettings(ctx context.Context) (*WindowProbeSettings, error) {
	if s.settingService == nil {
		return DefaultWindowProbeSettings(), nil
	}
	return s.settingService.GetWindowProbeSettings(ctx)
}

// SetSettings 写回探针配置。
func (s *WindowProbeService) SetSettings(ctx context.Context, settings *WindowProbeSettings) error {
	if s.settingService == nil {
		return errors.New("setting service unavailable")
	}
	return s.settingService.SetWindowProbeSettings(ctx, settings)
}

// ListHealth 全量列出健康状态（面板徽标数据源）。
func (s *WindowProbeService) ListHealth(ctx context.Context) ([]*AccountNodeHealth, error) {
	if s == nil || s.healthRepo == nil {
		return nil, errors.New("window probe service not initialized")
	}
	return s.healthRepo.List(ctx)
}

// ListHealthByAccountIDs 按账号列出健康状态。
func (s *WindowProbeService) ListHealthByAccountIDs(ctx context.Context, accountIDs []int64) ([]*AccountNodeHealth, error) {
	if s == nil || s.healthRepo == nil {
		return nil, errors.New("window probe service not initialized")
	}
	return s.healthRepo.ListByAccountIDs(ctx, accountIDs)
}

// ProbeAccount 对指定账号的指定出口做一次指纹探测，并落库状态机。
//
// proxyID 语义：
//   - 显式指定：按指定出口探测（P1 猎手使用）；
//   - 未指定：用账号当前绑定的代理；无绑定则直连出口。
//
// force=true 允许无视最小复探间隔（面板手动按钮；探测即污染的后果由操作者自担）。
func (s *WindowProbeService) ProbeAccount(ctx context.Context, accountID int64, proxyIDParam *int64, force bool) (*WindowProbeOutcome, error) {
	if s == nil || s.healthRepo == nil {
		return nil, errors.New("window probe service not initialized")
	}

	settings, err := s.GetSettings(ctx)
	if err != nil {
		return nil, fmt.Errorf("get window probe settings: %w", err)
	}

	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return nil, err
	}

	credAccount := account
	if account.IsCredentialShadow() {
		credAccount, err = resolveCredentialAccount(ctx, s.accountRepo, account)
		if err != nil {
			return nil, fmt.Errorf("resolve credential account: %w", err)
		}
	}
	if credAccount.Platform != PlatformOpenAI || !credAccount.IsOAuth() {
		return nil, errors.New("window probe only supports OpenAI OAuth accounts")
	}

	proxyID, proxy, err := s.resolveTargetProxy(ctx, account, proxyIDParam)
	if err != nil {
		return nil, err
	}

	health, err := s.healthRepo.GetByAccountAndProxy(ctx, accountID, proxyID)
	if err != nil {
		if !errors.Is(err, ErrNodeHealthNotFound) {
			return nil, fmt.Errorf("get node health: %w", err)
		}
		health = &AccountNodeHealth{
			AccountID: accountID,
			ProxyID:   proxyID,
			State:     NodeHealthStateUnknown,
		}
	}

	now := time.Now()
	if !force && health.LastProbeAt != nil {
		if elapsed := now.Sub(*health.LastProbeAt); elapsed < settings.MinProbeInterval() {
			return nil, fmt.Errorf("%w (last probe %s ago, min interval %s)",
				ErrWindowProbeTooFrequent, elapsed.Round(time.Second), settings.MinProbeInterval())
		}
	}

	question := settings.PickQuestion(health.ProbeCount)
	outcome := &WindowProbeOutcome{
		QuestionID: question.ID,
		Question:   question.Text,
	}

	answer, latencyMs, probeErr := s.sendFingerprintProbe(ctx, credAccount, proxy, question.Text, settings)
	outcome.Answer = answer
	outcome.LatencyMs = latencyMs

	health.Region = s.resolveProxyRegion(ctx, proxyID)

	switch {
	case probeErr != nil:
		outcome.Result = WindowProbeError
		outcome.Message = probeErr.Error()
	default:
		classified := classifyProbeAnswer(answer, question)
		switch classified {
		case WindowProbeFullPower, WindowProbeDegraded:
			outcome.Result = classified
		default:
			outcome.Result = WindowProbeError
			outcome.Message = "unclassifiable answer"
		}
	}

	applyProbeOutcome(health, now, outcome.Result, answer, settings.Cooldown())
	outcome.State = health.EffectiveState(now, settings.WindowDuration())

	if err := s.healthRepo.Upsert(ctx, health); err != nil {
		return nil, fmt.Errorf("save node health: %w", err)
	}
	outcome.Health = health
	return outcome, nil
}

// resolveTargetProxy 决定本次探测的出口：(账号,proxyID) 二元组由此唯一确定。
func (s *WindowProbeService) resolveTargetProxy(ctx context.Context, account *Account, proxyIDParam *int64) (int64, *Proxy, error) {
	// 显式指定出口（含显式直连）
	if proxyIDParam != nil {
		proxyID := *proxyIDParam
		if proxyID == DirectExitProxyID {
			return DirectExitProxyID, nil, nil
		}
		if s.proxyRepo == nil {
			return 0, nil, errors.New("proxy repository unavailable")
		}
		proxy, err := s.proxyRepo.GetByID(ctx, proxyID)
		if err != nil {
			return 0, nil, err
		}
		return proxyID, proxy, nil
	}

	// 未指定：跟随账号绑定
	if account.ProxyID == nil || *account.ProxyID == DirectExitProxyID {
		return DirectExitProxyID, nil, nil
	}
	proxyID := *account.ProxyID
	proxy := account.Proxy
	if proxy == nil && s.proxyRepo != nil {
		var err error
		proxy, err = s.proxyRepo.GetByID(ctx, proxyID)
		if err != nil {
			return 0, nil, err
		}
	}
	return proxyID, proxy, nil
}

// resolveProxyRegion 尽力填充出口地理快照（来自代理质量检测的延迟缓存）。
func (s *WindowProbeService) resolveProxyRegion(ctx context.Context, proxyID int64) string {
	if proxyID == DirectExitProxyID {
		return "direct"
	}
	if s.proxyLatencyCache == nil {
		return ""
	}
	infos, err := s.proxyLatencyCache.GetProxyLatencies(ctx, []int64{proxyID})
	if err != nil || infos == nil {
		return ""
	}
	info := infos[proxyID]
	if info == nil {
		return ""
	}
	parts := make([]string, 0, 3)
	if info.CountryCode != "" {
		parts = append(parts, info.CountryCode)
	}
	if info.Region != "" && info.Region != info.CountryCode {
		parts = append(parts, info.Region)
	}
	region := strings.Join(parts, "/")
	if len(region) > 64 {
		region = region[:64]
	}
	return region
}

// sendFingerprintProbe 发送一发指纹探测并解析 SSE 答案。
//
// 请求形态与实验仪器（EXPERIMENT-LOG-292 §5 probe 函数）逐项对齐：
// stream:true POST chatgpt.com/backend-api/codex/responses，
// headers: Authorization / Session_id / Chatgpt-Account-Id / Version / User-Agent / Originator / OpenAI-Beta。
// 注意 stream 必须 true（false 上游直接 400）。
func (s *WindowProbeService) sendFingerprintProbe(ctx context.Context, account *Account, proxy *Proxy, question string, settings *WindowProbeSettings) (string, int64, error) {
	authToken := account.GetOpenAIAccessToken()
	if authToken == "" {
		return "", 0, errors.New("no access token available")
	}

	ctx, cancel := context.WithTimeout(ctx, settings.ProbeTimeout())
	defer cancel()

	payload := map[string]any{
		"model":  settings.Model,
		"store":  false,
		"stream": true,
		"input": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": question},
				},
			},
		},
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return "", 0, fmt.Errorf("marshal probe payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatgptCodexAPIURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return "", 0, fmt.Errorf("create probe request: %w", err)
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))

	// 与真实转发/账号测试相同的 OAuth 探测头
	req.Host = "chatgpt.com"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+authToken)
	req.Header.Set("Session_id", uuid.NewString())
	identity := resolveCodexOutboundIdentity("")
	req.Header.Set("Originator", identity.originator)
	req.Header.Set("Version", identity.version)
	if customUA := strings.TrimSpace(account.GetOpenAIUserAgent()); customUA != "" {
		req.Header.Set("User-Agent", customUA)
	} else {
		req.Header.Set("User-Agent", identity.userAgent)
	}
	setOpenAIChatGPTAccountHeaders(req.Header, account)
	enforceCodexIdentityHeadersWithUA(req.Header, account.GetOpenAIUserAgent())

	proxyURL := ""
	if proxy != nil {
		proxyURL = proxy.URL()
	}

	var profile *tlsfingerprint.Profile
	if s.tlsFPProfileService != nil {
		profile = s.tlsFPProfileService.ResolveTLSProfile(account)
	}

	start := time.Now()
	resp, err := s.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, profile)
	latencyMs := time.Since(start).Milliseconds()
	if err != nil {
		return "", latencyMs, fmt.Errorf("upstream request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", latencyMs, fmt.Errorf("upstream returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	answer, err := parseCodexProbeAnswer(resp.Body)
	return answer, latencyMs, err
}

// parseCodexProbeAnswer 解析 codex/responses 的 SSE 流，取首个 output_text.done 文本。
// 等价于实验仪器里的 `grep '"type":"response.output_text.done"' | head -1`。
func parseCodexProbeAnswer(body io.Reader) (string, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var evt struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Code     string `json:"code"`
			Message  string `json:"message"`
			Response *struct {
				Status string `json:"status"`
				Error  *struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(data), &evt); err != nil {
			continue
		}
		switch evt.Type {
		case "response.output_text.done":
			if strings.TrimSpace(evt.Text) != "" {
				return evt.Text, nil
			}
		case "error":
			return "", fmt.Errorf("probe stream error: %s", streamErrorMessage(evt.Code, evt.Message))
		case "response.failed":
			if evt.Response != nil && evt.Response.Error != nil {
				return "", fmt.Errorf("probe stream failed: %s", streamErrorMessage(evt.Response.Error.Code, evt.Response.Error.Message))
			}
			return "", errors.New("probe stream failed")
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read probe stream: %w", err)
	}
	return "", errors.New("no answer in probe stream")
}

func streamErrorMessage(code, message string) string {
	if code != "" && message != "" {
		return code + ": " + message
	}
	if code != "" {
		return code
	}
	if message != "" {
		return message
	}
	return "unknown"
}
