package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/gin-gonic/gin"
)

// GetClaudeSubscriptionPriority returns the same cached state used by scheduling.
func (h *SettingHandler) GetClaudeSubscriptionPriority(c *gin.Context) {
	settings, err := h.settingService.GetClaudeSubscriptionPriority(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, settings)
}

func (h *SettingHandler) UpdateClaudeSubscriptionPriority(c *gin.Context) {
	var input struct {
		EnabledGroupIDs *[]int64 `json:"enabled_group_ids"`
	}
	if err := c.ShouldBindJSON(&input); err != nil || input.EnabledGroupIDs == nil {
		response.BadRequest(c, "enabled_group_ids must be an explicit array of group IDs")
		return
	}
	settings, err := h.settingService.UpdateClaudeSubscriptionPriority(c.Request.Context(), *input.EnabledGroupIDs)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, settings)
}
