package server

import (
	"errors"
	"fmt"
	"slices"

	"github.com/SnowballSH/modelgate/internal/accounting"
	"github.com/SnowballSH/modelgate/internal/models"
	"github.com/SnowballSH/modelgate/internal/oai"
	"github.com/SnowballSH/modelgate/internal/translate"
)

// promptOverheadTokens covers what a provider adds to the prompt beyond the
// request body, such as the system text Anthropic injects for tool use.
const promptOverheadTokens = 2048

// Documented per-image maxima: Anthropic's 4,784 visual tokens for Claude 4.7
// and later, and OpenAI's 30,000 patches at a 1.2 multiplier.
const (
	anthropicImageTokenAllowance = 4_784
	openAIImageTokenAllowance    = 36_000
)

func imageAllowance(model models.Model) int64 {
	if model.Provider == models.ProviderAnthropic {
		return anthropicImageTokenAllowance
	}
	return openAIImageTokenAllowance
}

// promptSize sizes a prompt from its body. maxTokens, when positive, bounds
// both figures: a provider rejects, and does not bill, a longer prompt.
type promptSize struct {
	textBytes   int
	imageTokens int64
	maxTokens   int64
}

func measurePrompt(bodyBytes int, images translate.ImageLoad, perImageTokens int64) promptSize {
	return promptSize{
		textBytes:   max(0, bodyBytes-images.InlineBytes),
		imageTokens: int64(images.Count) * perImageTokens,
	}
}

func (p promptSize) cappedAt(maxTokens int64) promptSize {
	p.maxTokens = maxTokens
	return p
}

func (p promptSize) worstCaseTokens() int64 {
	return p.capped(int64(p.textBytes) + p.imageTokens + promptOverheadTokens)
}

func (p promptSize) estimatedTokens() int64 {
	return p.capped(int64((p.textBytes+3)/4) + p.imageTokens)
}

func (p promptSize) capped(tokens int64) int64 {
	if p.maxTokens > 0 {
		return min(tokens, p.maxTokens)
	}
	return tokens
}

// worstCaseDemand bounds what a request can consume. Every prompt token is
// at least one byte of the body's text, an image is its allowance, and no
// prompt exceeds the model's max_input_tokens. The output bound is the
// largest output cap any upstream path could apply: the translators send
// max_completion_tokens when it is set and max_tokens otherwise, the
// passthrough sends both, the model's default applies only when the
// request names neither, and n multiplies the choices generated.
func worstCaseDemand(req oai.ChatRequest, prompt promptSize, defaultMaxTokens, ceiling int) (accounting.Demand, error) {
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
		InputTokens:  prompt.worstCaseTokens(),
		OutputTokens: int64(choices * perChoice),
	}, nil
}
