package middlewares

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"aurora/internal/apikey"

	"github.com/gin-gonic/gin"
)

func TestAuthorizationAcceptsTemporaryKeyAndRewritesForDownstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const master = "test-master-key"
	t.Setenv("Authorization", master)
	key, _, err := apikey.Issue(master, time.Now().UTC(), apikey.DefaultTTL)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	r := gin.New()
	r.Use(Authorization)
	r.GET("/protected", func(c *gin.Context) {
		if got := c.GetHeader("Authorization"); got != "Bearer "+master {
			t.Fatalf("downstream Authorization = %q", got)
		}
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
}

func TestAuthorizationRejectsExpiredTemporaryKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const master = "test-master-key"
	t.Setenv("Authorization", master)
	key, _, err := apikey.Issue(master, time.Now().UTC().Add(-25*time.Hour), apikey.DefaultTTL)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	r := gin.New()
	r.Use(Authorization)
	r.GET("/protected", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %s", w.Code, w.Body.String())
	}
}

func TestAdminAuthorizationRejectsTemporaryKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const master = "test-master-key"
	t.Setenv("Authorization", master)
	key, _, err := apikey.Issue(master, time.Now().UTC(), apikey.DefaultTTL)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	r := gin.New()
	r.POST("/v1/api-keys", AdminAuthorization, func(c *gin.Context) { c.Status(http.StatusNoContent) })
	req := httptest.NewRequest(http.MethodPost, "/v1/api-keys", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestAdminAuthorizationAcceptsMasterKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const master = "test-master-key"
	t.Setenv("Authorization", master)

	r := gin.New()
	r.POST("/v1/api-keys", AdminAuthorization, func(c *gin.Context) { c.Status(http.StatusNoContent) })
	req := httptest.NewRequest(http.MethodPost, "/v1/api-keys", nil)
	req.Header.Set("Authorization", "Bearer "+master)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
}
