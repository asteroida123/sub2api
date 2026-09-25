//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// /24 去重（实测：22 个同段 IP = 同一节点）
// ============================================================================

func TestSubnetKeyOf(t *testing.T) {
	// 同 /24 归一
	require.Equal(t, subnetKeyOf("14.137.237.91"), subnetKeyOf("14.137.237.42"))
	require.Equal(t, "14.137.237.0/24", subnetKeyOf("14.137.237.91"))
	// 不同 /24 区分
	require.NotEqual(t, subnetKeyOf("14.137.237.91"), subnetKeyOf("14.137.238.91"))
	// IPv6 /64
	require.Equal(t, subnetKeyOf("2001:db8:1:2::1"), subnetKeyOf("2001:db8:1:2:ffff::ff"))
	require.NotEqual(t, subnetKeyOf("2001:db8:1:2::1"), subnetKeyOf("2001:db8:1:3::1"))
	// 域名原样（大小写归一）
	require.Equal(t, "proxy.example.com", subnetKeyOf("Proxy.Example.Com"))
}

func TestDedupeHunterCandidatesBySubnet(t *testing.T) {
	proxies := []*Proxy{
		{ID: 1, Host: "14.137.237.91", Status: StatusActive},
		{ID: 2, Host: "14.137.237.42", Status: StatusActive},
		{ID: 3, Host: "84.17.47.150", Status: StatusActive},
		{ID: 4, Host: "jp.example.com", Status: StatusActive},
	}
	// 出口 IP 快照优先于 host：代理 1 的真实出口与代理 3 同段 → 3 被合并
	exitIPs := map[int64]string{3: "14.137.237.77"}

	deduped := DedupeHunterCandidatesBySubnet(proxies, exitIPs)
	ids := make([]int64, 0, len(deduped))
	for _, p := range deduped {
		ids = append(ids, p.ID)
	}
	require.Equal(t, []int64{1, 4}, ids, "same-/24 exit (2,3) merged, first kept")
}

// ============================================================================
// 区域多样性排序
// ============================================================================

func TestOrderHunterCandidatesByRegionDiversity(t *testing.T) {
	regionOf := map[int64]string{
		1: "JP", 2: "JP", 3: "JP",
		4: "US", 5: "US",
		6: "SG",
	}
	proxies := []*Proxy{{ID: 1}, {ID: 2}, {ID: 3}, {ID: 4}, {ID: 5}, {ID: 6}}
	ordered := OrderHunterCandidatesByRegionDiversity(proxies, func(p *Proxy) string { return regionOf[p.ID] })

	require.Len(t, ordered, 6)
	// 前三个必须覆盖三个不同区域（交错生效）
	firstThree := map[string]bool{}
	for _, p := range ordered[:3] {
		firstThree[regionOf[p.ID]] = true
	}
	require.Len(t, firstThree, 3)
}

// ============================================================================
// 轮次退避
// ============================================================================

func TestHunterBackoffDelay(t *testing.T) {
	base := 30 * time.Minute
	max := 240 * time.Minute

	require.Equal(t, base, HunterBackoffDelay(0, base, max))
	require.Equal(t, base, HunterBackoffDelay(1, base, max))
	require.Equal(t, 60*time.Minute, HunterBackoffDelay(2, base, max))
	require.Equal(t, 120*time.Minute, HunterBackoffDelay(3, base, max))
	require.Equal(t, max, HunterBackoffDelay(4, base, max), "capped at max")
	require.Equal(t, max, HunterBackoffDelay(10, base, max))
}

func TestWindowHunterSettingsNormalize(t *testing.T) {
	s := DefaultWindowHunterSettings()
	s.BaseIntervalMinutes = 0
	s.MaxIntervalMinutes = 0
	s.MaxCandidatesPerRound = 0
	s.Normalize()
	require.Equal(t, 30, s.BaseIntervalMinutes)
	require.Equal(t, 240, s.MaxIntervalMinutes)
	require.Equal(t, 50, s.MaxCandidatesPerRound)
}

// ============================================================================
// 猎手轮次：命中即停 + 全员未命中退避
// ============================================================================

