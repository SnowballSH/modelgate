package server

import (
	"fmt"
	"strings"

	"github.com/SnowballSH/modelgate/internal/models"
	"github.com/SnowballSH/modelgate/internal/oai"
	"github.com/SnowballSH/modelgate/internal/translate"
)

// limitViolation names the model-table limit a request crosses, as the
// message of the 400 that refuses it before any upstream call, or returns ""
// when the request is within the model's declared limits. effort is the
// request's canonical reasoning_effort, empty when it named none or one
// outside the vocabulary.
func limitViolation(publicID string, model models.Model, req oai.ChatRequest, effort string) string {
	limits := model.Limits
	if limits.NoForcedToolChoice && req.ForcesToolCall() {
		return fmt.Sprintf(`model %s does not support forced tool_choice; use "auto"`, publicID)
	}
	if req.ReasoningEffort != "" && !limits.AllowsEffort(effort) {
		return fmt.Sprintf("model %s does not support reasoning_effort %q; use one of %s",
			publicID, req.ReasoningEffort, strings.Join(limits.ReasoningEfforts, ", "))
	}
	if model.Translated() && !model.ImageInput && translate.HasImageParts(req.Messages) {
		return fmt.Sprintf("model %s does not support image input", publicID)
	}
	return ""
}
