package handler

import (
	"net/http"
	"strings"
	"time"

	"aurora/internal/apikey"
	"aurora/internal/config"

	"github.com/gin-gonic/gin"
)

type APIKeyHandler struct {
	cfg *config.Config
}

func NewAPIKeyHandler(cfg *config.Config) *APIKeyHandler {
	return &APIKeyHandler{cfg: cfg}
}

func (h *APIKeyHandler) Create(c *gin.Context) {
	if h == nil || h.cfg == nil || strings.TrimSpace(h.cfg.Authorization) == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{
			"message": "API key issuance is not configured",
			"type":    "server_error",
			"code":    "api_key_issuance_unavailable",
		}})
		return
	}

	now := time.Now().UTC()
	key, expiresAt, err := apikey.Issue(h.cfg.Authorization, now, apikey.DefaultTTL)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{
			"message": "Failed to issue API key",
			"type":    "server_error",
			"code":    "api_key_issue_failed",
		}})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"object":     "api_key",
		"key":        key,
		"created_at": now.Unix(),
		"expires_at": expiresAt.Unix(),
		"expires_in": int(apikey.DefaultTTL.Seconds()),
	})
}
