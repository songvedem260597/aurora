package handler

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

type ModelsHandler struct{}

func NewModelsHandler() *ModelsHandler {
	return &ModelsHandler{}
}

// ModelCapabilities mirrors the capability object exposed by 9Router. The
// opencode-9router plugin uses these flags to enable attachments, reasoning,
// and tool calling for dynamically discovered models.
type ModelCapabilities struct {
	Vision      bool `json:"vision"`
	PDF         bool `json:"pdf"`
	AudioInput  bool `json:"audioInput"`
	VideoInput  bool `json:"videoInput"`
	ImageOutput bool `json:"imageOutput"`
	AudioOutput bool `json:"audioOutput"`
	Search      bool `json:"search"`
	Tools       bool `json:"tools"`
	Reasoning   bool `json:"reasoning"`
}

type modelDefinition struct {
	ID           string
	Name         string
	OwnedBy      string
	Capabilities ModelCapabilities
}

var supportedModels = []modelDefinition{
	newModel("auto", "Auto", true),
	newModel("gpt-5-6", "GPT-5.6", true),
	newModel("gpt-5-6-thinking", "GPT-5.6 Thinking", true),
	newModel("gpt-5-6-pro", "GPT-5.6 Pro", true),
	newModel("gpt-5-5-instant", "GPT-5.5 Instant", false),
	newModel("gpt-5-5-thinking", "GPT-5.5 Thinking", true),
	newModel("gpt-5-5-pro", "GPT-5.5 Pro", true),
	newModel("gpt-5", "GPT-5", true),
	newModel("gpt-4o", "GPT-4o", false),
	newModel("gpt-4o-mini", "GPT-4o Mini", false),
	newModel("o3", "o3", true),
	newModel("o4-mini", "o4-mini", true),
	newModel("o4-mini-high", "o4-mini-high", true),
}

func newModel(id, name string, reasoning bool) modelDefinition {
	return modelDefinition{
		ID:      id,
		Name:    name,
		OwnedBy: "openai",
		Capabilities: ModelCapabilities{
			Vision:    true,
			PDF:       true,
			Tools:     true,
			Reasoning: reasoning,
		},
	}
}

func findModel(id string) (modelDefinition, bool) {
	for _, model := range supportedModels {
		if model.ID == id {
			return model, true
		}
	}
	return modelDefinition{}, false
}

// ListModels returns the OpenAI list shape plus the capability metadata used
// by 9Router-aware OpenCode discovery plugins.
func (h *ModelsHandler) ListModels(c *gin.Context) {
	type responseModel struct {
		ID           string            `json:"id"`
		Object       string            `json:"object"`
		Created      int               `json:"created"`
		OwnedBy      string            `json:"owned_by"`
		Capabilities ModelCapabilities `json:"capabilities"`
	}

	models := make([]responseModel, 0, len(supportedModels))
	for _, model := range supportedModels {
		models = append(models, responseModel{
			ID:           model.ID,
			Object:       "model",
			Created:      1685474247,
			OwnedBy:      model.OwnedBy,
			Capabilities: model.Capabilities,
		})
	}

	c.JSON(http.StatusOK, gin.H{"object": "list", "data": models})
}

// GetModelInfo matches 9Router's /v1/models/info endpoint. This lets the
// opencode-9router plugin enrich models that are not yet present in models.dev.
func (h *ModelsHandler) GetModelInfo(c *gin.Context) {
	id := strings.TrimSpace(c.Query("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
			"message": "Missing required query param: id (e.g. ?id=gpt-5-6)",
			"type":    "invalid_request_error",
			"param":   "id",
			"code":    "missing_required_parameter",
		}})
		return
	}

	model, ok := findModel(id)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{
			"message": "Model not found: " + id,
			"type":    "not_found",
			"param":   "id",
			"code":    "model_not_found",
		}})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"id":           model.ID,
		"name":         model.Name,
		"kind":         "llm",
		"owned_by":     model.OwnedBy,
		"endpoint":     "/v1/chat/completions",
		"capabilities": model.Capabilities,
	})
}
