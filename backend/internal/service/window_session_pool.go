package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// ============================================================================
// 窗口猎手 P2：满血会话池
//
// 语义（由 P2 验收实验裁定，见 docs/window-hunter-p2-experiment.md）：
//   - 窗口命中后立即预建 N 条独立上游 WS 会话（每条打 full_power_until 标记）；
//   - 采样器每 N 分钟对每条会话注入一发指纹 turn（~25 token）；
//   - 采样判降智 → 立即退役该条 + 回写 (账号,出口) 健康状态机 degraded；
//   - 业务 Acquire 优先取满血条目；门控模型可配置为"无满血即拒绝"。
//
// 实验背景（EXPERIMENT-LOG-292 §9）：节点标记到达后立即作用于已建立会话——
// "会话豁免"被实测证伪。若验收实验同样证伪池内存活，则本层的满血语义退化为
// "established+会话寿命（默认 60min）"的强制寿命实现（代码结构不变，仅文档标注）。
// ============================================================================

const SettingKeyWindowSessionPoolSettings = "window_session_pool_settings"

// WindowSessionPoolSettings 满血会话池配置。
type WindowSessionPoolSettings struct {
	// SamplingEnabled 指纹采样循环开关。
	SamplingEnabled bool `json:"sampling_enabled"`
	// SampleIntervalSeconds 单条会话的采样间隔，默认 300。
	SampleIntervalSeconds int `json:"sample_interval_seconds"`
	// SessionLifetimeMinutes 满血标记寿命（established+寿命），默认 60；同时受池 60min
	// 硬寿命（openAIWSConnMaxAge）约束，取两者较小。
	SessionLifetimeMinutes int `json:"session_lifetime_minutes"`
	// PrewarmCount 预建会话条数（验收实验 ≥3），默认 3。
	PrewarmCount int `json:"prewarm_count"`
	// SampleFailThreshold 连续采样失败多少次后撤销满血标记，默认 2。
	SampleFailThreshold int `json:"sample_fail_threshold"`
	// GateModels 门控模型列表（映射后的上游模型名，前缀匹配）。
	GateModels []string `json:"gate_models"`
	// RejectGatedWhenNoFullPower 门控模型在无满血会话时拒绝（false=按常规路径放行）。
	RejectGatedWhenNoFullPower bool `json:"reject_gated_when_no_full_power"`
}

// DefaultWindowSessionPoolSettings 默认配置。
func DefaultWindowSessionPoolSettings() *WindowSessionPoolSettings {
	return &WindowSessionPoolSettings{
		SamplingEnabled:            false,
		SampleIntervalSeconds:      300,
		SessionLifetimeMinutes:     60,
		PrewarmCount:               3,
		SampleFailThreshold:        2,
		GateModels:                 []string{"gpt-6-astra"},
		RejectGatedWhenNoFullPower: false,
	}
}

// Normalize 校正配置。
func (s *WindowSessionPoolSettings) Normalize() {
	if s.SampleIntervalSeconds < 15 {
		s.SampleIntervalSeconds = 300
	}
	if s.SampleIntervalSeconds > 3600 {
		s.SampleIntervalSeconds = 3600
	}
	if s.SessionLifetimeMinutes < 5 {
		s.SessionLifetimeMinutes = 60
	}
	if s.SessionLifetimeMinutes > 120 {
		s.SessionLifetimeMinutes = 120
	}
	if s.PrewarmCount < 1 {
		s.PrewarmCount = 3
	}
	if s.PrewarmCount > 10 {
		s.PrewarmCount = 10
	}
	if s.SampleFailThreshold < 1 {
		s.SampleFailThreshold = 2
	}
	if len(s.GateModels) == 0 {
		s.GateModels = []string{"gpt-6-astra"}
	}
}

// SampleInterval 采样间隔。
func (s *WindowSessionPoolSettings) SampleInterval() time.Duration {
	return time.Duration(s.SampleIntervalSeconds) * time.Second
}

// SessionLifetime 满血寿命。
func (s *WindowSessionPoolSettings) SessionLifetime() time.Duration {
	return time.Duration(s.SessionLifetimeMinutes) * time.Minute
}

