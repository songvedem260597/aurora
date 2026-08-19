package middlewares

import (
	"crypto/subtle"
	"net/http"
	"os"
	"strings"
	"time"

	"aurora/internal/apikey"

	"github.com/gin-gonic/gin"
)

func Authorization(c *gin.Context) {
	masterKey := strings.TrimSpace(os.Getenv("Authorization"))
	if masterKey == "" {
		c.Next()
		return
	}

	token, trailingToken, ok := bearerParts(c.GetHeader("Authorization"))
	if !ok {
		unauthorized(c, "Invalid or missing API key")
		return
	}

	if secureEqual(masterKey, token) {
		// Preserve the legacy admin form: Bearer <master-key> <upstream-token>.
		if trailingToken != "" {
			c.Request.Header.Set("Authorization", "Bearer "+trailingToken)
		}
		c.Next()
		return
	}

	if err := apikey.Validate(masterKey, token, time.Now().UTC()); err == nil {
		// Downstream account resolution must see the configured gateway key, not
		// the short-lived client key. This prevents temporary keys from being
		// mistaken for upstream access tokens.
		c.Request.Header.Set("Authorization", "Bearer "+masterKey)
		c.Next()
		return
	} else if err == apikey.ErrExpired {
		unauthorized(c, "API key expired")
		return
	}

	unauthorized(c, "Invalid or missing API key")
}

// AdminAuthorization only accepts the configured master key. Short-lived API
// keys can call the normal API but cannot mint more keys.
func AdminAuthorization(c *gin.Context) {
	masterKey := strings.TrimSpace(os.Getenv("Authorization"))
	if masterKey == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{
			"message": "API key administration is not configured",
			"type":    "server_error",
			"code":    "api_key_admin_unavailable",
		}})
		c.Abort()
		return
	}

	token, _, ok := bearerParts(c.GetHeader("Authorization"))
	if !ok || !secureEqual(masterKey, token) {
		unauthorized(c, "Master API key required")
		return
	}
	c.Next()
}

func bearerParts(header string) (token, trailing string, ok bool) {
	header = strings.TrimSpace(header)
	if len(header) < len("Bearer ") || !strings.EqualFold(header[:len("Bearer ")], "Bearer ") {
		return "", "", false
	}
	parts := strings.Fields(strings.TrimSpace(header[len("Bearer "):]))
	if len(parts) == 0 {
		return "", "", false
	}
	token = parts[0]
	if len(parts) > 1 {
		trailing = parts[1]
	}
	return token, trailing, token != ""
}

func secureEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func unauthorized(c *gin.Context, message string) {
	c.JSON(http.StatusUnauthorized, gin.H{"error": gin.H{
		"message": message,
		"type":    "authentication_error",
		"code":    "invalid_api_key",
	}})
	c.Abort()
}
