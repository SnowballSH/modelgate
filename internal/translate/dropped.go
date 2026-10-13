package translate

import (
	"encoding/json"

	"github.com/SnowballSH/modelgate/internal/oai"
)

const DroppedParamsHeader = "X-Modelgate-Dropped-Params"

type dropRules struct {
	sampling    bool
	unsupported bool
	stop        bool
}

func (o AnthropicOptions) Dropped(req oai.ChatRequest) []string {
	return droppedParams(req, dropRules{sampling: o.dropsSampling(req), unsupported: true})
}

func (o ResponsesOptions) Dropped(req oai.ChatRequest) []string {
	return droppedParams(req, dropRules{sampling: o.DropSampling, unsupported: true, stop: true})
}

func PassthroughDropped(req oai.ChatRequest, dropSampling bool) []string {
	return droppedParams(req, dropRules{sampling: dropSampling})
}

func droppedParams(req oai.ChatRequest, rules dropRules) []string {
	candidates := []struct {
		name    string
		dropped bool
	}{
		{"temperature", rules.sampling && req.Temperature != nil},
		{"top_p", rules.sampling && req.TopP != nil},
		{"frequency_penalty", rules.unsupported && nonZero(req.FrequencyPenalty)},
		{"presence_penalty", rules.unsupported && nonZero(req.PresencePenalty)},
		{"seed", rules.unsupported && req.Seed != nil},
		{"logprobs", rules.unsupported && req.Logprobs != nil && *req.Logprobs},
		{"top_logprobs", rules.unsupported && req.TopLogprobs != nil && *req.TopLogprobs > 0},
		{"logit_bias", rules.unsupported && namesBias(req.LogitBias)},
		{"stop", rules.stop && namesStop(req.Stop)},
	}
	var names []string
	for _, candidate := range candidates {
		if candidate.dropped {
			names = append(names, candidate.name)
		}
	}
	return names
}

func nonZero(value *float64) bool {
	return value != nil && *value != 0
}

func namesBias(raw json.RawMessage) bool {
	var bias map[string]json.RawMessage
	return json.Unmarshal(raw, &bias) == nil && len(bias) > 0
}

func namesStop(raw json.RawMessage) bool {
	stops, err := parseStop(raw)
	return err == nil && len(stops) > 0
}
