package server

import (
	"context"

	"github.com/SnowballSH/modelgate/internal/models"
	"github.com/SnowballSH/modelgate/internal/oai"
	"github.com/SnowballSH/modelgate/internal/translate"
)

type failFunc func(code, message string)

// preparedCall is a request translated for its model's upstream, before any
// shared capacity is taken: what the translation dropped, and how to serve it
// once admitted.
type preparedCall struct {
	dropped []string
	images  translate.ImageLoad
	serve   func(ctx context.Context, call admittedCall, fail failFunc)
}

func (h *PublicHandler) prepare(req oai.ChatRequest, model models.Model, keyID string) (preparedCall, error) {
	outputCap := model.OutputCapDefault(h.defaultMaxTokens)
	cacheKey := translate.NamespacedCacheKey(keyID, req.PromptCacheKey)
	switch {
	case model.Provider == models.ProviderAnthropic:
		return h.prepareAnthropic(req, translate.AnthropicOptions{
			ProviderModel:    model.ProviderModel,
			DefaultMaxTokens: outputCap,
			DropSampling:     model.DropsSampling,
			ImageInput:       model.ImageInput,
			CacheTTL:         model.CacheTTL,
		})
	case model.UpstreamAPI == models.UpstreamResponses:
		return h.prepareResponses(req, translate.ResponsesOptions{
			ProviderModel:    model.ProviderModel,
			DefaultMaxTokens: outputCap,
			DropSampling:     model.DropsSampling,
			ImageInput:       model.ImageInput,
			PromptCacheKey:   cacheKey,
		})
	default:
		up := passthroughRequest(req, model, outputCap, cacheKey)
		return preparedCall{
			dropped: translate.PassthroughDropped(req, model.DropsSampling),
			images:  translate.ImageLoad{Count: translate.CountImageParts(req.Messages)},
			serve: func(ctx context.Context, call admittedCall, _ failFunc) {
				h.chatOpenAI(ctx, call, up)
			},
		}, nil
	}
}

func (h *PublicHandler) prepareAnthropic(req oai.ChatRequest, opts translate.AnthropicOptions) (preparedCall, error) {
	areq, err := translate.ToAnthropicWith(req, opts)
	if err != nil {
		return preparedCall{}, err
	}
	return preparedCall{
		dropped: opts.Dropped(req),
		images:  translate.AnthropicImageLoad(areq),
		serve: func(ctx context.Context, call admittedCall, _ failFunc) {
			h.chatAnthropic(ctx, call, areq)
		},
	}, nil
}

func (h *PublicHandler) prepareResponses(req oai.ChatRequest, opts translate.ResponsesOptions) (preparedCall, error) {
	rreq, err := translate.ToResponsesWith(req, opts)
	if err != nil {
		return preparedCall{}, err
	}
	return preparedCall{
		dropped: opts.Dropped(req),
		images:  translate.ResponsesImageLoad(rreq),
		serve: func(ctx context.Context, call admittedCall, fail failFunc) {
			h.chatResponses(ctx, call, rreq, fail)
		},
	}, nil
}

// passthroughRequest forwards the request nearly verbatim: the public wire
// format is already OpenAI's, so only the model name, the default output cap,
// the cache key's namespace and the row's sampling rule need modelgate's hand.
func passthroughRequest(req oai.ChatRequest, model models.Model, outputCap int, cacheKey string) oai.ChatRequest {
	up := req
	up.Model = model.ProviderModel
	up.PromptCacheKey = cacheKey
	if up.MaxTokens == nil && up.MaxCompletionTokens == nil {
		up.MaxCompletionTokens = &outputCap
	}
	if model.DropsSampling {
		up.Temperature, up.TopP = nil, nil
	}
	return up
}
