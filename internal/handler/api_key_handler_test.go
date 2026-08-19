package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"aurora/internal/apikey"
	"aurora/internal/config"

	"github.com/gin-gonic/gin"
)

func TestAPIKeyHandlerCreateIssuesOneDayKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewAPIKeyHandler(&config.Config{Authorization: "master-secret"})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/api-keys", nil)

	h.Create(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	var body struct {
		Object    string `json:"object"`
		Key       string `json:"key"`
		CreatedAt int64  `json:"created_at"`
		ExpiresAt int64  `json:"expires_at"`
		ExpiresIn int    `json:"expires_in"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Object != "api_key" || body.Key == "" {
		t.Fatalf("unexpected response: %+v", body)
	}
	if body.ExpiresIn != 86400 || body.ExpiresAt-body.CreatedAt != 86400 {
		t.Fatalf("ttl mismatch: created=%d expires=%d expires_in=%d", body.CreatedAt, body.ExpiresAt, body.ExpiresIn)
	}
	if err := apikey.Validate("master-secret", body.Key, time.Unix(body.CreatedAt+1, 0).UTC()); err != nil {
		t.Fatalf("issued key should validate: %v", err)
	}
}
