package admin

import (
	"net/http"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// ClearGlobalRateLimitIfUnchanged is deliberately separate from the broad
// operator clear action. A stale or incomplete expectation never mutates state.
func (h *AccountHandler) ClearGlobalRateLimitIfUnchanged(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || accountID <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	var expected service.GlobalRateLimitClearExpectation
	if err := c.ShouldBindJSON(&expected); err != nil || !expected.Valid() {
		response.BadRequest(c, "Complete account generation and token fingerprint required")
		return
	}
	cleared, err := h.rateLimitService.ClearGlobalRateLimitIfUnchanged(c.Request.Context(), accountID, expected)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if !cleared {
		response.Error(c, http.StatusConflict, "Account generation changed; verify again")
		return
	}
	response.Success(c, gin.H{"cleared": true})
}