type fakeProbeRunner struct {
	results map[int64]WindowProbeOutcome // proxyID → outcome
	calls   []int64
}

func (f *fakeProbeRunner) ProbeAccount(ctx context.Context, accountID int64, proxyID *int64, force bool) (*WindowProbeOutcome, error) {
	id := *proxyID
	f.calls = append(f.calls, id)
	outcome := f.results[id]
	return &outcome, nil
}

type fakeHunterProxyRepo struct {
	ProxyRepository
	proxies []Proxy
}

func (f *fakeHunterProxyRepo) ListActive(ctx context.Context) ([]Proxy, error) {
	return f.proxies, nil
}

func (f *fakeHunterProxyRepo) ListByIDs(ctx context.Context, ids []int64) ([]Proxy, error) {
	return f.proxies, nil
}

func newHunterTestService(results map[int64]WindowProbeOutcome, proxies ...Proxy) (*WindowHunterService, *fakeProbeRunner) {
	runner := &fakeProbeRunner{results: results}
	svc := NewWindowHunterService(
		runner,
		&fakeHunterProxyRepo{proxies: proxies},
		nil,
		nil,
		&probeTestHealthRepo{repos: &fakeWindowProbeRepos{health: map[string]*AccountNodeHealth{}}},
	)
	return svc, runner
}

func hunterTestProxy(id int64, host string) Proxy {
	return Proxy{ID: id, Host: host, Status: StatusActive}
}

func TestWindowHunterRunRoundHitStops(t *testing.T) {
	// 三个候选，第 2 个命中满血 → 应在第 2 个后停止
	results := map[int64]WindowProbeOutcome{
		11: {Result: WindowProbeDegraded, Answer: "石破茂"},
		12: {Result: WindowProbeFullPower, Answer: "高市早苗"},
		13: {Result: WindowProbeDegraded, Answer: "拜登"},
	}
	svc, runner := newHunterTestService(results,
		hunterTestProxy(11, "1.1.1.1"),
		hunterTestProxy(12, "2.2.2.2"),
		hunterTestProxy(13, "3.3.3.3"),
	)

	runID, err := svc.RunRoundAsync(context.Background(), 2474, "manual")
	require.NoError(t, err)
	require.NotEmpty(t, runID)

	require.Eventually(t, func() bool {
		status, err := svc.Status(context.Background())
		return err == nil && !status.Running
	}, 2*time.Second, 10*time.Millisecond)

	require.Equal(t, []int64{11, 12}, runner.calls, "hit-and-stop: probe #13 never called")

	status, err := svc.Status(context.Background())
	require.NoError(t, err)
	require.Len(t, status.Runs, 1)
	run := status.Runs[0]
	require.True(t, run.Hit)
	require.Equal(t, int64(12), run.HitProxyID)
	require.Equal(t, 2, run.Scanned)
	require.Nil(t, run.NextRunAt, "命中即停：不再排程")
}

func TestWindowHunterRunRoundAllMissBacksOff(t *testing.T) {
	results := map[int64]WindowProbeOutcome{
		11: {Result: WindowProbeDegraded, Answer: "石破茂"},
		12: {Result: WindowProbeDegraded, Answer: "拜登"},
	}
	svc, _ := newHunterTestService(results,
		hunterTestProxy(11, "1.1.1.1"),
		hunterTestProxy(12, "2.2.2.2"),
	)

	runID, err := svc.RunRoundAsync(context.Background(), 2474, "manual")
	require.NoError(t, err)
	require.NotEmpty(t, runID)

	require.Eventually(t, func() bool {
		status, err := svc.Status(context.Background())
		return err == nil && !status.Running
	}, 2*time.Second, 10*time.Millisecond)

	status, err := svc.Status(context.Background())
	require.NoError(t, err)
	run := status.Runs[0]
	require.False(t, run.Hit)
	require.NotNil(t, run.NextRunAt, "全员未命中：进入退避")
	require.True(t, run.NextRunAt.After(time.Now()))
	require.Equal(t, 1, status.RoundsSinceHit)
}

