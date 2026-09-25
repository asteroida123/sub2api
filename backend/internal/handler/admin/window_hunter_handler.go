package admin

import (
	"errors"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// WindowHunterHandler 窗口猎手 P1：狩猎编排 API。
type WindowHunterHandler struct {
	hunterService *service.WindowHunterService
}

// NewWindowHunterHandler creates a new window hunter handler.
func NewWindowHunterHandler(hunterService *service.WindowHunterService) *WindowHunterHandler {
	return &WindowHunterHandler{hunterService: hunterService}
}

// Status GET /admin/window-hunter/status
func (h *WindowHunterHandler) Status(c *gin.Context) {
	if h.hunterService == nil {
		response.Error(c, http.StatusServiceUnavailable, "Window hunter service unavailable")
		return
	}
	status, err := h.hunterService.Status(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, status)
}

// RunRequest POST /admin/window-hunter/run 请求体。
type RunRequest struct {
	// AccountID 目标账号；缺省用 settings 里的 target_account_id。
	AccountID int64 `json:"account_id"`
}

// Run POST /admin/window-hunter/run
// 异步开一轮狩猎（并发 1：已有轮次在跑时返回 409）。
func (h *WindowHunterHandler) Run(c *gin.Context) {
	if h.hunterService == nil {
		response.Error(c, http.StatusServiceUnavailable, "Window hunter service unavailable")
		return
	}
	var req RunRequest
	_ = c.ShouldBindJSON(&req)

	runID, err := h.hunterService.RunRoundAsync(c.Request.Context(), req.AccountID, "manual")
	if err != nil {
		if errors.Is(err, service.ErrWindowHunterRunning) {
			c.JSON(http.StatusConflict, gin.H{
				"success": false,
				"message": err.Error(),
			})
			return
		}
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"run_id": runID})
}

// GetSettings GET /admin/window-hunter/settings
func (h *WindowHunterHandler) GetSettings(c *gin.Context) {
	if h.hunterService == nil {
		response.Error(c, http.StatusServiceUnavailable, "Window hunter service unavailable")
		return
	}
	settings, err := h.hunterService.GetSettings(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, settings)
}

// UpdateHunterSettingsRequest PUT /admin/window-hunter/settings 请求体。
type UpdateHunterSettingsRequest struct {
	AutoEnabled           bool    `json:"auto_enabled"`
	TargetAccountID       int64   `json:"target_account_id"`
	ProxyIDs              []int64 `json:"proxy_ids"`
	BaseIntervalMinutes   int     `json:"base_interval_minutes"`
	MaxIntervalMinutes    int     `json:"max_interval_minutes"`
	MaxCandidatesPerRound int     `json:"max_candidates_per_round"`
}

// UpdateSettings PUT /admin/window-hunter/settings
func (h *WindowHunterHandler) UpdateSettings(c *gin.Context) {
	if h.hunterService == nil {
		response.Error(c, http.StatusServiceUnavailable, "Window hunter service unavailable")
		return
	}
	var req UpdateHunterSettingsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	settings := &service.WindowHunterSettings{
		AutoEnabled:           req.AutoEnabled,
		TargetAccountID:       req.TargetAccountID,
		ProxyIDs:              req.ProxyIDs,
		BaseIntervalMinutes:   req.BaseIntervalMinutes,
		MaxIntervalMinutes:    req.MaxIntervalMinutes,
		MaxCandidatesPerRound: req.MaxCandidatesPerRound,
	}
	if err := h.hunterService.SetSettings(c.Request.Context(), settings); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, settings)
}
