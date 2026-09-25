package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ============================================================================
// 窗口猎手（Window Hunter）P1：轮换编排器
//
// 行为纪律（实验得出，违反即浪费窗口）：
//   - 并发 1：同一时间只发一发探测；
//   - 出口去重：同 /24 出口视为同一节点（实测 22 个同段 IP = 1 个节点）；
//   - 命中即停：找到满血出口立即结束本轮；
//   - 全员未命中 → 本轮结束进入退避，轮次退避：间隔逐轮加倍（经验：首轮 90+ 命中、
//     二轮 40、三轮 5——窗口是稀缺资源，扫完一轮就该歇）。
// ============================================================================

// WindowHunterSettings 猎手配置（settings 表 JSON，key 见 Get/SetWindowHunterSettings）。
type WindowHunterSettings struct {
	// AutoEnabled 自动模式：后台循环按退避节奏自动开轮。
	AutoEnabled bool `json:"auto_enabled"`
	// TargetAccountID 狩猎目标账号（OpenAI OAuth）。
	TargetAccountID int64 `json:"target_account_id"`
	// ProxyIDs 候选出口；空 = 全部 active 代理。
	ProxyIDs []int64 `json:"proxy_ids"`
	// BaseIntervalMinutes 退避基准间隔（首轮未命中后等待时长），默认 30。
	BaseIntervalMinutes int `json:"base_interval_minutes"`
	// MaxIntervalMinutes 退避上限，默认 240（≈标记滚动清除周期）。
	MaxIntervalMinutes int `json:"max_interval_minutes"`
	// MaxCandidatesPerRound 单轮候选上限（安全阀，防止大池子失控消耗配额），默认 50。
	MaxCandidatesPerRound int `json:"max_candidates_per_round"`
}

// DefaultWindowHunterSettings 默认配置。
func DefaultWindowHunterSettings() *WindowHunterSettings {
	return &WindowHunterSettings{
		AutoEnabled:           false,
		TargetAccountID:       0,
		ProxyIDs:              nil,
		BaseIntervalMinutes:   30,
		MaxIntervalMinutes:    240,
		MaxCandidatesPerRound: 50,
	}
}

// Normalize 校正非法配置值。
func (s *WindowHunterSettings) Normalize() {
	if s.BaseIntervalMinutes < 1 {
		s.BaseIntervalMinutes = 30
	}
	if s.BaseIntervalMinutes > 24*60 {
		s.BaseIntervalMinutes = 24 * 60
	}
	if s.MaxIntervalMinutes < s.BaseIntervalMinutes {
		s.MaxIntervalMinutes = 240
	}
	if s.MaxIntervalMinutes > 7*24*60 {
		s.MaxIntervalMinutes = 7 * 24 * 60
	}
	if s.MaxCandidatesPerRound < 1 {
		s.MaxCandidatesPerRound = 50
	}
	if s.MaxCandidatesPerRound > 500 {
		s.MaxCandidatesPerRound = 500
	}
	if len(s.ProxyIDs) > 1000 {
		s.ProxyIDs = s.ProxyIDs[:1000]
	}
}

// GetWindowHunterSettings 读取猎手配置。
func (s *SettingService) GetWindowHunterSettings(ctx context.Context) (*WindowHunterSettings, error) {
	return getWindowHunterSettingsValue(ctx, s)
}

// SetWindowHunterSettings 写回猎手配置。
func (s *SettingService) SetWindowHunterSettings(ctx context.Context, settings *WindowHunterSettings) error {
	return setWindowHunterSettingsValue(ctx, s, settings)
}

const SettingKeyWindowHunterSettings = "window_hunter_settings"

