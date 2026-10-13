package server

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/SnowballSH/modelgate/internal/accounting"
	"github.com/SnowballSH/modelgate/internal/anthro"
	"github.com/SnowballSH/modelgate/internal/oai"
	"github.com/SnowballSH/modelgate/internal/store"
	"github.com/SnowballSH/modelgate/internal/translate"
)

const (
	tinyImage     = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="
	visionRowJSON = `{"models":{"claude-vision":{"provider_model":"claude-vision","image_input":true,
		"input_usd_per_mtok":10,"output_usd_per_mtok":50,"cache_read_usd_per_mtok":0.25,"cache_write_usd_per_mtok":20},
		"gpt-vision":{"provider":"openai","upstream_api":"responses","provider_model":"gpt-vision","image_input":true,
		"input_usd_per_mtok":10,"output_usd_per_mtok":50,"cache_read_usd_per_mtok":0.25,"cache_write_usd_per_mtok":20}}}`
	inlineImagePrefix = "data:image/png;base64,"
)

func imageRequest(model string, stream bool, imageURL string) string {
	return fmt.Sprintf(`{"model":%q,"stream":%t,"max_tokens":1000,"messages":[{"role":"user","content":[{"type":"text","text":"what is this?"},{"type":"image_url","image_url":{"url":%q}}]}]}`, model, stream, imageURL)
}

func TestImageOnARowWithoutImageInputIsRefusedByName(t *testing.T) {
	for _, model := range []string{"claude-opus-5-5", "gpt-6-luna"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", model, stream), func(t *testing.T) {
				env := newDualEnvWithTable(t, limitedModelJSON, dispatchOpenAI(), fullResponseHandler(), 100)
				auth, _ := insertTestKey(t, env.store, nil)

				rec := doDual(env, auth, imageRequest(model, stream, tinyImage))
				var body oai.ErrorBody
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("decode %q: %v", rec.Body.String(), err)
				}
				want := fmt.Sprintf("model %s does not support image input", model)
				if rec.Code != http.StatusBadRequest || body.Error.Code != CodeInvalidRequest || body.Error.Message != want {
					t.Errorf("status %d, error %+v; want 400 %q", rec.Code, body.Error, want)
				}
				if *env.anthropicHits != 0 || len(*env.openaiCalls) != 0 {
					t.Errorf("upstream calls: anthropic %d, openai %v; want none", *env.anthropicHits, upstreamPaths(*env.openaiCalls))
				}
				if reserved := env.acct.Reserved(); reserved != 0 || len(env.guards.slots) != 0 {
					t.Errorf("a refused request holds reservation USD %v and %d slots, want none", reserved, len(env.guards.slots))
				}
				spend, err := env.store.MonthSpend(t.Context(), accounting.Month(env.now))
				if err != nil {
					t.Fatal(err)
				}
				if spend != 0 {
					t.Errorf("spend = %v, want nothing booked for a refused request", spend)
				}
			})
		}
	}
}

func TestPassthroughRowForwardsImagesWhateverItDeclares(t *testing.T) {
	env := newDualEnvWithTable(t, limitedModelJSON, dispatchOpenAI(), fullResponseHandler(), 100)
	auth, _ := insertTestKey(t, env.store, nil)
	rec := doDual(env, auth, imageRequest("gpt-chat", false, tinyImage))
	if rec.Code != http.StatusOK || len(*env.openaiSeen) != 1 {
		t.Fatalf("status %d, upstream requests %d; a chat_completions row forwards content parts unchanged", rec.Code, len(*env.openaiSeen))
	}
}

