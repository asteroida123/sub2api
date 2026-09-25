//go:build unit

package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// 归一化与分类器
// ============================================================================

func TestNormalizeProbeAnswer(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"高市早苗", "高市早苗"},
		{" 高市 早苗 ", "高市早苗"},
		{"石破茂。", "石破茂"},
		{"Lee Jae-myung.", "leejaemyung"},
		{"Takaichi Sanae！", "takaichisanae"},
		{"回答：特朗普（Trump）", "回答特朗普trump"},
		{"", ""},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, normalizeProbeAnswer(tc.in), "input: %q", tc.in)
	}
}

func TestClassifyProbeAnswer(t *testing.T) {
	q := DefaultWindowProbeQuestions()[0] // jp_pm

	// 满血：现任（新知识）
	require.Equal(t, WindowProbeFullPower, classifyProbeAnswer("高市早苗", q))
	require.Equal(t, WindowProbeFullPower, classifyProbeAnswer("现在是高市早苗。", q))
	require.Equal(t, WindowProbeFullPower, classifyProbeAnswer("Takaichi Sanae", q))

	// 降智：旧快照
	require.Equal(t, WindowProbeDegraded, classifyProbeAnswer("石破茂", q))
	require.Equal(t, WindowProbeDegraded, classifyProbeAnswer("石破 茂", q))
	require.Equal(t, WindowProbeDegraded, classifyProbeAnswer("Ishiba Shigeru", q))

	// 无法判定：空、无关答案、新旧同时命中
	require.Equal(t, WindowProbeResult(""), classifyProbeAnswer("", q))
	require.Equal(t, WindowProbeResult(""), classifyProbeAnswer("不知道", q))
	require.Equal(t, WindowProbeResult(""), classifyProbeAnswer("高市早苗石破茂", q))
}

// ============================================================================
// 状态机
// ============================================================================

func newTestHealth() *AccountNodeHealth {
	return &AccountNodeHealth{
		AccountID: 1,
		ProxyID:   2,
		State:     NodeHealthStateUnknown,
	}
}

func TestApplyProbeOutcomeFullPower(t *testing.T) {
	h := newTestHealth()
	now := time.Now()
	applyProbeOutcome(h, now, WindowProbeFullPower, "高市早苗", 4*time.Hour)

	require.Equal(t, NodeHealthStateFullPower, h.State)
	require.NotNil(t, h.WindowOpenedAt)
	require.Equal(t, now, *h.WindowOpenedAt)
	require.Nil(t, h.DegradedAt)
	require.Nil(t, h.CooldownUntil)
	require.Equal(t, int64(1), h.ProbeCount)
	require.Equal(t, "高市早苗", h.LastProbeAnswer)
	require.Equal(t, now, *h.LastProbeAt)
}

func TestApplyProbeOutcomeDegradedStartsCooldown(t *testing.T) {
	h := newTestHealth()
	now := time.Now()
	applyProbeOutcome(h, now, WindowProbeDegraded, "石破茂", 4*time.Hour)

	require.Equal(t, NodeHealthStateDegraded, h.State)
	require.NotNil(t, h.DegradedAt)
	require.NotNil(t, h.CooldownUntil)
	require.Equal(t, now.Add(4*time.Hour), *h.CooldownUntil)
	require.Equal(t, int64(1), h.ProbeCount)
}

func TestApplyProbeOutcomeErrorRecordsPollutionOnly(t *testing.T) {
	h := newTestHealth()
	h.ProbeCount = 3
	h.State = NodeHealthStateFullPower
	opened := time.Now().Add(-time.Minute)
	h.WindowOpenedAt = &opened
	now := time.Now()
	applyProbeOutcome(h, now, WindowProbeError, "", time.Hour)

	// 探测即污染：last_probe_at 必须刷新
	require.Equal(t, now, *h.LastProbeAt)
	// 但不改变状态结论、不计轮换
	require.Equal(t, NodeHealthStateFullPower, h.State)
	require.Equal(t, int64(3), h.ProbeCount)
	require.Equal(t, &opened, h.WindowOpenedAt)
}

