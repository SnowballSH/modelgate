package server

import (
	"errors"
	"fmt"
	"slices"

	"github.com/SnowballSH/modelgate/internal/accounting"
	"github.com/SnowballSH/modelgate/internal/oai"
)

// promptOverheadTokens covers what a provider adds to the prompt beyond the
// request body, such as the system text Anthropic injects for tool use.
const promptOverheadTokens = 2048

// worstCaseDemand bounds what a request can consume. Every prompt token is
// at least one byte of the body. The output bound is the largest output cap
// any upstream path could apply: the translators send max_completion_tokens
// when it is set and max_tokens otherwise, the passthrough sends both, the
// model's default applies only when the request names neither, and n
// multiplies the choices generated.
func worstCaseDemand(req oai.ChatRequest, promptBytes, defaultMaxTokens, ceiling int) (accounting.Demand, error) {
	var caps []int
	for _, field := range []struct {
		name  string
		value *int
	}{{"max_tokens", req.MaxTokens}, {"max_completion_tokens", req.MaxCompletionTokens}} {
		if field.value == nil {
			continue
		}
		if *field.value <= 0 {
			return accounting.Demand{}, fmt.Errorf("%s must be positive", field.name)
		}
		caps = append(caps, *field.value)
	}
	if len(caps) == 0 {
		caps = append(caps, defaultMaxTokens)
	}
	choices := 1
	if req.N != nil {
		if *req.N <= 0 {
			return accounting.Demand{}, errors.New("n must be positive")
		}
		choices = *req.N
	}
	perChoice := slices.Max(caps)
	if perChoice > ceiling || choices > ceiling/perChoice {
		return accounting.Demand{}, fmt.Errorf("the requested output (n times the max tokens) exceeds this gateway's ceiling of %d tokens", ceiling)
	}
	return accounting.Demand{
		InputTokens:  int64(promptBytes) + promptOverheadTokens,
		OutputTokens: int64(choices * perChoice),
	}, nil
}