func TestImagesReachBothTranslatedUpstreams(t *testing.T) {
	env, capture, auth := chatReadyEnv(t)

	if rec := doDual(env, auth, imageRequest("claude-chat", false, tinyImage)); rec.Code != http.StatusOK {
		t.Fatalf("claude-chat status = %d, body %s", rec.Code, rec.Body.String())
	}
	blocks := lastAnthropicRequest(t, capture).Messages[0].Content
	if len(blocks) != 2 || blocks[1].Type != "image" || blocks[1].Source == nil || blocks[1].Source.Type != "base64" || blocks[1].Source.MediaType != "image/png" {
		t.Errorf("anthropic blocks = %+v, want text then a base64 png image", blocks)
	}

	if rec := doDual(env, auth, imageRequest("gpt-vision", false, "https://example.com/cat.png")); rec.Code != http.StatusOK {
		t.Fatalf("gpt-vision status = %d, body %s", rec.Code, rec.Body.String())
	}
	parts := lastResponsesRequest(t, env).Input[0].Content
	if len(parts) != 2 || parts[1].Type != "input_image" || parts[1].ImageURL != "https://example.com/cat.png" {
		t.Errorf("responses parts = %+v, want text then the image url", parts)
	}
}

func TestLargeInlineImageReservesAnImageNotItsBytes(t *testing.T) {
	payload := strings.Repeat("A", 15<<20)
	body := imageRequest("claude-vision", false, inlineImagePrefix+payload)

	var env *publicEnv
	var reserved float64
	var upstreamImageBytes int
	answer := fullResponseHandler()
	env = newPublicEnvWith(t, func(w http.ResponseWriter, r *http.Request) {
		reserved = env.acct.Reserved()
		var areq anthro.MessagesRequest
		if json.NewDecoder(r.Body).Decode(&areq) == nil && len(areq.Messages) == 1 && len(areq.Messages[0].Content) == 2 && areq.Messages[0].Content[1].Source != nil {
			upstreamImageBytes = len(areq.Messages[0].Content[1].Source.Data)
		}
		answer(w, r)
	}, publicEnvOptions{maxBodyBytes: 16 << 20, budgetUSD: 100, rpm: 1000, tableJSON: visionRowJSON})
	auth, _ := insertTestKey(t, env.store, nil)

	rec := doPublic(env, http.MethodPost, "/v1/chat/completions", auth, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %.200s", rec.Code, rec.Body.String())
	}
	if upstreamImageBytes != len(payload) {
		t.Errorf("upstream image payload = %d bytes, want %d", upstreamImageBytes, len(payload))
	}
	textBytes := len(body) - len(payload)
	want := (float64(textBytes+imageTokenAllowance+promptOverheadTokens)*20 + 1000*50) / 1e6
	if math.Abs(reserved-want) > 1e-9 {
		t.Errorf("reservation while the image was in flight = USD %v, want %v: an image is %d tokens, not one per byte", reserved, want, imageTokenAllowance)
	}
}

func TestRemoteImageIsChargedTheAllowanceOnBothTranslatedUpstreams(t *testing.T) {
	for _, model := range []string{"claude-vision", "gpt-vision"} {
		t.Run(model, func(t *testing.T) {
			var env *dualEnv
			var reserved float64
			answer, responses := fullResponseHandler(), openaiResponsesHandler()
			env = newDualEnvWithTable(t, visionRowJSON, func(w http.ResponseWriter, r *http.Request) {
				reserved = env.acct.Reserved()
				responses(w, r)
			}, func(w http.ResponseWriter, r *http.Request) {
				reserved = env.acct.Reserved()
				answer(w, r)
			}, 100)
			auth, _ := insertTestKey(t, env.store, nil)

			body := imageRequest(model, false, "https://example.com/cat.png")
			if rec := doDual(env, auth, body); rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
			}
			want := (float64(len(body)+imageTokenAllowance+promptOverheadTokens)*20 + 1000*50) / 1e6
			if math.Abs(reserved-want) > 1e-9 {
				t.Errorf("reservation = USD %v, want %v: a remote image costs its allowance although it adds no inline bytes", reserved, want)
			}
		})
	}
}

func TestCutStreamEstimateCountsImagesAsTokens(t *testing.T) {
	prompt := measurePrompt(15<<20+400, translate.ImageLoad{Count: 2, InlineBytes: 15 << 20})
	booked := streamMeter{started: true, prompt: prompt}.bookable(store.Usage{})
	if want := int64(100 + 2*imageTokenAllowance); !booked.inputEstimated || booked.usage.InputTokens != want {
		t.Errorf("estimate = %+v, want %d input tokens: 400 text bytes at four per token plus two images", booked, want)
	}
}

