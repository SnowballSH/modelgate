package server

import (
	"context"
	"log/slog"

	"github.com/SnowballSH/modelgate/internal/models"
	"github.com/SnowballSH/modelgate/internal/oai"
	"github.com/SnowballSH/modelgate/internal/store"
)

// streamMeter tallies what a stream delivered so an interrupted stream can
// still be booked: a provider reports complete usage only in its final
// event, yet bills everything it generated before the cut.
type streamMeter struct {
	started     bool
	finalUsage  bool
	outputChars int
	prompt      promptSize
	// hiddenOutputCap is the output cap of a call that can bill output its
	// stream never shows, which a cut stream books in full.
	hiddenOutputCap int64
}

// mayHideOutput reports whether a call can bill output its stream never
// shows: the Responses API streams no reasoning tokens, and a call given a
// reasoning_effort may think before it answers.
func mayHideOutput(m models.Model, effort string) bool {
	return m.UpstreamAPI == models.UpstreamResponses || effort != ""
}

func (m *streamMeter) count(chunk oai.ChatChunk) {
	for _, choice := range chunk.Choices {
		m.outputChars += len(choice.Delta.Content)
		if choice.Delta.Refusal != nil {
			m.outputChars += len(*choice.Delta.Refusal)
		}
		for _, tc := range choice.Delta.ToolCalls {
			if tc.Function != nil {
				m.outputChars += len(tc.Function.Name) + len(tc.Function.Arguments)
			}
		}
	}
}

// bookedUsage is the usage a stream is booked at, and whether any of it is
// a guess: estimated when the stream was cut before its final usage,
// inputEstimated when the provider had reported no input count, which also
// decides the price tier the booking is priced at.
type bookedUsage struct {
	usage          store.Usage
	estimated      bool
	inputEstimated bool
}

// bookable is the usage to book: the reported usage once the final event
// arrived, otherwise the reported usage with output raised to a
// four-characters-per-token estimate of what streamed, or to the whole
// output cap when the call can hide output, and input estimated from the
// prompt when the provider had reported none.
func (m streamMeter) bookable(reported store.Usage) bookedUsage {
	if m.finalUsage || !m.started {
		return bookedUsage{usage: reported}
	}
	booked := bookedUsage{usage: reported, estimated: true, inputEstimated: reported.PromptTokens() == 0}
	booked.usage.OutputTokens = max(reported.OutputTokens, int64((m.outputChars+3)/4), m.hiddenOutputCap)
	if booked.inputEstimated {
		booked.usage.InputTokens = m.prompt.estimatedTokens()
	}
	return booked
}

// bookStreamUsage books what a stream billed, on every exit path, and marks
// an estimate as one in the log.
func (h *PublicHandler) bookStreamUsage(ctx context.Context, adm Admission, publicModel string, meter streamMeter, reported store.Usage) {
	booked := meter.bookable(reported)
	if !booked.usage.HasTokens() {
		return
	}
	if booked.estimated {
		slog.Warn("stream ended before its final usage; booking an estimate",
			"request_id", requestIDFrom(ctx), "key_id", adm.Key.ID, "model", publicModel,
			"input_tokens", booked.usage.InputTokens, "output_tokens", booked.usage.OutputTokens)
	}
	h.recordUsage(ctx, adm, publicModel, booked.usage, booked.inputEstimated)
}

// recordUsage runs on a context detached from the request: spend the
// provider billed must be booked even when the caller is already gone. An
// estimated prompt size cannot prove which price tier the request crossed,
// so it is booked at the dearest.
func (h *PublicHandler) recordUsage(ctx context.Context, adm Admission, publicModel string, usage store.Usage, inputEstimated bool) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bookingTimeout)
	defer cancel()
	now := h.now()
	pricing := adm.Model.PricingFor(usage.PromptTokens())
	if inputEstimated {
		pricing = adm.Model.WorstCasePricing()
	}
	if err := h.acct.Record(ctx, now, adm.Key.ID, publicModel, adm.Model.Provider, usage, pricing); err != nil {
		h.metrics.BookingFailed()
		slog.Error("usage booking failed", "request_id", requestIDFrom(ctx), "key_id", adm.Key.ID,
			"model", publicModel, "input_tokens", usage.InputTokens, "output_tokens", usage.OutputTokens, "err", err)
	}
	_ = h.store.TouchLastUsed(ctx, adm.Key.ID, now)
	h.metrics.AddTokens(publicModel, usage)
}