func getWindowHunterSettingsValue(ctx context.Context, s *SettingService) (*WindowHunterSettings, error) {
	if s == nil || s.settingRepo == nil {
		return DefaultWindowHunterSettings(), nil
	}
	value, err := s.settingRepo.GetValue(ctx, SettingKeyWindowHunterSettings)
	if err != nil {
		if err == ErrSettingNotFound {
			return DefaultWindowHunterSettings(), nil
		}
		return nil, fmt.Errorf("get window hunter settings: %w", err)
	}
	if strings.TrimSpace(value) == "" {
		return DefaultWindowHunterSettings(), nil
	}
	var settings WindowHunterSettings
	if err := json.Unmarshal([]byte(value), &settings); err != nil {
		return DefaultWindowHunterSettings(), nil
	}
	settings.Normalize()
	return &settings, nil
}

func setWindowHunterSettingsValue(ctx context.Context, s *SettingService, settings *WindowHunterSettings) error {
	if s == nil || s.settingRepo == nil {
		return errors.New("setting repository unavailable")
	}
	if settings == nil {
		return errors.New("settings cannot be nil")
	}
	settings.Normalize()
	data, err := json.Marshal(settings)
	if err != nil {
		return fmt.Errorf("marshal window hunter settings: %w", err)
	}
	return s.settingRepo.Set(ctx, SettingKeyWindowHunterSettings, string(data))
}

// ============================================================================
// 候选去重与排序（纯函数，单测覆盖）
// ============================================================================

// subnetKeyOf 计算 /24 网段键：IPv4 取前三段，IPv6 取 /64 前缀；非 IP 字符串原样返回。
// 实测依据：同 /24 的 22 个出口 IP 落在同一个计算节点。
func subnetKeyOf(ipOrHost string) string {
	raw := strings.TrimSpace(ipOrHost)
	if raw == "" {
		return ""
	}
	ip := net.ParseIP(raw)
	if ip == nil {
		// host 可能带端口或就是域名：剥端口
		if host, _, err := net.SplitHostPort(raw); err == nil {
			ip = net.ParseIP(host)
			if ip == nil {
				return strings.ToLower(host)
			}
		} else {
			return strings.ToLower(raw)
		}
	}
	if v4 := ip.To4(); v4 != nil {
		return fmt.Sprintf("%d.%d.%d.0/24", v4[0], v4[1], v4[2])
	}
	// IPv6 /64
	const slash64 = 64
	mask := net.CIDRMask(slash64, 128)
	return ip.Mask(mask).String() + "/64"
}

// exitIPForCandidate 出口 IP 解析：优先质量检测快照的出口 IP，回退代理 host。
func exitIPForCandidate(proxy *Proxy, exitIPs map[int64]string) string {
	if proxy == nil {
		return ""
	}
	if exitIPs != nil {
		if ip := strings.TrimSpace(exitIPs[proxy.ID]); ip != "" {
			return ip
		}
	}
	return proxy.Host
}

// DedupeHunterCandidatesBySubnet 按 /24 网段去重候选出口，保留每组第一个。
func DedupeHunterCandidatesBySubnet(proxies []*Proxy, exitIPs map[int64]string) []*Proxy {
	if len(proxies) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(proxies))
	out := make([]*Proxy, 0, len(proxies))
	for _, proxy := range proxies {
		if proxy == nil {
			continue
		}
		key := subnetKeyOf(exitIPForCandidate(proxy, exitIPs))
		if key == "" {
			key = fmt.Sprintf("id:%d", proxy.ID)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, proxy)
	}
	return out
}

// OrderHunterCandidatesByRegionDiversity 按国家/区域多样性交错排序：
// 同区域候选分散开，避免一轮扫完全是同区出口（地域亲和会让同区重复落同一节点簇）。
// regionOf 缺失的候选排在最后。
func OrderHunterCandidatesByRegionDiversity(candidates []*Proxy, regionOf func(*Proxy) string) []*Proxy {
	if len(candidates) <= 2 {
		return candidates
	}
	buckets := make(map[string][]*Proxy)
	var order []string // 区域首见顺序
	for _, proxy := range candidates {
		region := ""
		if regionOf != nil {
			region = strings.ToUpper(strings.TrimSpace(regionOf(proxy)))
		}
		if region == "" {
			region = "\x00unknown" // 排空桶后追加
		}
		if _, ok := buckets[region]; !ok {
			order = append(order, region)
		}
		buckets[region] = append(buckets[region], proxy)
	}
	out := make([]*Proxy, 0, len(candidates))
	// 交错轮询各区域桶
	for len(out) < len(candidates) {
		progressed := false
		for _, region := range order {
			bucket := buckets[region]
			if len(bucket) == 0 {
				continue
			}
			out = append(out, bucket[0])
			buckets[region] = bucket[1:]
			progressed = true
		}
		if !progressed {
			break
		}
	}
	return out
}