// MatchesGateModel 上游模型名是否命中门控列表（前缀匹配，大小写不敏感）。
func (s *WindowSessionPoolSettings) MatchesGateModel(model string) bool {
	normalized := strings.ToLower(strings.TrimSpace(model))
	if normalized == "" {
		return false
	}
	for _, gate := range s.GateModels {
		g := strings.ToLower(strings.TrimSpace(gate))
		if g == "" {
			continue
		}
		if normalized == g || strings.HasPrefix(normalized, g+"-") || strings.HasPrefix(normalized, g+"_") {
			return true
		}
	}
	return false
}

func (s *SettingService) getWindowSessionPoolSettings(ctx context.Context) (*WindowSessionPoolSettings, error) {
	if s == nil || s.settingRepo == nil {
		return DefaultWindowSessionPoolSettings(), nil
	}
	value, err := s.settingRepo.GetValue(ctx, SettingKeyWindowSessionPoolSettings)
	if err != nil {
		if err == ErrSettingNotFound {
			return DefaultWindowSessionPoolSettings(), nil
		}
		return nil, fmt.Errorf("get window session pool settings: %w", err)
	}
	if strings.TrimSpace(value) == "" {
		return DefaultWindowSessionPoolSettings(), nil
	}
	var settings WindowSessionPoolSettings
	if err := json.Unmarshal([]byte(value), &settings); err != nil {
		return DefaultWindowSessionPoolSettings(), nil
	}
	settings.Normalize()
	return &settings, nil
}

// GetWindowSessionPoolSettings 读取满血会话池配置。
func (s *SettingService) GetWindowSessionPoolSettings(ctx context.Context) (*WindowSessionPoolSettings, error) {
	return s.getWindowSessionPoolSettings(ctx)
}

// SetWindowSessionPoolSettings 写回满血会话池配置。
func (s *SettingService) SetWindowSessionPoolSettings(ctx context.Context, settings *WindowSessionPoolSettings) error {
	if s == nil || s.settingRepo == nil {
		return errors.New("setting repository unavailable")
	}
	if settings == nil {
		return errors.New("settings cannot be nil")
	}
	settings.Normalize()
	data, err := json.Marshal(settings)
	if err != nil {
		return fmt.Errorf("marshal window session pool settings: %w", err)
	}
	return s.settingRepo.Set(ctx, SettingKeyWindowSessionPoolSettings, string(data))
}

// ============================================================================
// 采样事件
// ============================================================================

// WindowSessionSampleEvent 单次会话指纹采样的记录。
type WindowSessionSampleEvent struct {
	ID        string            `json:"id"`
	AccountID int64             `json:"account_id"`
	ConnID    string            `json:"conn_id"`
	ProxyID   int64             `json:"proxy_id"`
	At        time.Time         `json:"at"`
	Result    WindowProbeResult `json:"result"`
	Answer    string            `json:"answer,omitempty"`
	Message   string            `json:"message,omitempty"`
	Retired   bool              `json:"retired"`
}

const windowSessionSampleLogCapacity = 100

// ============================================================================
// 服务
// ============================================================================

// WindowSessionPoolService 满血会话池采样与调度策略。
type WindowSessionPoolService struct {
	accountRepo    AccountRepository
	healthRepo     WindowProbeRepository
	settingService *SettingService
	gateway        *OpenAIGatewayService

	stopCh  chan struct{}
	started atomic.Bool

	sampleCounterMu sync.Mutex
	sampleCounters  map[string]int64 // connID → 已采样次数（题库轮换）

	mu      sync.Mutex
	samples []*WindowSessionSampleEvent

	// settings 热路径缓存（AcquirePolicy 每请求调用，60s TTL）
	policyMu      sync.Mutex
	policyCache   *WindowSessionPoolSettings
	policyExpires time.Time
}

// NewWindowSessionPoolService 构造满血会话池服务。
func NewWindowSessionPoolService(
	accountRepo AccountRepository,
	healthRepo WindowProbeRepository,
	settingService *SettingService,
	gateway *OpenAIGatewayService,
) *WindowSessionPoolService {
	return &WindowSessionPoolService{
		accountRepo:    accountRepo,
		healthRepo:     healthRepo,
		settingService: settingService,
		gateway:        gateway,
		stopCh:         make(chan struct{}),
		sampleCounters: map[string]int64{},
	}
}

func (s *WindowSessionPoolService) pool() *openAIWSConnPool {
	if s == nil || s.gateway == nil {
		return nil
	}
	return s.gateway.getOpenAIWSConnPool()
}

