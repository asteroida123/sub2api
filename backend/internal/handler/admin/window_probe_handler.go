package admin

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// WindowProbeHandler 窗口猎手 P0：指纹探针与健康徽标 API。
type WindowProbeHandler struct {
	probeService *service.WindowProbeService
}

// NewWindowProbeHandler creates a new window probe handler.
func NewWindowProbeHandler(probeService *service.WindowProbeService) *WindowProbeHandler {
	return &WindowProbeHandler{probeService: probeService}
}

// windowProbeHealthDTO 面板徽标视图：在落库状态之上叠加时间派生态与剩余时间。
type windowProbeHealthDTO struct {
	AccountID                int64  `json:"account_id"`
	ProxyID                  int64  `json:"proxy_id"`
	Region                   string `json:"region"`
	State                    string `json:"state"`
	StoredState              string `json:"stored_state"`
	WindowOpenedAt           *int64 `json:"window_opened_at,omitempty"`
	WindowRemainingSeconds   *int64 `json:"window_remaining_seconds,omitempty"`
	DegradedAt               *int64 `json:"degraded_at,omitempty"`
	CooldownUntil            *int64 `json:"cooldown_until,omitempty"`
	CooldownRemainingSeconds *int64 `json:"cooldown_remaining_seconds,omitempty"`
	ProbeCount               int64  `json:"probe_count"`
	LastProbeAt              *int64 `json:"last_probe_at,omitempty"`
	LastProbeAnswer          string `json:"last_probe_answer"`
}

type windowProbeHealthListDTO struct {
	Items       []*windowProbeHealthDTO `json:"items"`
	GeneratedAt int64                   `json:"generated_at"`
	WindowSecs  int64                   `json:"window_duration_seconds"`
}

func toWindowProbeHealthDTO(h *service.AccountNodeHealth, now time.Time, windowDuration time.Duration) *windowProbeHealthDTO {
	if h == nil {
		return nil
	}
	state := h.EffectiveState(now, windowDuration)
	out := &windowProbeHealthDTO{
		AccountID:       h.AccountID,
		ProxyID:         h.ProxyID,
		Region:          h.Region,
		State:           state,
		StoredState:     h.State,
		WindowOpenedAt:  unixSeconds(h.WindowOpenedAt),
		DegradedAt:      unixSeconds(h.DegradedAt),
		CooldownUntil:   unixSeconds(h.CooldownUntil),
		ProbeCount:      h.ProbeCount,
		LastProbeAt:     unixSeconds(h.LastProbeAt),
		LastProbeAnswer: h.LastProbeAnswer,
	}
	if state == service.NodeHealthStateCooldown && h.CooldownUntil != nil {
		remaining := int64(h.CooldownUntil.Sub(now).Seconds())
		if remaining < 0 {
			remaining = 0
		}
		out.CooldownRemainingSeconds = &remaining
	}
	if state == service.NodeHealthStateFullPower && h.WindowOpenedAt != nil {
		remaining := int64(windowDuration.Seconds()) - int64(now.Sub(*h.WindowOpenedAt).Seconds())
		if remaining < 0 {
			remaining = 0
		}
		out.WindowRemainingSeconds = &remaining
	}
	return out
}

func unixSeconds(t *time.Time) *int64 {
	if t == nil {
		return nil
	}
	v := t.Unix()
	return &v
}

// ListHealth GET /admin/window-probe/health?account_ids=1,2,3
// 面板账号列表徽标数据源；不传 account_ids 时返回全部。
func (h *WindowProbeHandler) ListHealth(c *gin.Context) {
	if h.probeService == nil {
		response.Error(c, http.StatusServiceUnavailable, "Window probe service unavailable")
		return
	}
	ctx := c.Request.Context()

	windowSettings, err := h.probeService.GetSettings(ctx)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	var rows []*service.AccountNodeHealth
	if raw := strings.TrimSpace(c.Query("account_ids")); raw != "" {
		ids, err := parseIDList(raw)
		if err != nil {
			response.BadRequest(c, "Invalid account_ids")
			return
		}
		rows, err = h.probeService.ListHealthByAccountIDs(ctx, ids)
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
	} else {
		rows, err = h.probeService.ListHealth(ctx)
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
	}

	now := time.Now()
	items := make([]*windowProbeHealthDTO, 0, len(rows))
	for _, row := range rows {
		items = append(items, toWindowProbeHealthDTO(row, now, windowSettings.WindowDuration()))
	}
	response.Success(c, windowProbeHealthListDTO{
		Items:       items,
		GeneratedAt: now.Unix(),
		WindowSecs:  int64(windowSettings.WindowDuration().Seconds()),
	})
}

// ProbeAccountRequest POST /admin/accounts/:id/window-probe 请求体。
type ProbeAccountRequest struct {
	// ProxyID 指定探测出口；缺省用账号绑定代理，无绑定则直连。0 = 显式直连。
	ProxyID *int64 `json:"proxy_id"`
	// Force 无视最小复探间隔（探测即污染，后果自担）。
	Force bool `json:"force"`
}

// Probe POST /admin/accounts/:id/window-probe
// 手动"探测一次"按钮；同步返回三态结论。
func (h *WindowProbeHandler) Probe(c *gin.Context) {
	if h.probeService == nil {
		response.Error(c, http.StatusServiceUnavailable, "Window probe service unavailable")
		return
	}
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid account ID")
		return
	}

	var req ProbeAccountRequest
	_ = c.ShouldBindJSON(&req)

	outcome, err := h.probeService.ProbeAccount(c.Request.Context(), accountID, req.ProxyID, req.Force)
	if err != nil {
		if errors.Is(err, service.ErrWindowProbeTooFrequent) {
			c.JSON(http.StatusTooManyRequests, gin.H{
				"success": false,
				"message": err.Error(),
			})
			return
		}
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, outcome)
}

// GetSettings GET /admin/window-probe/settings
func (h *WindowProbeHandler) GetSettings(c *gin.Context) {
	if h.probeService == nil {
		response.Error(c, http.StatusServiceUnavailable, "Window probe service unavailable")
		return
	}
	settings, err := h.probeService.GetSettings(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, settings)
}

// UpdateWindowProbeSettingsRequest PUT /admin/window-probe/settings 请求体。
type UpdateWindowProbeSettingsRequest struct {
	Model                   string                        `json:"model"`
	Questions               []service.WindowProbeQuestion `json:"questions"`
	CooldownMinutes         int                           `json:"cooldown_minutes"`
	MinProbeIntervalSeconds int                           `json:"min_probe_interval_seconds"`
	WindowDurationSeconds   int                           `json:"window_duration_seconds"`
	ProbeTimeoutSeconds     int                           `json:"probe_timeout_seconds"`
}

// UpdateSettings PUT /admin/window-probe/settings
func (h *WindowProbeHandler) UpdateSettings(c *gin.Context) {
	if h.probeService == nil {
		response.Error(c, http.StatusServiceUnavailable, "Window probe service unavailable")
		return
	}
	var req UpdateWindowProbeSettingsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	settings := &service.WindowProbeSettings{
		Model:                   req.Model,
		Questions:               req.Questions,
		CooldownMinutes:         req.CooldownMinutes,
		MinProbeIntervalSeconds: req.MinProbeIntervalSeconds,
		WindowDurationSeconds:   req.WindowDurationSeconds,
		ProbeTimeoutSeconds:     req.ProbeTimeoutSeconds,
	}
	if err := h.probeService.SetSettings(c.Request.Context(), settings); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, settings)
}

func parseIDList(raw string) ([]int64, error) {
	parts := strings.Split(raw, ",")
	ids := make([]int64, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		id, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}