// HunterBackoffDelay 计算轮次退避间隔：base × 2^(roundsSinceMiss-1)，封顶 max。
// roundsSinceMiss=0（尚未错过或刚命中）返回 base。
func HunterBackoffDelay(roundsSinceMiss int, base, max time.Duration) time.Duration {
	if base <= 0 {
		base = 30 * time.Minute
	}
	if max < base {
		max = base
	}
	if roundsSinceMiss <= 0 {
		return base
	}
	delay := base
	for i := 1; i < roundsSinceMiss; i++ {
		delay *= 2
		if delay >= max {
			return max
		}
	}
	return delay
}

// ============================================================================
// 运行记录
// ============================================================================

// WindowHunterCandidateResult 单个候选出口的探测结论。
type WindowHunterCandidateResult struct {
	ProxyID   int64             `json:"proxy_id"`
	ProxyName string            `json:"proxy_name"`
	Region    string            `json:"region,omitempty"`
	Result    WindowProbeResult `json:"result"`
	Answer    string            `json:"answer,omitempty"`
	LatencyMs int64             `json:"latency_ms"`
	Message   string            `json:"message,omitempty"`
}

// WindowHunterRunEntry 一轮狩猎的完整记录。
type WindowHunterRunEntry struct {
	ID         string                        `json:"id"`
	AccountID  int64                         `json:"account_id"`
	Trigger    string                        `json:"trigger"` // manual | auto
	StartedAt  time.Time                     `json:"started_at"`
	FinishedAt time.Time                     `json:"finished_at"`
	Running    bool                          `json:"running"`
	Hit        bool                          `json:"hit"`
	HitProxyID int64                         `json:"hit_proxy_id,omitempty"`
	Scanned    int                           `json:"scanned"`
	Total      int                           `json:"total"`
	Round      int                           `json:"round"`
	NextRunAt  *time.Time                    `json:"next_run_at,omitempty"`
	Candidates []WindowHunterCandidateResult `json:"candidates,omitempty"`
	Error      string                        `json:"error,omitempty"`
}

// windowHunterRunLogCapacity 运行日志环形容量（内存态，重启清空；
// 探测事实本身已持久化在 account_node_health）。
const windowHunterRunLogCapacity = 30

// ErrWindowHunterRunning 已有一轮在跑（并发 1 纪律）。
var ErrWindowHunterRunning = errors.New("window hunter: another round is already running")

// ErrWindowHunterNoTarget 未配置目标账号。
var ErrWindowHunterNoTarget = errors.New("window hunter: target account is not configured")

// ============================================================================
// 猎手服务
// ============================================================================

// WindowProbeRunner 猎手对探针服务的窄依赖（便于单测替身）。
type WindowProbeRunner interface {
	ProbeAccount(ctx context.Context, accountID int64, proxyID *int64, force bool) (*WindowProbeOutcome, error)
}

// WindowHunterService 窗口猎手编排器。
type WindowHunterService struct {
	probe             WindowProbeRunner
	proxyRepo         ProxyRepository
	proxyLatencyCache ProxyLatencyCache
	settingService    *SettingService
	healthRepo        WindowProbeRepository

	mu             sync.Mutex
	running        bool
	runs           []*WindowHunterRunEntry
	roundsSinceHit int
	nextRunAt      time.Time
	autoStopCh     chan struct{}
	autoStarted    bool
}