// GetSettings 读取配置。
func (s *WindowSessionPoolService) GetSettings(ctx context.Context) (*WindowSessionPoolSettings, error) {
	if s == nil || s.settingService == nil {
		return DefaultWindowSessionPoolSettings(), nil
	}
	return s.settingService.GetWindowSessionPoolSettings(ctx)
}

// SetSettings 写回配置。
func (s *WindowSessionPoolService) SetSettings(ctx context.Context, settings *WindowSessionPoolSettings) error {
	if s == nil || s.settingService == nil {
		return errors.New("setting service unavailable")
	}
	if err := s.settingService.SetWindowSessionPoolSettings(ctx, settings); err != nil {
		return err
	}
	s.policyMu.Lock()
	s.policyCache = nil
	s.policyExpires = time.Time{}
	s.policyMu.Unlock()
	return nil
}

// cachedSettings 带缓存的配置读取（采样调度与请求热路径共用）。
func (s *WindowSessionPoolService) cachedSettings(ctx context.Context) *WindowSessionPoolSettings {
	s.policyMu.Lock()
	defer s.policyMu.Unlock()
	now := time.Now()
	if s.policyCache != nil && now.Before(s.policyExpires) {
		return s.policyCache
	}
	settings := DefaultWindowSessionPoolSettings()
	if s.settingService != nil {
		if fetched, err := s.settingService.GetWindowSessionPoolSettings(ctx); err == nil && fetched != nil {
			settings = fetched
		}
	}
	s.policyCache = settings
	s.policyExpires = now.Add(60 * time.Second)
	return settings
}

// AcquirePolicyForModel 请求热路径策略：该（映射后）模型是否要求满血会话。
func (s *WindowSessionPoolService) AcquirePolicyForModel(mappedModel string) bool {
	if s == nil {
		return false
	}
	settings := s.cachedSettings(context.Background())
	if !settings.RejectGatedWhenNoFullPower {
		return false
	}
	return settings.MatchesGateModel(mappedModel)
}

// Start 启动采样循环。
func (s *WindowSessionPoolService) Start() {
	if s == nil {
		return
	}
	if !s.started.CompareAndSwap(false, true) {
		return
	}
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-s.stopCh:
				return
			case <-ticker.C:
				s.sweep(context.Background())
			}
		}
	}()
}

// sweep 采样一轮：并发 1 纪律，逐条检查到期会话。
func (s *WindowSessionPoolService) sweep(ctx context.Context) {
	settings := s.cachedSettings(ctx)
	if !settings.SamplingEnabled {
		return
	}
	pool := s.pool()
	if pool == nil {
		return
	}
	now := time.Now()
	for _, snapshot := range pool.SnapshotConns(0, true) {
		if ctx.Err() != nil {
			return
		}
		if snapshot.Degraded || snapshot.Leased || !snapshot.IsFullPower {
			continue
		}
		if snapshot.LastSampleAt != nil && now.Sub(*snapshot.LastSampleAt) < settings.SampleInterval() {
			continue
		}
		s.SampleConn(ctx, snapshot.AccountID, snapshot.ConnID)
	}
}

// AcquireForSampling 借出指定连接用于采样（绕过兼容性匹配——采样点名精确连接）。
func (p *openAIWSConnPool) AcquireForSampling(accountID int64, connID string) (*openAIWSConnLease, error) {
	if p == nil || accountID <= 0 || connID == "" {
		return nil, errors.New("invalid sampling acquire")
	}
	ap, ok := p.getAccountPool(accountID)
	if !ok || ap == nil {
		return nil, errOpenAIWSConnClosed
	}
	ap.mu.Lock()
	conn := ap.conns[connID]
	ap.mu.Unlock()
	if conn == nil || conn.degradedFlag.Load() {
		return nil, errOpenAIWSConnClosed
	}
	if !conn.tryAcquire() {
		return nil, errOpenAIWSConnQueueFull
	}
	p.metrics.acquireTotal.Add(1)
	return &openAIWSConnLease{pool: p, accountID: accountID, conn: conn, reused: true}, nil
}