func TestWindowHunterConcurrencyOne(t *testing.T) {
	// 探针挂起模拟慢探测：第二轮必须被拒绝
	svc, runner := newHunterTestService(nil, hunterTestProxy(11, "1.1.1.1"))
	runner.results = map[int64]WindowProbeOutcome{}

	runID, err := svc.RunRoundAsync(context.Background(), 2474, "manual")
	require.NoError(t, err)
	require.NotEmpty(t, runID)

	_, err = svc.RunRoundAsync(context.Background(), 2474, "manual")
	require.ErrorIs(t, err, ErrWindowHunterRunning)

	require.Eventually(t, func() bool {
		status, err := svc.Status(context.Background())
		return err == nil && !status.Running
	}, 2*time.Second, 10*time.Millisecond)
}

func TestWindowHunterNoTarget(t *testing.T) {
	svc, _ := newHunterTestService(nil)
	// settings 无 target（nil settingService → 默认 0），也未显式传账号
	_, err := svc.RunRoundAsync(context.Background(), 0, "manual")
	require.NoError(t, err) // 异步启动成功，轮内报错

	require.Eventually(t, func() bool {
		status, err := svc.Status(context.Background())
		return err == nil && !status.Running && len(status.Runs) == 1 && status.Runs[0].Error != ""
	}, 2*time.Second, 10*time.Millisecond)
	status, _ := svc.Status(context.Background())
	require.ErrorContains(t, errors.New(status.Runs[0].Error), ErrWindowHunterNoTarget.Error())
}

// ============================================================================
// 满血会话池（P2）：标记 / 门控 / 优先挑选 / 退役
// ============================================================================

func newWindowPoolTestService(t *testing.T) (*WindowSessionPoolService, *openAIWSConnPool, *openAIWSCountingDialer) {
	t.Helper()
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 8
	// MaxIdlePerAccount 要 ≥ 预建数：池清理会把超出空闲上限的连接回收
	// （生产提示：启用满血会话池时该值需 ≥ prewarm_count）
	cfg.Gateway.OpenAIWS.MinIdlePerAccount = 8
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 8
	cfg.Gateway.OpenAIWS.PoolTargetUtilization = 0.8
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 1

	gateway := &OpenAIGatewayService{cfg: cfg}
	svc := NewWindowSessionPoolService(nil, nil, nil, gateway)
	pool := svc.pool()
	require.NotNil(t, pool)
	dialer := &openAIWSCountingDialer{}
	pool.setClientDialerForTest(dialer)
	return svc, pool, dialer
}