// NewWindowHunterService 构造窗口猎手。
func NewWindowHunterService(
	probe WindowProbeRunner,
	proxyRepo ProxyRepository,
	proxyLatencyCache ProxyLatencyCache,
	settingService *SettingService,
	healthRepo WindowProbeRepository,
) *WindowHunterService {
	return &WindowHunterService{
		probe:             probe,
		proxyRepo:         proxyRepo,
		proxyLatencyCache: proxyLatencyCache,
		settingService:    settingService,
		healthRepo:        healthRepo,
		autoStopCh:        make(chan struct{}),
	}
}

// GetSettings 读取猎手配置。
func (h *WindowHunterService) GetSettings(ctx context.Context) (*WindowHunterSettings, error) {
	if h == nil || h.settingService == nil {
		return DefaultWindowHunterSettings(), nil
	}
	return h.settingService.GetWindowHunterSettings(ctx)
}

// SetSettings 写回猎手配置。
func (h *WindowHunterService) SetSettings(ctx context.Context, settings *WindowHunterSettings) error {
	if h == nil || h.settingService == nil {
		return errors.New("setting service unavailable")
	}
	return h.settingService.SetWindowHunterSettings(ctx, settings)
}

// resolveCandidates 解析本轮候选出口：显式列表 > 全部 active；按 /24 去重 + 区域多样性排序 + 上限截断。
func (h *WindowHunterService) resolveCandidates(ctx context.Context, settings *WindowHunterSettings) ([]*Proxy, error) {
	var proxies []Proxy
	if len(settings.ProxyIDs) > 0 {
		list, listErr := h.proxyRepo.ListByIDs(ctx, settings.ProxyIDs)
		if listErr != nil {
			return nil, fmt.Errorf("list candidate proxies: %w", listErr)
		}
		proxies = list
	} else {
		list, listErr := h.proxyRepo.ListActive(ctx)
		if listErr != nil {
			return nil, fmt.Errorf("list active proxies: %w", listErr)
		}
		proxies = list
	}
	proxyPtrs := filterActiveProxies(proxies)

	exitIPs := map[int64]string{}
	if h.proxyLatencyCache != nil && len(proxyPtrs) > 0 {
		ids := make([]int64, 0, len(proxyPtrs))
		for _, proxy := range proxyPtrs {
			ids = append(ids, proxy.ID)
		}
		if infos, infosErr := h.proxyLatencyCache.GetProxyLatencies(ctx, ids); infosErr == nil && infos != nil {
			for id, info := range infos {
				if info != nil && info.IPAddress != "" {
					exitIPs[id] = info.IPAddress
				}
			}
		}
	}

	deduped := DedupeHunterCandidatesBySubnet(proxyPtrs, exitIPs)
	deduped = OrderHunterCandidatesByRegionDiversity(deduped, func(proxy *Proxy) string {
		return h.regionOf(ctx, proxy)
	})
	if len(deduped) > settings.MaxCandidatesPerRound {
		deduped = deduped[:settings.MaxCandidatesPerRound]
	}
	return deduped, nil
}

func filterActiveProxies(proxies []Proxy) []*Proxy {
	out := make([]*Proxy, 0, len(proxies))
	now := time.Now()
	for i := range proxies {
		if proxies[i].IsActive() && !proxies[i].IsExpired(now) {
			out = append(out, &proxies[i])
		}
	}
	return out
}

func (h *WindowHunterService) regionOf(ctx context.Context, proxy *Proxy) string {
	if proxy == nil || h.proxyLatencyCache == nil {
		return ""
	}
	infos, err := h.proxyLatencyCache.GetProxyLatencies(ctx, []int64{proxy.ID})
	if err != nil || infos == nil {
		return ""
	}
	if info := infos[proxy.ID]; info != nil {
		if info.CountryCode != "" {
			return info.CountryCode
		}
		return info.Country
	}
	return ""
}