// SampleConn 对一条会话注入一发指纹 turn 并按结论处置。
func (s *WindowSessionPoolService) SampleConn(ctx context.Context, accountID int64, connID string) *WindowSessionSampleEvent {
	pool := s.pool()
	if pool == nil {
		return nil
	}
	event := &WindowSessionSampleEvent{
		ID:        uuid.NewString(),
		AccountID: accountID,
		ConnID:    connID,
		At:        time.Now(),
	}

	probeSettings := DefaultWindowProbeSettings()
	if s.settingService != nil {
		if fetched, err := s.settingService.GetWindowProbeSettings(ctx); err == nil && fetched != nil {
			probeSettings = fetched
		}
	}
	question := probeSettings.PickQuestion(s.nextSampleCount(connID))

	lease, acquireErr := pool.AcquireForSampling(accountID, connID)
	if acquireErr != nil {
		event.Result = WindowProbeError
		event.Message = fmt.Sprintf("acquire for sampling: %v", acquireErr)
		s.recordSample(event)
		return event
	}
	cleanExit := false
	defer func() {
		if !cleanExit {
			lease.MarkBroken()
		}
		lease.Release()
	}()
	conn := lease.conn
	if conn != nil {
		event.ProxyID = conn.ProxyID()
	}

	answer, sampleErr := s.runFingerprintTurn(ctx, lease, question.Text, probeSettings)
	event.Result = WindowProbeError
	if sampleErr != nil {
		event.Message = sampleErr.Error()
	} else {
		switch classified := classifyProbeAnswer(answer, question); classified {
		case WindowProbeFullPower, WindowProbeDegraded:
			event.Result = classified
			event.Answer = answer
		default:
			event.Message = "unclassifiable answer"
			event.Answer = answer
		}
	}

	now := time.Now()
	settings := s.cachedSettings(ctx)
	switch event.Result {
	case WindowProbeFullPower:
		conn.markSampled(now, answer, false)
	case WindowProbeDegraded:
		// 采样劣化：清除满血标记 + 退役 + 回写状态机
		conn.markSampled(now, answer, true)
		event.Retired = true
		cleanExit = true
		lease.MarkBroken() // 劣化连接不再回池复用
		pool.RetireDegradedConn(accountID, connID)
		s.writeBackDegraded(ctx, accountID, event.ProxyID, answer, now, probeSettings.Cooldown())
	default:
		conn.recordSampleFailure(now, settings.SampleFailThreshold)
	}

	s.recordSample(event)
	return event
}

func (s *WindowSessionPoolService) nextSampleCount(connID string) int64 {
	s.sampleCounterMu.Lock()
	defer s.sampleCounterMu.Unlock()
	s.sampleCounters[connID]++
	return s.sampleCounters[connID] - 1
}

// runFingerprintTurn 在已借出的 WS 会话上发送指纹 turn 并读取回答。
// 帧协议：response.create 字段与 HTTP /responses 一致（EXPERIMENT-LOG-292 §8/§9 的 WS 实验）。
func (s *WindowSessionPoolService) runFingerprintTurn(ctx context.Context, lease *openAIWSConnLease, question string, probeSettings *WindowProbeSettings) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, probeSettings.ProbeTimeout())
	defer cancel()

	payload := map[string]any{
		"type":   "response.create",
		"model":  probeSettings.Model,
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
	if err := lease.WriteJSONWithContextTimeout(ctx, payload, openAIWSSampleWriteTimeout); err != nil {
		return "", fmt.Errorf("write sample turn: %w", err)
	}

	var answer string
	for {
		if ctx.Err() != nil {
			return answer, fmt.Errorf("sample turn timeout: %w", ctx.Err())
		}
		message, readErr := lease.ReadMessageWithContextTimeout(ctx, openAIWSSampleReadTimeout)
		if readErr != nil {
			return answer, fmt.Errorf("read sample stream: %w", readErr)
		}
		var evt struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(message, &evt) != nil {
			continue
		}
		switch evt.Type {
		case "response.output_text.done":
			if strings.TrimSpace(evt.Text) != "" {
				answer = evt.Text
			}
		case "response.completed", "response.incomplete":
			if strings.TrimSpace(answer) != "" {
				return answer, nil
			}
			return "", errors.New("sample turn finished without answer")
		case "response.failed", "error":
			return answer, fmt.Errorf("sample turn stream error: %s", truncateOpenAIWSLogValue(string(message), 256))
		}
	}
}

const (
	openAIWSSampleReadTimeout  = 30 * time.Second
	openAIWSSampleWriteTimeout = 10 * time.Second
)