func TestEffectiveStateWindowExpiry(t *testing.T) {
	h := newTestHealth()
	opened := time.Now().Add(-5 * time.Minute)
	h.State = NodeHealthStateFullPower
	h.WindowOpenedAt = &opened

	// 窗口时长 240s：5 分钟前开启的窗口已过期 → 视为降智
	require.Equal(t, NodeHealthStateDegraded, h.EffectiveState(time.Now(), 240*time.Second))

	fresh := time.Now().Add(-time.Minute)
	h.WindowOpenedAt = &fresh
	require.Equal(t, NodeHealthStateFullPower, h.EffectiveState(time.Now(), 240*time.Second))
}

func TestEffectiveStateCooldownDerivation(t *testing.T) {
	h := newTestHealth()
	now := time.Now()
	h.State = NodeHealthStateDegraded
	h.CooldownUntil = &now

	// 冷却已到期 → degraded；未到期 → cooldown
	require.Equal(t, NodeHealthStateDegraded, h.EffectiveState(now.Add(time.Minute), time.Minute))

	until := now.Add(2 * time.Hour)
	h.CooldownUntil = &until
	require.Equal(t, NodeHealthStateCooldown, h.EffectiveState(now, time.Minute))
}

func TestEffectiveStateUnknownAndNil(t *testing.T) {
	var h *AccountNodeHealth
	require.Equal(t, NodeHealthStateUnknown, h.EffectiveState(time.Now(), time.Minute))
	require.Equal(t, NodeHealthStateUnknown, newTestHealth().EffectiveState(time.Now(), time.Minute))
}

// ============================================================================
// 配置
// ============================================================================

func TestWindowProbeSettingsNormalize(t *testing.T) {
	s := &WindowProbeSettings{
		Questions: []WindowProbeQuestion{{ID: "bad", Text: ""}},
	}
	s.Normalize()

	require.Equal(t, "gpt-6-astra", s.Model)
	// 唯一题目缺关键词 → 回退默认题库
	require.NotEmpty(t, s.Questions)
	require.Equal(t, 240, s.CooldownMinutes)
	require.Equal(t, 240, s.WindowDurationSeconds)
	require.Equal(t, 90, s.ProbeTimeoutSeconds)
	require.Equal(t, DefaultWindowProbeSettings().Cooldown(), s.Cooldown())
}

func TestWindowProbeSettingsPickQuestionRotates(t *testing.T) {
	s := DefaultWindowProbeSettings()
	seen := make(map[string]bool)
	for i := int64(0); i < int64(len(s.Questions)); i++ {
		q := s.PickQuestion(i)
		require.False(t, seen[q.ID], "question %s repeated within one cycle", q.ID)
		seen[q.ID] = true
	}
	require.Equal(t, s.PickQuestion(0).ID, s.PickQuestion(int64(len(s.Questions))).ID)
}

// ============================================================================
// SSE 解析
// ============================================================================

func TestParseCodexProbeAnswerExtractsFirstDone(t *testing.T) {
	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created"}`,
		``,
		`data: {"type":"response.output_text.done","text":"高市早苗"}`,
		``,
		`data: {"type":"response.output_text.done","text":"第二条不应被取"}`,
		``,
		`data: {"type":"response.completed"}`,
	}, "\n")

	answer, err := parseCodexProbeAnswer(strings.NewReader(stream))
	require.NoError(t, err)
	require.Equal(t, "高市早苗", answer)
}

func TestParseCodexProbeAnswerErrorEvent(t *testing.T) {
	stream := "data: {\"type\":\"error\",\"code\":\"server_is_overloaded\",\"message\":\"busy\"}\n"
	_, err := parseCodexProbeAnswer(strings.NewReader(stream))
	require.Error(t, err)
	require.Contains(t, err.Error(), "server_is_overloaded")
}

func TestParseCodexProbeAnswerFailedEvent(t *testing.T) {
	stream := `data: {"type":"response.failed","response":{"status":"failed","error":{"code":"insufficient_quota","message":"no quota"}}}` + "\n"
	_, err := parseCodexProbeAnswer(strings.NewReader(stream))
	require.Error(t, err)
	require.Contains(t, err.Error(), "insufficient_quota")
}

func TestParseCodexProbeAnswerNoAnswer(t *testing.T) {
	_, err := parseCodexProbeAnswer(strings.NewReader("data: {\"type\":\"response.created\"}\n\n"))
	require.Error(t, err)
}

