package admin

import (
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// WindowSessionPoolHandler 窗口猎手 P2：满血会话池观测与操作 API。
type WindowSessionPoolHandler struct {
	sessionPoolService *service.WindowSessionPoolService
}

// NewWindowSessionPoolHandler creates a new window session pool handler.
func NewWindowSessionPoolHandler(sessionPoolService *service.WindowSessionPoolService) *WindowSessionPoolHandler {
	return &WindowSessionPoolHandler{sessionPoolService: sessionPoolService}
}

// Snapshot GET /admin/window-session-pool
func (h *WindowSessionPoolHandler) Snapshot(c *gin.Context) {
	if h.sessionPoolService == nil {
		response.Error(c, http.StatusServiceUnavailable, "Window session pool service unavailable")
		return
	}
	sessions, samples := h.sessionPoolService.Snapshot()
	response.Success(c, gin.H{
		"sessions":       sessions,
		"recent_samples": samples,
	})
}

// PrewarmRequest POST /admin/window-session-pool/prewarm 请求体。
type PrewarmRequest struct {
	AccountID int64 `json:"account_id" binding:"required"`
	// Count 预建条数；缺省用 settings.prewarm_count（默认 3，验收实验要求 ≥3）。
	Count int `json:"count"`
}

// Prewarm POST /admin/window-session-pool/prewarm
// 窗口命中后"多建"：预建 N 条满血会话存入池。
func (h *WindowSessionPoolHandler) Prewarm(c *gin.Context) {
	if h.sessionPoolService == nil {
		response.Error(c, http.StatusServiceUnavailable, "Window session pool service unavailable")
		return
	}
	var req PrewarmRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	ids, err := h.sessionPoolService.Prewarm(c.Request.Context(), req.AccountID, req.Count)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"conn_ids": ids, "count": len(ids)})
}

// SampleRequest POST /admin/window-session-pool/sample 请求体。
type SampleRequest struct {
	AccountID int64  `json:"account_id" binding:"required"`
	ConnID    string `json:"conn_id" binding:"required"`
}

// Sample POST /admin/window-session-pool/sample
// 手动对一条会话注入指纹 turn（验收实验的逐条判定工具）。
func (h *WindowSessionPoolHandler) Sample(c *gin.Context) {
	if h.sessionPoolService == nil {
		response.Error(c, http.StatusServiceUnavailable, "Window session pool service unavailable")
		return
	}
	var req SampleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	event := h.sessionPoolService.SampleConn(c.Request.Context(), req.AccountID, req.ConnID)
	if event == nil {
		response.Error(c, http.StatusInternalServerError, "sample failed")
		return
	}
	response.Success(c, event)
}

// GetSettings GET /admin/window-session-pool/settings
func (h *WindowSessionPoolHandler) GetSettings(c *gin.Context) {
	if h.sessionPoolService == nil {
		response.Error(c, http.StatusServiceUnavailable, "Window session pool service unavailable")
		return
	}
	settings, err := h.sessionPoolService.GetSettings(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, settings)
}

// UpdateSessionPoolSettingsRequest PUT /admin/window-session-pool/settings 请求体。
type UpdateSessionPoolSettingsRequest struct {
	SamplingEnabled            bool     `json:"sampling_enabled"`
	SampleIntervalSeconds      int      `json:"sample_interval_seconds"`
	SessionLifetimeMinutes     int      `json:"session_lifetime_minutes"`
	PrewarmCount               int      `json:"prewarm_count"`
	SampleFailThreshold        int      `json:"sample_fail_threshold"`
	GateModels                 []string `json:"gate_models"`
	RejectGatedWhenNoFullPower bool     `json:"reject_gated_when_no_full_power"`
}

// UpdateSettings PUT /admin/window-session-pool/settings
func (h *WindowSessionPoolHandler) UpdateSettings(c *gin.Context) {
	if h.sessionPoolService == nil {
		response.Error(c, http.StatusServiceUnavailable, "Window session pool service unavailable")
		return
	}
	var req UpdateSessionPoolSettingsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	settings := &service.WindowSessionPoolSettings{
		SamplingEnabled:            req.SamplingEnabled,
		SampleIntervalSeconds:      req.SampleIntervalSeconds,
		SessionLifetimeMinutes:     req.SessionLifetimeMinutes,
		PrewarmCount:               req.PrewarmCount,
		SampleFailThreshold:        req.SampleFailThreshold,
		GateModels:                 req.GateModels,
		RejectGatedWhenNoFullPower: req.RejectGatedWhenNoFullPower,
	}
	if err := h.sessionPoolService.SetSettings(c.Request.Context(), settings); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, settings)
}