// RunRoundAsync 异步开一轮狩猎（面板"立即狩猎"按钮）。已有一轮在跑时返回 ErrWindowHunterRunning。
func (h *WindowHunterService) RunRoundAsync(ctx context.Context, accountID int64, trigger string) (string, error) {
	if h == nil {
		return "", errors.New("window hunter not initialized")
	}
	h.mu.Lock()
	if h.running {
		h.mu.Unlock()
		return "", ErrWindowHunterRunning
	}
	h.running = true
	if accountID <= 0 {
		accountID = 0 // 由 runRound 内按 settings 解析
	}
	entry := &WindowHunterRunEntry{
		ID:        uuid.NewString(),
		AccountID: accountID,
		Trigger:   trigger,
		StartedAt: time.Now(),
		Running:   true,
		Round:     h.roundsSinceHit + 1,
	}
	h.runs = appendWindowHunterRun(h.runs, entry)
	h.mu.Unlock()

	go func() {
		h.runRound(context.WithoutCancel(ctx), entry)
	}()
	return entry.ID, nil
}

// runRound 执行一轮：并发 1、逐个探测、命中即停。
func (h *WindowHunterService) runRound(ctx context.Context, entry *WindowHunterRunEntry) {
	defer func() {
		h.mu.Lock()
		h.running = false
		h.mu.Unlock()
	}()

	settings, err := h.GetSettings(ctx)
	if err != nil {
		h.finishRun(entry, time.Now(), false, 0, err)
		return
	}
	accountID := entry.AccountID
	if accountID <= 0 {
		accountID = settings.TargetAccountID
	}
	if accountID <= 0 {
		h.finishRun(entry, time.Now(), false, 0, ErrWindowHunterNoTarget)
		return
	}
	entry.AccountID = accountID

	candidates, err := h.resolveCandidates(ctx, settings)
	if err != nil {
		h.finishRun(entry, time.Now(), false, 0, err)
		return
	}
	entry.Total = len(candidates)

	for _, candidate := range candidates {
		select {
		case <-ctx.Done():
			h.finishRun(entry, time.Now(), false, entry.Scanned, ctx.Err())
			return
		default:
		}
		outcome, probeErr := h.probe.ProbeAccount(ctx, accountID, &candidate.ID, true)
		candidateResult := WindowHunterCandidateResult{
			ProxyID:   candidate.ID,
			ProxyName: candidate.Name,
			Region:    h.regionOf(ctx, candidate),
		}
		if probeErr != nil {
			candidateResult.Result = WindowProbeError
			candidateResult.Message = probeErr.Error()
		} else if outcome != nil {
			candidateResult.Result = outcome.Result
			candidateResult.Answer = outcome.Answer
			candidateResult.LatencyMs = outcome.LatencyMs
			candidateResult.Message = outcome.Message
		}
		entry.Candidates = append(entry.Candidates, candidateResult)
		entry.Scanned++

		if candidateResult.Result == WindowProbeFullPower {
			now := time.Now()
			entry.HitProxyID = candidate.ID
			h.mu.Lock()
			h.roundsSinceHit = 0
			h.nextRunAt = time.Time{} // 命中即停：不再自动排程
			h.mu.Unlock()
			h.finishRun(entry, now, true, entry.Scanned, nil)
			return
		}
	}

	// 全员未命中：进入退避
	now := time.Now()
	h.mu.Lock()
	h.roundsSinceHit++
	delay := HunterBackoffDelay(
		h.roundsSinceHit,
		time.Duration(settings.BaseIntervalMinutes)*time.Minute,
		time.Duration(settings.MaxIntervalMinutes)*time.Minute,
	)
	next := now.Add(delay)
	h.nextRunAt = next
	h.mu.Unlock()
	entry.NextRunAt = &next
	h.finishRun(entry, now, false, entry.Scanned, nil)
}

func (h *WindowHunterService) finishRun(entry *WindowHunterRunEntry, finishedAt time.Time, hit bool, scanned int, runErr error) {
	entry.FinishedAt = finishedAt
	entry.Running = false
	entry.Hit = hit
	entry.Scanned = scanned
	if runErr != nil {
		entry.Error = runErr.Error()
	}
}