// writeBackDegraded 把采样劣化结论写回 (账号,出口) 健康状态机。
func (s *WindowSessionPoolService) writeBackDegraded(ctx context.Context, accountID, proxyID int64, answer string, now time.Time, cooldown time.Duration) {
	if s == nil || s.healthRepo == nil {
		return
	}
	health, err := s.healthRepo.GetByAccountAndProxy(ctx, accountID, proxyID)
	if err != nil {
		if !errors.Is(err, ErrNodeHealthNotFound) {
			return
		}
		health = &AccountNodeHealth{AccountID: accountID, ProxyID: proxyID, State: NodeHealthStateUnknown}
	}
	applyProbeOutcome(health, now, WindowProbeDegraded, answer, cooldown)
	_ = s.healthRepo.Upsert(ctx, health)
}

func (s *WindowSessionPoolService) recordSample(event *WindowSessionSampleEvent) {
	if event == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.samples = append(s.samples, event)
	if len(s.samples) > windowSessionSampleLogCapacity {
		s.samples = s.samples[len(s.samples)-windowSessionSampleLogCapacity:]
	}
}

// Prewarm 为目标账号预建 N 条满血会话（窗口命中后调用）。
func (s *WindowSessionPoolService) Prewarm(ctx context.Context, accountID int64, count int) ([]string, error) {
	pool := s.pool()
	if pool == nil {
		return nil, errors.New("ws pool unavailable")
	}
	if s.accountRepo != nil {
		account, err := s.accountRepo.GetByID(ctx, accountID)
		if err != nil {
			return nil, err
		}
		if account.Platform != PlatformOpenAI || !account.IsOAuth() {
			return nil, errors.New("prewarm only supports OpenAI OAuth accounts")
		}
	}
	settings := s.cachedSettings(ctx)
	if count <= 0 {
		count = settings.PrewarmCount
	}
	base, ok := pool.LastAcquireRequest(accountID)
	if !ok {
		return nil, errors.New("no recent ws acquire for account: run at least one business request first (prewarm reuses its handshake headers)")
	}
	until := time.Now().Add(settings.SessionLifetime())
	return pool.PrewarmFullPowerConns(ctx, base, count, until)
}

// LastAcquireRequest 返回账号最近一次成功的 acquire 请求（prewarm 借其握手头拨新连接）。
func (p *openAIWSConnPool) LastAcquireRequest(accountID int64) (openAIWSAcquireRequest, bool) {
	if p == nil || accountID <= 0 {
		return openAIWSAcquireRequest{}, false
	}
	ap, ok := p.getAccountPool(accountID)
	if !ok || ap == nil {
		return openAIWSAcquireRequest{}, false
	}
	ap.mu.Lock()
	defer ap.mu.Unlock()
	if ap.lastAcquire == nil {
		return openAIWSAcquireRequest{}, false
	}
	return cloneOpenAIWSAcquireRequest(*ap.lastAcquire), true
}

// Snapshot 返回池内满血会话视图 + 最近采样记录（面板/验收实验观测口）。
func (s *WindowSessionPoolService) Snapshot() ([]OpenAIWSConnHealthSnapshot, []*WindowSessionSampleEvent) {
	if s == nil {
		return nil, nil
	}
	pool := s.pool()
	var sessions []OpenAIWSConnHealthSnapshot
	if pool != nil {
		sessions = pool.SnapshotConns(0, true)
	}
	s.mu.Lock()
	samples := make([]*WindowSessionSampleEvent, len(s.samples))
	copy(samples, s.samples)
	s.mu.Unlock()
	return sessions, samples
}

// openAIWSFullPowerUnavailableError 门控模型在"无满血即拒绝"策略下无满血会话可用。
// 不走 HTTP 回退（回退即接受降智供货，与门控语义相悖），直接以错误终止该次请求。
type openAIWSFullPowerUnavailableError struct {
	Model string
}

func (e *openAIWSFullPowerUnavailableError) Error() string {
	if e == nil {
		return "no full-power session available"
	}
	return fmt.Sprintf("no full-power session available for gated model %s (window hunter: prewarm sessions first or disable reject_gated_when_no_full_power)", e.Model)
}

// SetWindowSessionPoolService 注入满血会话池（wire 装配）。
func (s *OpenAIGatewayService) SetWindowSessionPoolService(pool *WindowSessionPoolService) {
	if s == nil {
		return
	}
	s.windowSessionPool = pool
}