func TestCutStreamEstimateCountsARemoteImage(t *testing.T) {
	booked := streamMeter{started: true, prompt: measurePrompt(400, translate.ImageLoad{Count: 1})}.bookable(store.Usage{})
	if want := int64(100 + imageTokenAllowance); booked.usage.InputTokens != want {
		t.Errorf("input estimate = %d, want %d: 400 text bytes at four per token plus one remote image", booked.usage.InputTokens, want)
	}
}

func TestBookableDecidesWhetherInputWasEstimated(t *testing.T) {
	prompt := measurePrompt(400, translate.ImageLoad{})
	tests := []struct {
		name     string
		meter    streamMeter
		reported store.Usage
		want     bool
	}{
		{"final usage reported", streamMeter{started: true, finalUsage: true, prompt: prompt}, store.Usage{InputTokens: 5}, false},
		{"stream never started", streamMeter{prompt: prompt}, store.Usage{}, false},
		{"cut before any input count", streamMeter{started: true, prompt: prompt}, store.Usage{OutputTokens: 3}, true},
		{"cut after the provider reported input", streamMeter{started: true, prompt: prompt}, store.Usage{InputTokens: 7}, false},
		{"cut after only cache reads were reported", streamMeter{started: true, prompt: prompt}, store.Usage{CacheReadTokens: 7}, false},
	}
	for _, tc := range tests {
		if got := tc.meter.bookable(tc.reported).inputEstimated; got != tc.want {
			t.Errorf("%s: inputEstimated = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestPromptSize(t *testing.T) {
	plain := measurePrompt(1000, translate.ImageLoad{})
	if plain.worstCaseTokens() != 1000+promptOverheadTokens || plain.estimatedTokens() != 250 {
		t.Errorf("text-only prompt = %+v: worst case %d, estimate %d", plain, plain.worstCaseTokens(), plain.estimatedTokens())
	}
	remote := measurePrompt(1000, translate.ImageLoad{Count: 1})
	if remote.worstCaseTokens() != 1000+imageTokenAllowance+promptOverheadTokens {
		t.Errorf("a remote image must add its allowance to the bound, got %d", remote.worstCaseTokens())
	}
}

func TestInlineBytesNeverLeaveTheTextRemainderNegativeOrShort(t *testing.T) {
	payload := strings.Repeat("A", 4096)
	body := imageRequest("claude-vision", false, inlineImagePrefix+payload)
	var req oai.ChatRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	trueText := len(body) - len(inlineImagePrefix) - len(payload)

	areq, err := translate.ToAnthropicWith(req, translate.AnthropicOptions{ProviderModel: "m", DefaultMaxTokens: 10, ImageInput: true})
	if err != nil {
		t.Fatal(err)
	}
	anthropicText := measurePrompt(len(body), translate.AnthropicImageLoad(areq)).textBytes
	if anthropicText != trueText+len(inlineImagePrefix) {
		t.Errorf("anthropic text remainder = %d, want %d: only the base64 data is subtracted, so the data: header stays counted as text", anthropicText, trueText+len(inlineImagePrefix))
	}

	rreq, err := translate.ToResponsesWith(req, translate.ResponsesOptions{ProviderModel: "m", DefaultMaxTokens: 10, ImageInput: true})
	if err != nil {
		t.Fatal(err)
	}
	responsesText := measurePrompt(len(body), translate.ResponsesImageLoad(rreq)).textBytes
	if responsesText != trueText {
		t.Errorf("responses text remainder = %d, want %d: the whole data URI is subtracted", responsesText, trueText)
	}

	for name, text := range map[string]int{"anthropic": anthropicText, "responses": responsesText} {
		if text < trueText {
			t.Errorf("%s remainder %d under-counts the %d text bytes", name, text, trueText)
		}
	}
}