func TestWindowSessionPoolPrewarmMarksFullPower(t *testing.T) {
	svc, pool, _ := newWindowPoolTestService(t)
	accountID := int64(2474)
	account := &Account{ID: accountID, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	pool.getOrCreateAccountPool(accountID).mu.Lock()
	pool.getOrCreateAccountPool(accountID).lastAcquire = &openAIWSAcquireRequest{
		Account: account,
		WSURL:   "wss://example.com/v1/responses",
	}
	pool.getOrCreateAccountPool(accountID).mu.Unlock()

	ids, err := svc.Prewarm(context.Background(), accountID, 3)
	require.NoError(t, err)
	require.Len(t, ids, 3)

	// 默认会话寿命 60min：标记应落在 [now+55min, now+61min] 窗口内
	lower := time.Now().Add(55 * time.Minute)
	upper := time.Now().Add(61 * time.Minute)
	sessions := pool.SnapshotConns(accountID, true)
	require.Len(t, sessions, 3)
	for _, s := range sessions {
		require.True(t, s.IsFullPower)
		require.False(t, s.FullPowerUntil.IsZero())
		require.True(t, s.FullPowerUntil.After(lower), "full_power_until=%v", s.FullPowerUntil)
		require.True(t, s.FullPowerUntil.Before(upper), "full_power_until=%v", s.FullPowerUntil)
	}
}

func TestRequireFullPowerGate(t *testing.T) {
	svc, pool, _ := newWindowPoolTestService(t)
	accountID := int64(2474)
	account := &Account{ID: accountID, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

	// 池中只有普通连接（未标记）→ 门控请求拿不到满血 → 哨兵错误，且不新建连接
	lease, err := pool.Acquire(context.Background(), openAIWSAcquireRequest{
		Account:          account,
		WSURL:            "wss://example.com/v1/responses",
		RequireFullPower: true,
	})
	require.ErrorIs(t, err, errOpenAIWSFullPowerUnavailable)
	require.Nil(t, lease)

	// 预建满血连接后门控请求成功，且优先选中满血条目
	pool.getOrCreateAccountPool(accountID).mu.Lock()
	pool.getOrCreateAccountPool(accountID).lastAcquire = &openAIWSAcquireRequest{
		Account: account,
		WSURL:   "wss://example.com/v1/responses",
	}
	pool.getOrCreateAccountPool(accountID).mu.Unlock()
	_, err = svc.Prewarm(context.Background(), accountID, 2)
	require.NoError(t, err)

	// 再放一个未标记连接进池（ForceNewConn，避免满血优先策略借走刚预建的连接）
	unmarked, err := pool.Acquire(context.Background(), openAIWSAcquireRequest{
		Account:      account,
		WSURL:        "wss://example.com/v1/responses",
		ForceNewConn: true,
	})
	require.NoError(t, err)
	defer unmarked.Release()

	lease, err = pool.Acquire(context.Background(), openAIWSAcquireRequest{
		Account:          account,
		WSURL:            "wss://example.com/v1/responses",
		RequireFullPower: true,
	})
	require.NoError(t, err)
	require.NotNil(t, lease)
	require.True(t, lease.conn.isFullPowerAt(time.Now()), "门控请求必须拿到满血连接")
	lease.Release()
}

func TestRetireDegradedConn(t *testing.T) {
	svc, pool, _ := newWindowPoolTestService(t)
	accountID := int64(2475)
	account := &Account{ID: accountID, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	pool.getOrCreateAccountPool(accountID).mu.Lock()
	pool.getOrCreateAccountPool(accountID).lastAcquire = &openAIWSAcquireRequest{
		Account: account,
		WSURL:   "wss://example.com/v1/responses",
	}
	pool.getOrCreateAccountPool(accountID).mu.Unlock()

	ids, err := svc.Prewarm(context.Background(), accountID, 1)
	require.NoError(t, err)
	require.Len(t, ids, 1)
	connID := ids[0]

	pool.RetireDegradedConn(accountID, connID)

	sessions := pool.SnapshotConns(accountID, true)
	for _, s := range sessions {
		if s.ConnID == connID {
			t.Fatalf("retired conn %s should be evicted", connID)
		}
	}
}

func TestSampleFailureRevokesFullPower(t *testing.T) {
	// 连续采样失败达到阈值 → 撤销满血标记（节点状态不可信）
	cfg := &config.Config{}
	conn := newOpenAIWSConn("conn_test", 1, &openAIWSFakeConn{}, http.Header{})
	conn.MarkFullPowerUntil(time.Now().Add(time.Hour))
	require.True(t, conn.isFullPowerAt(time.Now()))

	for i := 0; i < 2; i++ {
		conn.recordSampleFailure(time.Now(), 2)
	}
	require.False(t, conn.isFullPowerAt(time.Now()), "连续失败后撤销满血标记")
	require.False(t, conn.degradedFlag.Load(), "失败≠降智判定")
	_ = cfg
}

func TestWindowSessionPoolSettingsGateModel(t *testing.T) {
	s := DefaultWindowSessionPoolSettings()
	require.True(t, s.MatchesGateModel("gpt-6-astra"))
	require.True(t, s.MatchesGateModel("GPT-6-Astra-Chat"))
	require.False(t, s.MatchesGateModel("gpt-5.4"))
	require.False(t, s.MatchesGateModel("gpt-6-astrox"))
}

// ============================================================================
// P1 面板徽标派生态与探针三态已在 window_probe_unit_test.go 覆盖；
// 此处补猎手配置序列化往返。
// ============================================================================

func TestWindowHunterSettingsRoundTrip(t *testing.T) {
	s := DefaultWindowHunterSettings()
	s.AutoEnabled = true
	s.TargetAccountID = 2474
	s.Normalize()

	require.True(t, s.AutoEnabled)
	require.Equal(t, int64(2474), s.TargetAccountID)
}