// ============================================================================
// ProbeAccount 全链路（fake 上游 + fake 仓储）
// ============================================================================

type fakeHTTPUpstream struct {
	statusCode int
	body       string
	// answers 按题面文本作答（验证题库轮换）；未命中题面时回退 body。
	answers   map[string]string
	err       error
	lastProxy string
}

func (f *fakeHTTPUpstream) Do(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error) {
	return f.DoWithTLS(req, proxyURL, accountID, accountConcurrency, nil)
}

func (f *fakeHTTPUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.lastProxy = proxyURL
	body := f.body
	if f.answers != nil && req.Body != nil {
		raw, _ := io.ReadAll(req.Body)
		_ = req.Body.Close()
		var payload struct {
			Input []struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"input"`
		}
		if json.Unmarshal(raw, &payload) == nil && len(payload.Input) > 0 && len(payload.Input[0].Content) > 0 {
			if answer, ok := f.answers[payload.Input[0].Content[0].Text]; ok {
				body = "data: {\"type\":\"response.output_text.done\",\"text\":\"" + answer + "\"}\n\n"
			}
		}
	}
	resp := &http.Response{
		StatusCode: f.statusCode,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{},
	}
	return resp, nil
}

type fakeWindowProbeRepos struct {
	accounts map[int64]*Account
	proxies  map[int64]*Proxy
	health   map[string]*AccountNodeHealth
}

func (f *fakeWindowProbeRepos) GetByIDAccount(ctx context.Context, id int64) (*Account, error) {
	if acc, ok := f.accounts[id]; ok {
		return acc, nil
	}
	return nil, ErrAccountNotFound
}

func (f *fakeWindowProbeRepos) GetByIDProxy(ctx context.Context, id int64) (*Proxy, error) {
	if p, ok := f.proxies[id]; ok {
		return p, nil
	}
	return nil, ErrProxyNotFound
}

func (f *fakeWindowProbeRepos) key(accountID, proxyID int64) string {
	return strconv.FormatInt(accountID, 10) + ":" + strconv.FormatInt(proxyID, 10)
}

func TestProbeAccountEndToEndFullPower(t *testing.T) {
	sse := "data: {\"type\":\"response.output_text.done\",\"text\":\"高市早苗\"}\n\n"
	svc, repos, upstream := newProbeTestService(sse)

	outcome, err := svc.ProbeAccount(context.Background(), 1, nil, false)
	require.NoError(t, err)
	require.Equal(t, WindowProbeFullPower, outcome.Result)
	require.Equal(t, NodeHealthStateFullPower, outcome.State)
	require.Equal(t, "高市早苗", outcome.Answer)

	stored := repos.health[repos.key(1, 7)]
	require.NotNil(t, stored)
	require.Equal(t, NodeHealthStateFullPower, stored.State)
	// 默认走账号绑定代理
	require.Equal(t, "http://user:pass@proxy.example:8080", upstream.lastProxy)
}

func TestProbeAccountEndToEndDegradedAndGuard(t *testing.T) {
	svc, repos, upstream := newProbeTestService("")
	// 题库轮换：第一题日本首相答旧首相，第二题美国总统也答旧总统，均应判降智
	upstream.answers = map[string]string{
		DefaultWindowProbeQuestions()[0].Text: "石破茂",
		DefaultWindowProbeQuestions()[1].Text: "拜登",
	}

	outcome, err := svc.ProbeAccount(context.Background(), 1, nil, false)
	require.NoError(t, err)
	require.Equal(t, WindowProbeDegraded, outcome.Result)
	require.Equal(t, NodeHealthStateCooldown, outcome.State)

	// 探测即污染：间隔未到拒绝复探
	_, err = svc.ProbeAccount(context.Background(), 1, nil, false)
	require.ErrorIs(t, err, ErrWindowProbeTooFrequent)

	// force 绕过间隔
	outcome, err = svc.ProbeAccount(context.Background(), 1, nil, true)
	require.NoError(t, err)
	require.Equal(t, WindowProbeDegraded, outcome.Result)
	require.Equal(t, int64(2), repos.health[repos.key(1, 7)].ProbeCount)
}

func TestProbeAccountUpstreamErrorKeepsState(t *testing.T) {
	svc, repos, _ := newProbeTestService("data: {\"type\":\"response.output_text.done\",\"text\":\"高市早苗\"}\n\n")

	// 首发命中
	_, err := svc.ProbeAccount(context.Background(), 1, nil, true)
	require.NoError(t, err)

	// 上游故障（force 绕过间隔）
	svc.httpUpstream.(*fakeHTTPUpstream).statusCode = http.StatusUnauthorized
	outcome, err := svc.ProbeAccount(context.Background(), 1, nil, true)
	require.NoError(t, err)
	require.Equal(t, WindowProbeError, outcome.Result)
	require.Contains(t, outcome.Message, "401")
	// 状态结论不被错误探测改变
	require.Equal(t, NodeHealthStateFullPower, repos.health[repos.key(1, 7)].State)
}

func TestProbeAccountRejectsNonOpenAIOAuth(t *testing.T) {
	svc, _, _ := newProbeTestService("")
	svc.accountRepo.(*probeTestAccountRepo).repos.accounts[1] = &Account{
		ID:          1,
		Platform:    PlatformOpenAI,
		Type:        "apikey",
		Credentials: map[string]any{},
	}
	_, err := svc.ProbeAccount(context.Background(), 1, nil, false)
	require.ErrorContains(t, err, "OpenAI OAuth")
}

// newProbeTestService 构造被测服务：账号绑定代理，默认配置（探测间隔 120s）。
func newProbeTestService(upstreamBody string) (*WindowProbeService, *fakeWindowProbeRepos, *fakeHTTPUpstream) {
	repos := &fakeWindowProbeRepos{
		accounts: map[int64]*Account{
			1: {
				ID:       1,
				Platform: PlatformOpenAI,
				Type:     "oauth",
				Credentials: map[string]any{
					"access_token": "test-token",
					"account_id":   "chatgpt-acct",
				},
				ProxyID:     probeIntPtr(7),
				Concurrency: 1,
			},
		},
		proxies: map[int64]*Proxy{
			7: {
				ID:       7,
				Protocol: "http",
				Host:     "proxy.example",
				Port:     8080,
				Username: "user",
				Password: "pass",
				Status:   StatusActive,
			},
		},
		health: map[string]*AccountNodeHealth{},
	}
	upstream := &fakeHTTPUpstream{statusCode: http.StatusOK, body: upstreamBody}

	svc := &WindowProbeService{
		accountRepo:  &probeTestAccountRepo{repos: repos},
		proxyRepo:    &probeTestProxyRepo{repos: repos},
		healthRepo:   &probeTestHealthRepo{repos},
		httpUpstream: upstream,
	}
	return svc, repos, upstream
}

type probeTestAccountRepo struct {
	AccountRepository
	repos *fakeWindowProbeRepos
}

func (p *probeTestAccountRepo) GetByID(ctx context.Context, id int64) (*Account, error) {
	return p.repos.GetByIDAccount(ctx, id)
}

type probeTestProxyRepo struct {
	ProxyRepository
	repos *fakeWindowProbeRepos
}

func (p *probeTestProxyRepo) GetByID(ctx context.Context, id int64) (*Proxy, error) {
	return p.repos.GetByIDProxy(ctx, id)
}

type probeTestHealthRepo struct{ repos *fakeWindowProbeRepos }

func (p *probeTestHealthRepo) GetByAccountAndProxy(ctx context.Context, accountID, proxyID int64) (*AccountNodeHealth, error) {
	if h, ok := p.repos.health[p.repos.key(accountID, proxyID)]; ok {
		return h, nil
	}
	return nil, ErrNodeHealthNotFound
}

func (p *probeTestHealthRepo) Upsert(ctx context.Context, h *AccountNodeHealth) error {
	p.repos.health[p.repos.key(h.AccountID, h.ProxyID)] = h
	return nil
}

func (p *probeTestHealthRepo) List(ctx context.Context) ([]*AccountNodeHealth, error) {
	out := make([]*AccountNodeHealth, 0, len(p.repos.health))
	for _, h := range p.repos.health {
		out = append(out, h)
	}
	return out, nil
}

func (p *probeTestHealthRepo) ListByAccountIDs(ctx context.Context, accountIDs []int64) ([]*AccountNodeHealth, error) {
	return p.List(ctx)
}

func probeIntPtr(v int64) *int64 { return &v }