// Status 返回猎手当前状态（面板轮询）。
func (h *WindowHunterService) Status(ctx context.Context) (*WindowHunterStatus, error) {
	if h == nil {
		return nil, errors.New("window hunter not initialized")
	}
	settings, err := h.GetSettings(ctx)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	status := &WindowHunterStatus{
		Running:        h.running,
		RoundsSinceHit: h.roundsSinceHit,
		NextRunAt:      h.nextRunAt,
		Settings:       settings,
		Runs:           make([]*WindowHunterRunEntry, 0, len(h.runs)),
	}
	// 面板日志裁剪单轮候选明细，全量探测事实在健康表
	for _, run := range h.runs {
		trimmed := *run
		if len(run.Candidates) > 10 {
			trimmed.Candidates = run.Candidates[:10]
		}
		status.Runs = append(status.Runs, &trimmed)
	}
	return status, nil
}

// WindowHunterStatus 猎手状态快照。
type WindowHunterStatus struct {
	Running        bool                    `json:"running"`
	RoundsSinceHit int                     `json:"rounds_since_hit"`
	NextRunAt      time.Time               `json:"next_run_at,omitempty"`
	Settings       *WindowHunterSettings   `json:"settings"`
	Runs           []*WindowHunterRunEntry `json:"runs"`
}

// StartAutoLoop 启动自动狩猎循环（应用启动时调用一次；AutoEnabled=false 时空转）。
func (h *WindowHunterService) StartAutoLoop() {
	if h == nil {
		return
	}
	h.mu.Lock()
	if h.autoStarted {
		h.mu.Unlock()
		return
	}
	h.autoStarted = true
	stopCh := h.autoStopCh
	h.mu.Unlock()

	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				h.autoTick()
			}
		}
	}()
}

// autoTick 自动模式调度：到点开轮；命中后若窗口过期（健康态不再满血）则重新武装。
func (h *WindowHunterService) autoTick() {
	ctx := context.Background()
	settings, err := h.GetSettings(ctx)
	if err != nil || settings == nil || !settings.AutoEnabled {
		return
	}
	h.mu.Lock()
	running := h.running
	nextRunAt := h.nextRunAt
	h.mu.Unlock()
	if running {
		return
	}
	// 未武装（上一轮命中后停止）时：目标已不再满血 → 重新进入狩猎节奏
	if nextRunAt.IsZero() {
		if h.targetStillFullPower(ctx, settings.TargetAccountID) {
			return
		}
		h.mu.Lock()
		h.roundsSinceHit = 0
		h.nextRunAt = time.Now().Add(HunterBackoffDelay(0,
			time.Duration(settings.BaseIntervalMinutes)*time.Minute,
			time.Duration(settings.MaxIntervalMinutes)*time.Minute))
		h.mu.Unlock()
		return
	}
	if time.Now().Before(nextRunAt) {
		return
	}
	_, _ = h.RunRoundAsync(ctx, settings.TargetAccountID, "auto")
}

func (h *WindowHunterService) targetStillFullPower(ctx context.Context, accountID int64) bool {
	if accountID <= 0 || h.healthRepo == nil {
		return false
	}
	rows, err := h.healthRepo.ListByAccountIDs(ctx, []int64{accountID})
	if err != nil {
		return false
	}
	window := 240 * time.Second
	if h.settingService != nil {
		if probeSettings, err := h.settingService.GetWindowProbeSettings(ctx); err == nil && probeSettings != nil {
			window = probeSettings.WindowDuration()
		}
	}
	for _, row := range rows {
		if row.EffectiveState(time.Now(), window) == NodeHealthStateFullPower {
			return true
		}
	}
	return false
}

func appendWindowHunterRun(runs []*WindowHunterRunEntry, entry *WindowHunterRunEntry) []*WindowHunterRunEntry {
	runs = append(runs, entry)
	if len(runs) > windowHunterRunLogCapacity {
		runs = runs[len(runs)-windowHunterRunLogCapacity:]
	}
	return runs
}
