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
	promptBytes int
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

// bookable is the usage to book: the reported usage once the final event
// arrived, otherwise the reported usage with output raised to a
// four-characters-per-token estimate of what streamed, or to the whole
// output cap when the call can hide output, and input estimated from the
// prompt when the provider had reported none.
func (m streamMeter) bookable(reported store.Usage) (store.Usage, bool) {
	if m.finalUsage || !m.started {
		return reported, false
	}
	estimate := reported
	estimate.OutputTokens = max(reported.OutputTokens, int64((m.outputChars+3)/4), m.hiddenOutputCap)
	if reported.InputTokens+reported.CacheReadTokens+reported.CacheWriteTokens == 0 {
		estimate.InputTokens = int64((m.promptBytes + 3) / 4)
	}
	return estimate, true
}

// bookStreamUsage books what a stream billed, on every exit path, and marks
// an estimate as one in the log.
func (h *PublicHandler) bookStreamUsage(ctx context.Context, adm Admission, publicModel string, meter streamMeter, reported store.Usage) {
	usage, estimated := meter.bookable(reported)
	if !usage.HasTokens() {
		return
	}
	if estimated {
		slog.Warn("stream ended before its final usage; booking an estimate",
			"request_id", requestIDFrom(ctx), "key_id", adm.Key.ID, "model", publicModel,
			"input_tokens", usage.InputTokens, "output_tokens", usage.OutputTokens)
	}
	h.recordUsage(ctx, adm, publicModel, usage)
}

// recordUsage runs on a context detached from the request: spend the
// provider billed must be booked even when the caller is already gone.
func (h *PublicHandler) recordUsage(ctx context.Context, adm Admission, publicModel string, usage store.Usage) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bookingTimeout)
	defer cancel()
	now := h.now()
	if err := h.acct.Record(ctx, now, adm.Key.ID, publicModel, usage, adm.Model.Pricing); err != nil {
		h.metrics.BookingFailed()
		slog.Error("usage booking failed", "request_id", requestIDFrom(ctx), "key_id", adm.Key.ID,
			"model", publicModel, "input_tokens", usage.InputTokens, "output_tokens", usage.OutputTokens, "err", err)
	}
	_ = h.store.TouchLastUsed(ctx, adm.Key.ID, now)
	h.metrics.AddTokens(publicModel, usage)
}
